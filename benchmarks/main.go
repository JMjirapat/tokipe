// Command benchmarks measures billed input tokens for the same synthetic
// workload with and without agentkit, against the acceptance criterion in
// spec §3.2: ≥30% reduction in billed input tokens.
//
// Run: go run ./benchmarks
//
// # What is being compared
//
// Both arms answer the identical 12-turn workload — a growing conversation,
// repeated tool calls, retrieval against a small corpus, and a handful of
// deterministic validation turns. Both arms talk to the same metered client.
// The only difference is what reaches it.
//
// The baseline is a plausible naive implementation, not a strawman: it keeps
// full history, sends the whole retrieved corpus uncompressed, re-executes and
// re-sends tool output every time it is referenced, and declares no cache
// breakpoints. That is what hand-rolled agent code usually does.
//
// # Accounting assumptions (stated because they drive the headline number)
//
//   - Tokens are estimated at 4 characters per token. Real tokenizers differ,
//     but both arms are measured the same way, so the ratio holds.
//   - A cache read is billed at 0.1× an ordinary input token, matching
//     Anthropic's prompt-caching pricing. A cache write is billed at 1.25×.
//   - A prefix counts as a cache read only if the exact same prefix was sent
//     before, which is precisely what CacheAlignStage is designed to preserve.
//   - Short-circuited turns cost zero input tokens: no request is made at all.
//
// The estimate is deliberately conservative: output tokens are ignored, and
// the baseline is charged nothing for the extra latency of its redundant tool
// executions.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/JMjirapat/tokipe"
	"github.com/JMjirapat/tokipe/budget"
	"github.com/JMjirapat/tokipe/config"
	"github.com/JMjirapat/tokipe/history"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/router"
	storemock "github.com/JMjirapat/tokipe/stores/mock"
	"github.com/JMjirapat/tokipe/toolcache"
)

const (
	charsPerToken   = 4
	cacheReadRate   = 0.10
	cacheWriteRate  = 1.25
	targetReduction = 30.0
)

func main() {
	base := run("baseline (no agentkit)", baselineArm)
	opt := run("agentkit v1 (full pipeline)", agentkitArm)
	// Phase 6 is measured as a separate arm so its contribution is visible on
	// its own rather than folded into the v1 headline. On this workload the
	// conversation is only 12 turns, so trimming has little to bite on — a
	// 100-turn loop is where it earns its keep. Reporting the small number is
	// the point: an optimization that helps elsewhere should not be credited
	// with a saving it did not produce here.
	trimmed := run("agentkit + history budget", historyArm)

	fmt.Printf("\n%s\n", strings.Repeat("═", 64))
	report("baseline", base)
	report("github.com/JMjirapat/tokipe", opt)

	report("+history", trimmed)
	reduction := 100 * (1 - opt.billed/base.billed)
	withHistory := 100 * (1 - trimmed.billed/base.billed)
	fmt.Printf("\n  billed input tokens : %.0f → %.0f\n", base.billed, opt.billed)
	fmt.Printf("  reduction           : %.1f%%   (target ≥ %.0f%%)\n", reduction, targetReduction)
	fmt.Printf("  with history budget : %.1f%%   (Phase 6 adds %+.1f pp here)\n",
		withHistory, withHistory-reduction)
	fmt.Printf("  history dropped     : %d messages across %d turns\n", trimmed.dropped, trimmed.turns)
	fmt.Printf("  turns short-circuited: %d/%d (%.0f%%)\n",
		opt.shortCircuited, opt.turns, 100*float64(opt.shortCircuited)/float64(opt.turns))
	fmt.Printf("  LLM calls avoided    : %d\n", base.calls-opt.calls)
	fmt.Printf("  tool executions      : %d → %d\n", base.toolExecs, opt.toolExecs)

	if reduction < targetReduction {
		fmt.Printf("\nFAIL: %.1f%% is below the %.0f%% acceptance criterion\n", reduction, targetReduction)
		os.Exit(1)
	}
	fmt.Printf("\nPASS: %.1f%% ≥ %.0f%%\n", reduction, targetReduction)

	measureLongLoop()
}

type result struct {
	billed         float64
	calls          int
	turns          int
	shortCircuited int
	toolExecs      int
	dropped        int
}

func report(label string, r result) {
	fmt.Printf("  %-9s  billed=%9.0f  llm_calls=%2d  tool_execs=%2d\n", label, r.billed, r.calls, r.toolExecs)
}

func run(label string, arm func(*meter) result) result {
	m := &meter{seen: map[string]bool{}}
	r := arm(m)
	r.billed = m.billed
	r.calls = m.calls
	fmt.Printf("%-26s → %.0f billed input tokens over %d turns\n", label, m.billed, r.turns)
	return r
}

// ── metered client ─────────────────────────────────────────────────────────

