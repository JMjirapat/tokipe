// Command observability renders a terminal dashboard from tokipe's own
// counters, histograms and degradation events.
//
//	go run ./examples/observability          # everything healthy
//	go run ./examples/observability -break   # dependencies failing, turns still served
//
// It exists to demonstrate the point of Phase 7: fail-open keeps a turn alive
// when an optimization breaks, which is right for availability and blind for
// operations. Run it with -break and watch every turn still succeed while the
// degradation log fills up. That log is the difference between "the system is
// fine" and "the system is fine *because* nothing is optimizing any more".
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/JMjirapat/tokipe"
	"github.com/JMjirapat/tokipe/budget"
	"github.com/JMjirapat/tokipe/config"
	"github.com/JMjirapat/tokipe/history"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/preprocess"
	"github.com/JMjirapat/tokipe/providers/mock"
	"github.com/JMjirapat/tokipe/stores"
	"github.com/JMjirapat/tokipe/toolcache"
)

func main() {
	broken := flag.Bool("break", false, "make every optional dependency fail")
	turns := flag.Int("turns", 40, "how many turns to run")
	flag.Parse()

	// Observability implements Recorder plus the optional histogram, gauge and
	// degradation interfaces. A caller that implements only Recorder still gets
	// counters; the rest is silently skipped.
	rec := metrics.NewObservability()

	kit, tools := build(rec, *broken)

	start := time.Now()
	for i := range *turns {
		req := turn(i)
		if _, err := kit.Run(context.Background(), req); err != nil {
			log.Fatalf("turn %d failed — fail-open is broken: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	// Hit rate is a derived value, so it is a gauge the caller sets rather than
	// something the library can compute for you.
	hits := rec.Count(toolcache.CounterHit)
	misses := rec.Count(toolcache.CounterMiss)
	if hits+misses > 0 {
		metrics.Set(rec, "toolcache.hit_rate", float64(hits)/float64(hits+misses), nil)
	}

	render(rec, *turns, *broken, elapsed, tools)
}

func build(rec metrics.Recorder, broken bool) (*pipeline.Pipeline, *int) {
	toolRuns := 0
	exec := func(_ context.Context, c pipeline.ToolCall) (any, error) {
		toolRuns++
		if broken {
			return nil, errors.New("tool backend unreachable")
		}
		return "exit 0: 178 passed", nil
	}

	var (
		toolStore toolcache.Cache     = toolcache.NewMemoryCache()
		embedder  stores.Embedder     = okEmbedder{}
		vectors   stores.VectorStore  = okStore{}
		counter   budget.TokenCounter = budget.CharEstimator{}
	)
	if broken {
		toolStore = brokenCache{}
		embedder = brokenEmbedder{}
		counter = brokenCounter{}
	}

	kit := tokipe.New(mock.New("model", "an answer from the model"),
		config.WithMetrics(rec),
		config.WithPreprocess(shaRule{broken: broken}),
		config.WithToolCache(toolStore, exec, time.Minute),
		config.WithRAG(embedder, vectors, 3),
		config.WithDefaultCompression(),
		config.WithHistoryBudget(budget.Policy{NewQuestionBudget: 600}, counter),
		config.WithCacheAlignment(),
	)
	return kit, &toolRuns
}

func turn(i int) *pipeline.Request {
	req := &pipeline.Request{
		Messages: []pipeline.Message{
			{Role: "system", Content: "You are a terse assistant.", Static: true},
		},
		TurnType: pipeline.TurnNewQuestion,
	}
	// A growing conversation, so the history budget has something to do.
	for j := range i {
		req.Messages = append(req.Messages,
			pipeline.Message{Role: "user", Content: fmt.Sprintf("earlier question %d %s", j, strings.Repeat("q ", 8))},
			pipeline.Message{Role: "assistant", Content: fmt.Sprintf("earlier answer %d %s", j, strings.Repeat("a ", 8))})
	}

	switch i % 4 {
	case 0:
		req.Query = "validate sha 3f2a1c9d4e5b6a7c8d9e0f1a2b3c4d5e6f7a8b9c" // short-circuits
	case 1:
		req.Query = "What do the docs say about caching?"
		req.NeedsRetrieval = true
	case 2:
		req.Query = "Run the tests."
		req.ToolCalls = []pipeline.ToolCall{{Name: "run_tests", Args: map[string]any{"pkg": "./..."}}}
	default:
		req.Query = "Summarise the thread so far."
	}
	return req
}

// --- rendering --------------------------------------------------------------

func render(rec *metrics.Observability, turns int, broken bool, elapsed time.Duration, toolRuns *int) {
	mode := "healthy"
	if broken {
		mode = "every optional dependency FAILING"
	}
	line := strings.Repeat("─", 68)

	fmt.Printf("\n tokipe observability — %d turns in %v — %s\n%s\n",
		turns, elapsed.Round(time.Millisecond), mode, line)

	// Counters.
	fmt.Printf("\n COUNTERS\n")
	snap := rec.Snapshot()
	names := make([]string, 0, len(snap))
	for n := range snap {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("   %-34s %6d\n", n, snap[n])
	}

	// Derived rates: the numbers an operator actually watches.
	short := rec.Count("preprocess.short_circuit")
	hits, misses := rec.Count(toolcache.CounterHit), rec.Count(toolcache.CounterMiss)
	fmt.Printf("\n RATES\n")
	fmt.Printf("   %-34s %5.0f%%   (%d/%d turns answered with no model call)\n",
		"short-circuit rate", 100*float64(short)/float64(turns), short, turns)
	if hits+misses > 0 {
		fmt.Printf("   %-34s %5.0f%%   (%d hits, %d misses)\n",
			"tool cache hit rate", 100*rec.GaugeValue("toolcache.hit_rate"), hits, misses)
	} else {
		fmt.Printf("   %-34s     —   (the cache was never reached)\n", "tool cache hit rate")
	}
	fmt.Printf("   %-34s %6d   real tool executions\n", "tool executions", *toolRuns)

	// Per-stage latency, from the histograms metrics.Timed records.
	fmt.Printf("\n STAGE LATENCY (ms)\n")
	if !metrics.SupportsHistograms(rec) {
		fmt.Printf("   this recorder does not implement histograms\n")
	} else {
		printLatency(rec)
	}

	// Token sizes, so a budget can be checked against real traffic.
	if n, mean, peak := rec.Summary(history.MetricTokensAfter); n > 0 {
		fmt.Printf("\n CONTEXT SIZE AFTER TRIMMING (tokens)\n")
		fmt.Printf("   %d trimmed turns, mean %.0f, peak %.0f\n", n, mean, peak)
	}

	// The part that only exists because of Phase 7.
	events := rec.Degradations()
	fmt.Printf("\n DEGRADATIONS  (%d)\n", len(events))
	if len(events) == 0 {
		fmt.Printf("   none — every optimization did its job\n")
	} else {
		grouped := map[string]int{}
		for _, d := range events {
			grouped[d.Stage+" / "+d.Reason]++
		}
		keys := make([]string, 0, len(grouped))
		for k := range grouped {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("   %-44s ×%d\n", k, grouped[k])
		}
		fmt.Printf("\n   most recent: %v\n", events[len(events)-1])
	}

	fmt.Printf("\n%s\n", line)
	if broken {
		fmt.Printf(" All %d turns still succeeded. Without the degradation log above,\n"+
			" nothing here would have told you the pipeline stopped optimizing.\n", turns)
	} else {
		fmt.Printf(" Re-run with -break to see the same turns succeed while every\n" +
			" optimization fails, and what that looks like from the outside.\n")
	}
}

func printLatency(rec *metrics.Observability) {
	byStage := rec.SummaryBy(metrics.StageLatency, "stage")
	if len(byStage) == 0 {
		fmt.Printf("   no observations recorded\n")
		return
	}
	stages := make([]string, 0, len(byStage))
	for name := range byStage {
		stages = append(stages, name)
	}
	// Slowest first: the one worth looking at is the one costing the most.
	sort.Slice(stages, func(i, j int) bool {
		return byStage[stages[i]].Mean > byStage[stages[j]].Mean
	})

	fmt.Printf("   %-16s %8s %10s %10s\n", "stage", "calls", "mean", "peak")
	for _, name := range stages {
		s := byStage[name]
		fmt.Printf("   %-16s %8d %10.3f %10.3f\n", name, s.Count, s.Mean, s.Max)
	}
	n, mean, peak := rec.Summary(metrics.StageLatency)
	fmt.Printf("   %-16s %8d %10.3f %10.3f\n", "ALL", n, mean, peak)
}

// --- deliberately broken dependencies ---------------------------------------

type shaRule struct{ broken bool }

func (shaRule) Name() string { return "git_sha_validator" }

func (r shaRule) CanHandle(req *pipeline.Request) bool {
	return strings.HasPrefix(strings.ToLower(req.Query), "validate sha ")
}

func (r shaRule) Handle(req *pipeline.Request) (*pipeline.Response, error) {
	if r.broken {
		return nil, errors.New("validator dependency unavailable")
	}
	return &pipeline.Response{Content: "sha is valid", ModelUsed: "none"}, nil
}

type okEmbedder struct{}

func (okEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

type brokenEmbedder struct{}

func (brokenEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("embedding service timeout")
}

type okStore struct{}

func (okStore) Search(context.Context, []float32, int) ([]pipeline.Chunk, error) {
	return []pipeline.Chunk{
		{Content: `{"caching":"prefix based","_debug":"drop me"}`, SourceURL: "docs/cache", Similarity: 0.9},
		{Content: "   Padded    prose   about   caching.   ", SourceURL: "docs/prose", Similarity: 0.5},
	}, nil
}

type brokenCache struct{}

func (brokenCache) Get(context.Context, string, map[string]any) (toolcache.CachedResult, bool, error) {
	panic("cache backend connection reset")
}

func (brokenCache) Set(context.Context, string, map[string]any, any, time.Duration) error {
	return errors.New("cache backend read-only")
}

type brokenCounter struct{}

func (brokenCounter) CountTokens(context.Context, string) (int, error) {
	return 0, errors.New("tokenizer endpoint 503")
}

var _ preprocess.Rule = shaRule{}
