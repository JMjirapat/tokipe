package config_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"agentkit/budget"
	"agentkit/cache"
	"agentkit/compress"
	"agentkit/config"
	"agentkit/pipeline"
	"agentkit/preprocess"
	"agentkit/stores/mock"
	"agentkit/toolcache"
)

func names(stages []pipeline.Stage) []string {
	out := make([]string, len(stages))
	for i, s := range stages {
		out[i] = s.Name()
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func allOptions() []config.Option {
	rule := preprocess.RuleFunc{
		RuleName: "never",
		Match:    func(*pipeline.Request) bool { return false },
	}
	exec := func(context.Context, pipeline.ToolCall) (any, error) { return nil, nil }
	return []config.Option{
		config.WithPreprocess(rule),
		config.WithToolCache(toolcache.NewMemoryCache(), exec, time.Minute),
		config.WithRAG(mock.NewEmbedder(mock.DefaultDim), mock.NewStore(), 3),
		config.WithDefaultCompression(),
		config.WithHistoryBudget(budget.DefaultPolicy(), nil),
		config.WithCacheAlignment(),
	}
}

// The whole point of this package: the ordering is not the caller's to get
// wrong. Retrieval must precede compression, and both must precede cache
// alignment (spec §2.4.3, §2.4.6).
func TestStagesAreInTheRequiredOrder(t *testing.T) {
	want := []string{"preprocess", "toolcache", "rag", "compress", "history", "cache_align"}
	got := names(config.New(allOptions()...).Stages())
	if !equal(got, want) {
		t.Fatalf("stage order = %v, want %v", got, want)
	}
}

func TestOptionOrderDoesNotAffectStageOrder(t *testing.T) {
	opts := allOptions()
	reversed := make([]config.Option, len(opts))
	for i, o := range opts {
		reversed[len(opts)-1-i] = o
	}

	forward := names(config.New(opts...).Stages())
	backward := names(config.New(reversed...).Stages())
	if !equal(forward, backward) {
		t.Fatalf("declaring options in reverse changed the pipeline: %v vs %v", forward, backward)
	}
}

func TestExtraStagesRunBeforeAlignment(t *testing.T) {
	extra := stubStage{name: "custom"}
	got := names(config.New(append(allOptions(), config.WithStage(extra))...).Stages())

	last := got[len(got)-1]
	if last != "cache_align" {
		t.Fatalf("cache_align must stay last, got %q", last)
	}
	if got[len(got)-2] != "custom" {
		t.Fatalf("custom stage should sit just before alignment, got %v", got)
	}
}

func TestZeroConfigIsPassThrough(t *testing.T) {
	if got := config.New().Stages(); len(got) != 0 {
		t.Fatalf("zero config should enable nothing, got %v", names(got))
	}
}

func TestPartialConfigOnlyEnablesWhatIsSupplied(t *testing.T) {
	cases := map[string][]config.Option{
		"rag needs both embedder and store":      {config.WithRAG(mock.NewEmbedder(mock.DefaultDim), nil, 3)},
		"toolcache needs an executor":            {config.WithToolCache(toolcache.NewMemoryCache(), nil, 0)},
		"toolcache needs a cache":                {config.WithToolCache(nil, func(context.Context, pipeline.ToolCall) (any, error) { return nil, nil }, 0)},
		"no compressors, no stage":               {config.WithCompression()},
		"an empty budget policy enables nothing": {config.WithHistoryBudget(budget.Policy{}, nil)},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if got := config.New(opts...).Stages(); len(got) != 0 {
				t.Fatalf("expected no stages, got %v", names(got))
			}
		})
	}
}

func TestCacheAlignmentUsesDefaultSpecsWhenNoneGiven(t *testing.T) {
	withDefault := config.New(config.WithCacheAlignment())
	if len(withDefault.AlignerSpecs) != 0 {
		t.Fatal("no specs should be stored when none are passed")
	}
	if len(withDefault.Stages()) != 1 {
		t.Fatal("alignment stage should still be built from cache.DefaultSpecs")
	}

	custom := config.New(config.WithCacheAlignment(cache.BreakpointSpec{Reason: "custom"}))
	if len(custom.AlignerSpecs) != 1 {
		t.Fatalf("explicit specs should be kept, got %v", custom.AlignerSpecs)
	}
}

func TestNilOptionIsIgnored(t *testing.T) {
	if got := config.New(nil, config.WithDefaultCompression()); len(got.Stages()) != 1 {
		t.Fatalf("a nil option must be skipped, not panic")
	}
}

func TestDefaultCompressionOrdersJSONBeforeText(t *testing.T) {
	c := config.New(config.WithDefaultCompression())
	if len(c.Compressors) != 2 {
		t.Fatalf("want 2 compressors, got %d", len(c.Compressors))
	}
	if _, ok := c.Compressors[0].(*compress.JSONCompressor); !ok {
		t.Fatalf("JSON compressor must be tried first, got %T", c.Compressors[0])
	}
}

type stubStage struct{ name string }

func (s stubStage) Name() string { return s.name }
func (s stubStage) Process(_ context.Context, r *pipeline.Request) (*pipeline.Request, error) {
	return r, nil
}

// History trimming must sit after compression (it needs the final payload size)
// and before alignment (which places breakpoints over the final layout).
// Getting this wrong is silent: alignment would anchor to content trimming then
// removed.
func TestHistoryTrimsAfterCompressionAndBeforeAlignment(t *testing.T) {
	got := names(config.New(allOptions()...).Stages())

	idx := func(name string) int {
		for i, n := range got {
			if n == name {
				return i
			}
		}
		return -1
	}
	compressAt, historyAt, alignAt := idx("compress"), idx("history"), idx("cache_align")
	if compressAt < 0 || historyAt < 0 || alignAt < 0 {
		t.Fatalf("missing stages in %v", got)
	}
	if !(compressAt < historyAt && historyAt < alignAt) {
		t.Fatalf("order = %v; want compress < history < cache_align", got)
	}
}

// A custom stage still cannot get between history and alignment.
func TestCustomStagesStayAfterHistory(t *testing.T) {
	got := names(config.New(append(allOptions(), config.WithStage(stubStage{name: "custom"}))...).Stages())
	if got[len(got)-1] != "cache_align" || got[len(got)-2] != "custom" {
		t.Fatalf("order = %v; want [... history custom cache_align]", got)
	}
}

// Dedupe must precede compression: comparing raw text is more reliable than
// comparing text two compressors have already rewritten, and compressing a
// chunk that is about to be dropped is wasted work.
func TestDedupeRunsBeforeCompression(t *testing.T) {
	got := names(config.New(append(allOptions(), config.WithChunkDedupe())...).Stages())

	idx := func(name string) int {
		for i, n := range got {
			if n == name {
				return i
			}
		}
		return -1
	}
	dedupeAt, compressAt := idx("dedupe"), idx("compress")
	if dedupeAt < 0 || compressAt < 0 {
		t.Fatalf("missing stages in %v", got)
	}
	if dedupeAt > compressAt {
		t.Fatalf("order = %v; want dedupe before compress", got)
	}
	// And still ahead of everything that reads the final layout.
	if idx("dedupe") > idx("cache_align") {
		t.Fatalf("order = %v; dedupe must precede alignment", got)
	}
}

func TestDedupeIsOptional(t *testing.T) {
	if got := names(config.New(allOptions()...).Stages()); slices.Contains(got, "dedupe") {
		t.Errorf("dedupe should be off unless requested: %v", got)
	}
}