// meter is a ModelClient that prices a request instead of answering it. It
// simulates provider-side prompt caching: a static prefix it has seen before
// is billed at the cache-read rate, and the first sighting pays the write
// premium.
type meter struct {
	billed float64
	calls  int
	seen   map[string]bool
}

func (m *meter) Name() string { return "meter" }

func (m *meter) Send(_ context.Context, req *pipeline.Request) (*pipeline.Response, error) {
	m.calls++

	cachedChars, restChars := split(req)
	cached := float64(cachedChars) / charsPerToken
	rest := float64(restChars) / charsPerToken

	switch {
	case cachedChars == 0:
		m.billed += rest
	case m.seen[prefixKey(req)]:
		m.billed += cached*cacheReadRate + rest
	default:
		m.seen[prefixKey(req)] = true
		m.billed += cached*cacheWriteRate + rest
	}

	return &pipeline.Response{Content: "ok", ModelUsed: m.Name()}, nil
}

// split separates the characters covered by a cache breakpoint from the rest.
// With no breakpoint, everything is billed at full rate — which is exactly the
// baseline's problem.
func split(req *pipeline.Request) (cached, rest int) {
	last := -1
	for _, bp := range req.CacheBreakpoints {
		if bp.AfterMessageIndex > last {
			last = bp.AfterMessageIndex
		}
	}
	for i, msg := range req.Messages {
		if i <= last {
			cached += len(msg.Content)
		} else {
			rest += len(msg.Content)
		}
	}
	rest += len(req.Query)
	for _, c := range req.RetrievedChunks {
		rest += len(c.Content) + len(c.SourceURL)
	}
	if v, ok := req.Metadata[toolcache.MetaResults]; ok {
		if results, ok := v.(map[string]any); ok {
			for _, r := range results {
				rest += len(fmt.Sprint(r))
			}
		}
	}
	return cached, rest
}

func prefixKey(req *pipeline.Request) string {
	last := -1
	for _, bp := range req.CacheBreakpoints {
		if bp.AfterMessageIndex > last {
			last = bp.AfterMessageIndex
		}
	}
	var b strings.Builder
	for i, msg := range req.Messages {
		if i <= last {
			b.WriteString(msg.Content)
		}
	}
	return b.String()
}

// ── the two arms ───────────────────────────────────────────────────────────

func baselineArm(m *meter) result {
	p := pipeline.New(m) // no stages: straight through to the model
	execs := 0
	history := []pipeline.Message{{Role: "system", Content: systemPrompt, Static: true}}
	r := result{}

	for _, t := range workload() {
		r.turns++
		req := &pipeline.Request{Query: t.query, Messages: history}

		// Naive retrieval: send the whole corpus, uncompressed, every time.
		if t.needsRetrieval {
			req.RetrievedChunks = append([]pipeline.Chunk(nil), corpus...)
		}
		// Naive tools: execute every time, attach the raw output.
		if len(t.toolCalls) > 0 {
			results := map[string]any{}
			for _, call := range t.toolCalls {
				execs++
				v, _ := toolOutput(call)
				results[call.Name] = v
			}
			req.SetMeta(toolcache.MetaResults, results)
		}

		resp, err := p.Run(context.Background(), req)
		if err != nil {
			log.Fatalf("baseline: %v", err)
		}
		history = append(history,
			pipeline.Message{Role: "user", Content: t.query},
			pipeline.Message{Role: "assistant", Content: resp.Content},
		)
	}
	r.toolExecs = execs
	return r
}

// historyArm is agentkitArm plus budget enforcement, so the two differ by
// exactly one option and the delta is attributable.
func historyArm(m *meter) result {
	r, dropped := agentkitArmWith(m, config.WithHistoryBudget(
		budget.Policy{
			RoutineStepBudget:   400,
			NewQuestionBudget:   700,
			ErrorRecoveryBudget: 900,
		},
		nil, // CharEstimator: the benchmark has no provider to ask
		history.WithSummarizer(history.ElisionSummarizer()),
	))
	r.dropped = dropped
	return r
}

func agentkitArm(m *meter) result {
	r, _ := agentkitArmWith(m)
	return r
}

