package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/JMjirapat/tokipe/pipeline"
)

const systemPrompt = `You are a coding agent operating inside a Go monorepo.

Tools available to you:
  - list_tests(pkg)          list the tests in a package
  - run_tests(pkg, race)     run a package's tests
  - read_file(path)          read a file from the repository
  - search(query, limit)     search the codebase

Rules:
  - Prefer reading a file over guessing its contents.
  - Cite the source path for every claim you make about the codebase.
  - Never invent a test name; list them first.
  - When a command fails, report the exact error before proposing a fix.
  - Keep patches minimal and explain the tradeoffs you considered.`

// corpus is the retrievable documentation. Contents are padded and verbose on
// purpose — that is what the compressors exist to shrink.
var corpus = []pipeline.Chunk{
	{
		SourceURL: "docs/caching.md",
		Content: "Prompt   caching    anchors   a  breakpoint\n\n\n   after   static   content   only.\n\n" +
			"   Dynamic   content   such   as   retrieved   chunks   must   never   sit   before   a   breakpoint,\n\n\n" +
			"   because   it   changes   every   turn   and   invalidates   the   cached   prefix.   ",
	},
	{
		SourceURL: "docs/router.json",
		Content: `{
        "router"    : "heuristic",
        "weights"   : { "length": 0.5, "code": 0.3, "chunks": 0.2 },
        "saturation": { "length_chars": 8000, "chunks": 10, "fences": 2 },
        "_debug_trace_id"     : "trace-9f81-aa02-not-useful-to-the-model",
        "_internal_span"      : "span:4471/parent:0091",
        "_generated_at"       : "2026-07-26T03:00:00Z"
      }`,
	},
	{
		SourceURL: "docs/toolcache.md",
		Content: "Tool   results   are   keyed   by   a   SHA-256   hash   of   the   tool   name   and   its   arguments.\n\n\n" +
			"   Argument   maps   are   canonicalised   first,   so   key   order   never   changes   the   hash.   ",
	},
	{
		SourceURL: "docs/budget.json",
		Content: `{
        "routine_step_budget"  : 4000,
        "new_question_budget"  : 12000,
        "error_recovery_budget": 20000,
        "_notes"               : "starting points, not tuned constants",
        "_owner_email"         : "nobody@example.com",
        "_ticket"              : "PLAT-4471"
      }`,
	},
	{
		SourceURL: "docs/failopen.md",
		Content: "Every    optimization    fails    open.\n\n\n   A   compression   error,   a   dead   cache   backend,   or   an\n\n" +
			"   embedding   timeout   leaves   the   turn   intact   and   simply   forfeits   that   optimization.   ",
	},
	{
		SourceURL: "docs/lazyload.md",
		Content: "Lazy   loading   is   opt-in   because   it   changes   the   tool   surface   the   model   sees.\n\n\n" +
			"   The   caller   must   expose   a   load_content   tool   for   references   to   be   resolvable.   ",
	},
}

type turn struct {
	query          string
	needsRetrieval bool
	toolCalls      []pipeline.ToolCall
}

var (
	listTests = pipeline.ToolCall{Name: "list_tests", Args: map[string]any{"pkg": "./cache/..."}}
	runTests  = pipeline.ToolCall{Name: "run_tests", Args: map[string]any{"pkg": "./...", "race": true}}
	readFile  = pipeline.ToolCall{Name: "read_file", Args: map[string]any{"path": "cache/breakpoint.go"}}
	search    = pipeline.ToolCall{Name: "search", Args: map[string]any{"query": "computeBreakpoints", "limit": 20}}
)

// workload is one realistic agent session: it grows, it repeats itself, and it
// contains work that never needed a model in the first place.
func workload() []turn {
	return []turn{
		{query: "Where can cache breakpoints be placed?", needsRetrieval: true},
		{query: "validate sha 3f2a1c9d4e5b6a7c8d9e0f1a2b3c4d5e6f7a8b9c"},
		{query: "List the tests in the cache package.", toolCalls: []pipeline.ToolCall{listTests}},
		{query: "Run the full suite with the race detector.", toolCalls: []pipeline.ToolCall{runTests}},
		{query: "How are tool results keyed?", needsRetrieval: true},
		{query: "Show me where breakpoints are computed.", toolCalls: []pipeline.ToolCall{readFile, search}},
		{query: "validate sha 0000000000000000000000000000000000000000"},
		{query: "Re-run the suite; I want to be sure nothing regressed.", toolCalls: []pipeline.ToolCall{runTests}},
		{query: "What happens when the embedding service is down?", needsRetrieval: true},
		{query: "Check the tests in the cache package again.", toolCalls: []pipeline.ToolCall{listTests}},
		{query: "validate sha notasha"},
		{query: "Summarise everything you have established so far.", needsRetrieval: true},
	}
}

// toolOutput is the deterministic stand-in for real tool execution. The
// outputs are verbose, as real tool output tends to be.
func toolOutput(call pipeline.ToolCall) (any, error) {
	switch call.Name {
	case "list_tests":
		return strings.Join([]string{
			"TestComputeBreakpointsDeterministic", "TestNoBreakpointInDynamicSegment",
			"TestStablePrefixAcrossTurns", "TestReorderingIsStable",
			"TestEmptyRequestEmitsNothing", "TestSpecsWithoutStaticAnchorIgnored",
		}, "\n"), nil
	case "run_tests":
		return map[string]any{
			"passed": 178, "failed": 0, "skipped": 2, "elapsed_ms": 4210,
			"packages": []string{"pipeline", "cache", "compress", "router", "toolcache", "rag", "budget", "lazyload"},
			"coverage": map[string]float64{"pipeline": 89.3, "cache": 97.2, "router": 100.0},
		}, nil
	case "read_file":
		return "package cache\n\n" + strings.Repeat("// computeBreakpoints is pure and deterministic.\n", 12) +
			"func computeBreakpoints(req *pipeline.Request, specs []BreakpointSpec) []pipeline.CacheBreakpoint {\n\t// ...\n}\n", nil
	case "search":
		var b strings.Builder
		for i := range 20 {
			fmt.Fprintf(&b, "cache/breakpoint.go:%d: computeBreakpoints(req, specs)\n", 40+i*3)
		}
		return b.String(), nil
	default:
		return nil, fmt.Errorf("unknown tool %q", call.Name)
	}
}

// shaRule answers git-SHA validation without a model — the same deterministic
// short-circuit the coding-agent example uses.
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
