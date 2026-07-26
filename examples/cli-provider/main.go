// Command cli-provider runs agentkit against a command-line coding agent
// instead of an HTTP API — no API key, no separate billing, just whatever CLI
// your subscription already authenticates.
//
//	go run ./examples/cli-provider           # dry run: prints what would be sent
//	go run ./examples/cli-provider -live     # actually invokes the CLI
//	go run ./examples/cli-provider -live -cli codex
//
// Dry run is the default on purpose: a live run spends real subscription quota
// and takes seconds per turn. Dry run still exercises the whole pipeline — the
// stages all execute, and only the final Send is intercepted — so it shows
// exactly which turns would have reached the CLI and what prompt each would
// have carried.
//
// # Why this matters with a CLI backend
//
// A CLI gives you no cache_control hooks, so CacheAlignStage cannot help the
// way it does against the raw API. The savings come from somewhere else, and
// they are larger, not smaller: every turn a preprocess rule answers is an
// entire CLI process that never starts. Against an API that saves tokens;
// against a CLI it saves whole seconds.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"agentkit"
	"agentkit/config"
	"agentkit/metrics"
	"agentkit/pipeline"
	"agentkit/preprocess"
	"agentkit/providers/cli"
	"agentkit/router"
	"agentkit/toolcache"
)

func main() {
	live := flag.Bool("live", false, "actually invoke the CLI (spends subscription quota)")
	which := flag.String("cli", "claude", "which CLI preset to use: claude | codex | opencode")
	model := flag.String("model", "", "model override, for the opencode preset")
	flag.Parse()

	cfg, err := preset(*which, *model)
	if err != nil {
		log.Fatal(err)
	}

	if _, err := exec.LookPath(cfg.Command); err != nil {
		fmt.Printf("%q is not on PATH — nothing to demonstrate.\n", cfg.Command)
		fmt.Println("Install it, or pass -cli with one you have (claude | codex | opencode).")
		return // not a failure: this example is meant to be safe to run anywhere
	}

	client, err := cli.New(cfg)
	if err != nil {
		log.Fatalf("cli.New: %v", err)
	}

	// In dry-run mode the CLI client is wrapped rather than replaced, so the
	// exact prompt the real client would have sent is what gets printed.
	var backend pipeline.ModelClient = client
	dry := &dryRun{inner: client}
	if !*live {
		backend = dry
	}

	rec := metrics.NewInMemory()
	var toolExecs atomic.Int64

	kit := agentkit.New(backend,
		config.WithMetrics(rec),
		// The big win with a CLI backend: these turns never spawn a process.
		config.WithPreprocess(shaRule{}, arithmeticRule{}),
		config.WithToolCache(toolcache.NewMemoryCache(), func(_ context.Context, c pipeline.ToolCall) (any, error) {
			toolExecs.Add(1)
			return "exit 0: 178 passed", nil
		}, time.Hour),
		config.WithDefaultCompression(),
		config.WithCacheAlignment(),
		// One tier here, but the router is what would split traffic between,
		// say, a local opencode model and the cloud-backed claude CLI.
		config.WithRouter(router.NewHeuristicRouter(router.Tier{Client: backend, MaxComplexity: 1.0})),
	)

	fmt.Printf("backend: %s   mode: %s\n", client.Name(), modeLabel(*live))
	fmt.Println(strings.Repeat("─", 66))

	history := []pipeline.Message{
		{Role: "system", Content: "You are a terse assistant. Answer in one short line.", Static: true},
	}
	start := time.Now()

	for i, t := range turns() {
		req := &pipeline.Request{Query: t.query, Messages: history, ToolCalls: t.toolCalls}

		turnStart := time.Now()
		resp, err := kit.Run(context.Background(), req)
		if err != nil {
			log.Fatalf("turn %d: %v", i+1, err)
		}

		fmt.Printf("\n%d. %s\n", i+1, t.label)
		if resp.ShortCircuited {
			fmt.Printf("   ✂  %v — no process spawned (%v)\n",
				req.Metadata[preprocess.MetaMatchedRule], time.Since(turnStart).Round(time.Microsecond))
			fmt.Printf("   →  %s\n", resp.Content)
		} else {
			fmt.Printf("   ↗  sent to %s (%v)\n", client.Name(), time.Since(turnStart).Round(time.Millisecond))
			if *live {
				fmt.Printf("   →  %s\n", firstLine(resp.Content))
				fmt.Printf("   usage: in=%d out=%d cache_read=%d\n",
					resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheReadTokens)
			}
		}

		history = append(history,
			pipeline.Message{Role: "user", Content: t.query},
			pipeline.Message{Role: "assistant", Content: resp.Content},
		)
	}

	total := len(turns())
	short := rec.Count(preprocess.CounterShortCircuit)
	fmt.Println("\n" + strings.Repeat("─", 66))
	fmt.Printf("%d turns in %v\n", total, time.Since(start).Round(time.Millisecond))
	fmt.Printf("  CLI invocations : %d of %d (%d short-circuited before launch)\n", total-short, total, short)
	fmt.Printf("  tool executions : %d of %d calls issued\n", toolExecs.Load(), issuedToolCalls())
	fmt.Printf("  metrics         : %v\n", rec.Snapshot())

	if !*live {
		fmt.Printf("\nPrompt that turn 1 would have sent (%d bytes):\n%s\n",
			len(dry.firstPrompt), indent(dry.firstPrompt))
		fmt.Println("\nRe-run with -live to actually invoke the CLI.")
	}

	// A CLI turn costs seconds; the assertion is that we avoided several.
	if short == 0 {
		log.Fatal("expected the preprocess rules to short-circuit some turns")
	}
}