func agentkitArmWith(m *meter, extra ...config.Option) (result, int) {
	execs := 0
	exec := func(_ context.Context, call pipeline.ToolCall) (any, error) {
		execs++
		return toolOutput(call)
	}

	store := storemock.NewStore()
	store.Add(storemock.DefaultDim, corpus...)

	opts := []config.Option{
		config.WithMetrics(metrics.NewInMemory()),
		config.WithPreprocess(shaRule{}),
		config.WithToolCache(toolcache.NewMemoryCache(), exec, time.Hour),
		config.WithRAG(storemock.NewEmbedder(storemock.DefaultDim), store, 2),
		config.WithDefaultCompression(),
		config.WithCacheAlignment(),
		config.WithRouter(router.NewHeuristicRouter(router.Tier{Client: m, MaxComplexity: 1.0})),
	}
	kit := agentkit.New(m, append(opts, extra...)...)

	convo := []pipeline.Message{{Role: "system", Content: systemPrompt, Static: true}}
	r := result{}
	dropped := 0

	for _, t := range workload() {
		r.turns++
		req := &pipeline.Request{
			Query:          t.query,
			Messages:       convo,
			NeedsRetrieval: t.needsRetrieval,
			ToolCalls:      t.toolCalls,
			TurnType:       budget.Classify(&pipeline.Request{Query: t.query, ToolCalls: t.toolCalls}),
		}

		resp, err := kit.Run(context.Background(), req)
		if err != nil {
			log.Fatalf("agentkit: %v", err)
		}
		if resp.ShortCircuited {
			r.shortCircuited++
		}
		if n, ok := req.Metadata[history.MetaDropped].(int); ok {
			dropped += n
		}
		// Continue from what was actually sent, so trimming compounds the way it
		// would in a real agent rather than resetting each turn.
		//
		// Both the query and the reply are appended, exactly as the baseline arm
		// does. An earlier version of this loop appended only the reply, which
		// made the conversation grow slower in this arm than in the baseline and
		// inflated the headline from 57.4% to 67.4% for no real reason. The arms
		// must record identical content or the comparison means nothing.
		convo = append(append([]pipeline.Message(nil), req.Messages...),
			pipeline.Message{Role: "user", Content: t.query},
			pipeline.Message{Role: "assistant", Content: resp.Content})
	}
	r.toolExecs = execs
	return r, dropped
}

// ── long-loop measurement (Phase 6) ────────────────────────────────────────

// longLoopTurns is the horizon Phase 6 was built for. The 12-turn workload
// above never reaches its budget, so trimming contributes nothing there and the
// report says so; this is where unbounded history actually costs money.
const longLoopTurns = 100

// measureLongLoop runs the same growing conversation with and without history
// trimming and reports the difference.
//
// Deliberately separate from the main workload rather than replacing it: the
// v1 headline is a claim about that workload and must stay comparable across
// revisions. Adding a second measurement is honest; moving the goalposts of the
// first is not.
func measureLongLoop() {
	fmt.Printf("\n%s\nlong-loop measurement — %d turns, growing context\n%s\n",
		strings.Repeat("═", 64), longLoopTurns, strings.Repeat("─", 64))

	policy := budget.Policy{NewQuestionBudget: 1200}

	untrimmed, _, peakUntrimmed := longLoop(nil)
	trimmed, dropped, peakTrimmed := longLoop(&policy)

	saving := 100 * (1 - trimmed/untrimmed)
	fmt.Printf("  no budget      : %9.0f billed tokens, peak request %d tokens\n", untrimmed, peakUntrimmed)
	fmt.Printf("  history budget : %9.0f billed tokens, peak request %d tokens (limit %d)\n",
		trimmed, peakTrimmed, policy.NewQuestionBudget)
	fmt.Printf("  reduction      : %.1f%%, %d messages dropped\n", saving, dropped)

	if peakTrimmed > policy.NewQuestionBudget {
		fmt.Printf("\nFAIL: peak request %d exceeds the %d budget\n", peakTrimmed, policy.NewQuestionBudget)
		os.Exit(1)
	}
	fmt.Printf("  every turn stayed within budget\n")
}

// longLoop returns billed tokens, messages dropped, and the largest single
// request measured in estimator tokens. A nil policy disables trimming.
func longLoop(policy *budget.Policy) (billed float64, dropped, peak int) {
	m := &meter{seen: map[string]bool{}}

	opts := []config.Option{config.WithCacheAlignment()}
	if policy != nil {
		opts = append(opts, config.WithHistoryBudget(*policy, nil,
			history.WithSummarizer(history.ElisionSummarizer())))
	}
	kit := agentkit.New(m, opts...)

	convo := []pipeline.Message{{Role: "system", Content: systemPrompt, Static: true}}
	counter := budget.CharEstimator{}

	for turn := range longLoopTurns {
		req := &pipeline.Request{
			Query:    fmt.Sprintf("Turn %d: %s", turn, strings.Repeat("please continue the analysis ", 3)),
			Messages: convo,
			TurnType: pipeline.TurnNewQuestion,
		}

		resp, err := kit.Run(context.Background(), req)
		if err != nil {
			log.Fatalf("long loop: %v", err)
		}
		if n, ok := req.Metadata[history.MetaDropped].(int); ok {
			dropped += n
		}

		cost, err := budget.CountRequest(context.Background(), counter, req)
		if err != nil {
			log.Fatalf("count: %v", err)
		}
		if cost.Total > peak {
			peak = cost.Total
		}

		convo = append(append([]pipeline.Message(nil), req.Messages...),
			pipeline.Message{Role: "user", Content: req.Query},
			pipeline.Message{Role: "assistant", Content: resp.Content + " " + strings.Repeat("detail ", 8)},
		)
	}
	return m.billed, dropped, peak
}
