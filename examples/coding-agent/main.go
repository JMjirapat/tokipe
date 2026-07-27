// Command coding-agent is the most complete example: a long-running,
// multi-turn agent loop with heavy tool use and a growing context, wiring
// every stage together — preprocess, toolcache, rag, compress, lazyload,
// cache alignment, routing, and budget.
//
// Everything runs against mocks; no API key, no network, no database.
//
// Run: go run ./examples/coding-agent
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JMjirapat/tokipe"
	"github.com/JMjirapat/tokipe/budget"
	"github.com/JMjirapat/tokipe/config"
	"github.com/JMjirapat/tokipe/lazyload"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/preprocess"
	"github.com/JMjirapat/tokipe/providers/mock"
	"github.com/JMjirapat/tokipe/router"
	storemock "github.com/JMjirapat/tokipe/stores/mock"
	"github.com/JMjirapat/tokipe/toolcache"
)

const systemPrompt = `You are a coding agent working in a Go repository.
Use the provided tools. Prefer reading a file over guessing its contents.`

func main() {
	ctx := context.Background()
	rec := metrics.NewInMemory()

	// ── tools ──────────────────────────────────────────────────────────────
	// The agent's real tool executor. tokipe never runs tools itself; it
	// only decides when a call can be answered from cache instead.
	executions := 0
	exec := func(_ context.Context, call pipeline.ToolCall) (any, error) {
		executions++
		switch call.Name {
		case "list_tests":
			return []string{"TestAlign", "TestCompress", "TestRoute"}, nil
		case "run_tests":
			return map[string]any{"passed": 178, "failed": 0, "elapsed_ms": 4210}, nil
		default:
			return nil, fmt.Errorf("unknown tool %q", call.Name)
		}
	}

	// ── lazy loading ───────────────────────────────────────────────────────
	// Opt-in and deliberately NOT a pipeline stage: exposing it changes the
	// tool surface the model sees, so the caller owns that decision.
	root := mustTempRepo()
	defer os.RemoveAll(root)
	loader, err := lazyload.NewFileLoader(root)
	if err != nil {
		log.Fatalf("file loader: %v", err)
	}

	// ── retrieval ──────────────────────────────────────────────────────────
	store := storemock.NewStore()
	store.Add(storemock.DefaultDim,
		pipeline.Chunk{SourceURL: "docs/align.md", Content: "   Cache   alignment   places breakpoints only after static content.\n\n\n"},
		pipeline.Chunk{SourceURL: "docs/router.md", Content: `{ "router": "heuristic", "weights": {"length":0.5,"code":0.3,"chunks":0.2}, "_debug": "ignore-me" }`},
		pipeline.Chunk{SourceURL: "docs/tools.md", Content: "Tool results   are   cached by a  hash of (name, args)."},
	)

	// ── models ─────────────────────────────────────────────────────────────
	cheap := mock.New("local-8b", "done")
	strong := mock.New("cloud-frontier", "done")

	kit := tokipe.New(strong,
		config.WithMetrics(rec),
		config.WithPreprocess(gitShaRule{}),
		config.WithToolCache(toolcache.NewMemoryCache(), exec, 10*time.Minute),
		config.WithRAG(storemock.NewEmbedder(storemock.DefaultDim), store, 2),
		config.WithDefaultCompression(),
		config.WithCacheAlignment(),
		config.WithRouter(router.NewHeuristicRouter(
			router.Tier{Client: cheap, MaxComplexity: 0.35},
			router.Tier{Client: strong, MaxComplexity: 1.0},
		)),
	)

	// ── the loop ───────────────────────────────────────────────────────────
	policy := budget.DefaultPolicy()
	history := []pipeline.Message{{Role: "system", Content: systemPrompt, Static: true}}

	for i, turn := range turns(loader) {
		req := turn.build(history)
		req.TurnType = budget.Classify(req) // caller's job, per spec §2.4.8

		resp, err := kit.Run(ctx, req)
		if err != nil {
			log.Fatalf("turn %d: %v", i+1, err)
		}

		fmt.Printf("\n── turn %d ─ %s\n", i+1, turn.label)
		fmt.Printf("   turn type    : %v (budget %d tokens)\n", req.TurnType, policy.BudgetFor(req.TurnType))
		if rule, ok := req.Metadata[preprocess.MetaMatchedRule]; ok {
			fmt.Printf("   short-circuit: %v — no LLM call made\n", rule)
		} else {
			fmt.Printf("   model        : %v\n", req.Metadata["router.client"])
			fmt.Printf("   chunks       : %d, breakpoints: %v\n", len(req.RetrievedChunks), req.CacheBreakpoints)
		}
		if results, ok := req.Metadata[toolcache.MetaResults]; ok {
			fmt.Printf("   tool results : %d resolved\n", len(results.(map[string]any)))
		}
		fmt.Printf("   answer       : %s (short-circuited=%v)\n", resp.Content, resp.ShortCircuited)

		history = append(history,
			pipeline.Message{Role: "user", Content: req.Query},
			pipeline.Message{Role: "assistant", Content: resp.Content},
		)
	}

	// Four tool calls were issued across turns 3 and 4, but only the two
	// distinct ones ever ran: the repeat turn was served entirely from cache.
	fmt.Printf("\ntool executions: %d real, out of 4 calls issued\n", executions)
	fmt.Printf("metrics: %v\n", rec.Snapshot())
	if executions != 2 {
		log.Fatalf("expected 2 real tool executions, got %d", executions)
	}
	fmt.Println("\nOK — all stages wired, tools deduplicated, prefix kept cacheable.")
}