// dryRun stands in for the real client and records what it was asked to send.
// It wraps rather than replaces the CLI client so the prompt shown is rendered
// by exactly the same code path a live run uses.
type dryRun struct {
	inner       *cli.Client
	firstPrompt string
}

func (d *dryRun) Name() string { return d.inner.Name() }

func (d *dryRun) Send(_ context.Context, req *pipeline.Request) (*pipeline.Response, error) {
	if d.firstPrompt == "" {
		d.firstPrompt = cli.RenderPrompt(req)
	}
	return &pipeline.Response{Content: "(dry run — no CLI invoked)", ModelUsed: d.inner.Name()}, nil
}

func preset(which, model string) (cli.Config, error) {
	switch which {
	case "claude":
		return cli.ClaudePreset(""), nil
	case "codex":
		return cli.CodexPreset(""), nil
	case "opencode":
		return cli.OpenCodePreset("", model), nil
	default:
		return cli.Config{}, fmt.Errorf("unknown -cli %q: want claude, codex, or opencode", which)
	}
}

type turnSpec struct {
	label     string
	query     string
	toolCalls []pipeline.ToolCall
}

func turns() []turnSpec {
	runTests := pipeline.ToolCall{Name: "run_tests", Args: map[string]any{"pkg": "./...", "race": true}}

	return []turnSpec{
		{label: "real question", query: "In one line: what is a prompt cache breakpoint?"},
		{label: "deterministic — SHA validation", query: "validate sha 3f2a1c9d4e5b6a7c8d9e0f1a2b3c4d5e6f7a8b9c"},
		{label: "deterministic — arithmetic", query: "compute 1287 * 43"},
		// The tool has already been executed by agentkit — the result rides
		// along in the prompt's <tool_results> block. The query says so
		// explicitly, because a CLI agent has its own tools and will happily
		// redo the work if you leave the door open.
		{label: "tool call, cold", query: "Test results are provided below. In one line, state pass/fail. Do not run anything.", toolCalls: []pipeline.ToolCall{runTests}},
		{label: "same tool call — served from cache", query: "Same results below. In one line, has anything changed? Do not run anything.", toolCalls: []pipeline.ToolCall{runTests}},
		{label: "deterministic — bad SHA", query: "validate sha notasha"},
		{label: "real question", query: "In one line: why keep a prompt prefix stable?"},
	}
}

func issuedToolCalls() int {
	n := 0
	for _, t := range turns() {
		n += len(t.toolCalls)
	}
	return n
}

// shaRule and arithmeticRule are the kind of work that should never reach a
// model — and with a CLI backend, never start a process.
type shaRule struct{}

func (shaRule) Name() string { return "git_sha_validator" }

func (shaRule) CanHandle(req *pipeline.Request) bool {
	return strings.HasPrefix(strings.ToLower(req.Query), "validate sha ")
}

func (shaRule) Handle(req *pipeline.Request) (*pipeline.Response, error) {
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

type arithmeticRule struct{}

func (arithmeticRule) Name() string { return "arithmetic" }

func (arithmeticRule) CanHandle(req *pipeline.Request) bool {
	var a, b int
	_, err := fmt.Sscanf(strings.TrimSpace(req.Query), "compute %d * %d", &a, &b)
	return err == nil
}

func (arithmeticRule) Handle(req *pipeline.Request) (*pipeline.Response, error) {
	var a, b int
	if _, err := fmt.Sscanf(strings.TrimSpace(req.Query), "compute %d * %d", &a, &b); err != nil {
		return nil, err
	}
	return &pipeline.Response{Content: fmt.Sprint(a * b), ModelUsed: "none"}, nil
}

func modeLabel(live bool) string {
	if live {
		return "live (spending quota)"
	}
	return "dry run"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString("   │ ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