type turn struct {
	label string
	build func(history []pipeline.Message) *pipeline.Request
}

func turns(loader lazyload.Loader) []turn {
	toolCalls := []pipeline.ToolCall{
		{Name: "list_tests", Args: map[string]any{"pkg": "./..."}},
		{Name: "run_tests", Args: map[string]any{"pkg": "./...", "race": true}},
	}

	return []turn{
		{"new question, needs retrieval", func(h []pipeline.Message) *pipeline.Request {
			return &pipeline.Request{
				Query:          "Where are cache breakpoints allowed to go?",
				Messages:       h,
				NeedsRetrieval: true,
			}
		}},
		{"deterministic — never reaches the LLM", func(h []pipeline.Message) *pipeline.Request {
			return &pipeline.Request{Query: "validate sha 3f2a1c9d4e5b6a7c8d9e0f1a2b3c4d5e6f7a8b9c", Messages: h}
		}},
		{"tool call — cold, executes for real", func(h []pipeline.Message) *pipeline.Request {
			return &pipeline.Request{Query: "Run the tests.", Messages: h, ToolCalls: toolCalls}
		}},
		{"same tool call — served from cache", func(h []pipeline.Message) *pipeline.Request {
			return &pipeline.Request{Query: "Run the tests again to be sure.", Messages: h, ToolCalls: toolCalls}
		}},
		{"lazy-loaded file, resolved by the caller", func(h []pipeline.Message) *pipeline.Request {
			body, err := loader.Resolve(context.Background(), lazyload.Ref{ID: "main.go", Kind: lazyload.KindFile})
			if err != nil {
				log.Fatalf("lazyload: %v", err)
			}
			return &pipeline.Request{
				Query:    "Explain this file:\n```go\n" + body + "```",
				Messages: h,
			}
		}},
		{"error recovery", func(h []pipeline.Message) *pipeline.Request {
			return &pipeline.Request{
				Query:    "That failed. Fix it.",
				Messages: append(h, pipeline.Message{Role: "user", Content: "error: build failed, undefined: foo"}),
			}
		}},
	}
}

// gitShaRule answers "is this a well-formed git SHA" without an LLM — the
// cheapest possible turn. Deterministic checks like this are exactly what
// preprocess rules are for (spec §1.5 use case 4).
type gitShaRule struct{}

func (gitShaRule) Name() string { return "git_sha_validator" }

func (gitShaRule) CanHandle(req *pipeline.Request) bool {
	return strings.HasPrefix(strings.ToLower(req.Query), "validate sha ")
}

func (gitShaRule) Handle(req *pipeline.Request) (*pipeline.Response, error) {
	sha := strings.TrimSpace(req.Query[len("validate sha "):])
	valid := len(sha) == 40 && strings.IndexFunc(sha, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdef", r)
	}) < 0
	out, err := json.Marshal(map[string]any{"sha": sha, "valid": valid})
	if err != nil {
		return nil, err
	}
	return &pipeline.Response{Content: string(out), ModelUsed: "none"}, nil
}

func mustTempRepo() string {
	dir, err := os.MkdirTemp("", "tokipe-coding-agent-*")
	if err != nil {
		log.Fatalf("temp dir: %v", err)
	}
	src := "package main\n\nfunc main() {\n\tprintln(\"hello from the sandboxed repo\")\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		log.Fatalf("write: %v", err)
	}
	return dir
}
