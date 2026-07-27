package tokipe_test

import (
	"context"
	"testing"
	"time"

	"github.com/JMjirapat/tokipe"
	"github.com/JMjirapat/tokipe/compress"
	"github.com/JMjirapat/tokipe/config"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/providers/mock"
	"github.com/JMjirapat/tokipe/router"
	"github.com/JMjirapat/tokipe/stores"
	"github.com/JMjirapat/tokipe/toolcache"
)

// Regression tests for QA-REPORT.md finding 1: a panic in caller-supplied code
// escaped Pipeline.Run and prevented the model call, violating the fail-open
// guarantee (spec §2.5.1).
//
// QA reported it for the tool Executor. Probing the other extension points
// showed the same defect in preprocess Rules, Compressors, and the
// Embedder/VectorStore — four instances of one bug, all fixed at the
// internal/safe boundary. Each is pinned here.
//
// Deliberately NOT covered: a panic from a caller's own pipeline.Stage added
// via config.WithStage. That is the caller's code in the caller's pipeline;
// swallowing it would hide their bug rather than tolerate a third party's.

// --- the exact reproduction from the QA report ------------------------------

func TestPanickingToolExecutorStillReachesTheModel(t *testing.T) {
	model := mock.New("m", "answer")
	rec := metrics.NewInMemory()

	kit := tokipe.New(model,
		config.WithMetrics(rec),
		config.WithToolCache(toolcache.NewMemoryCache(),
			func(context.Context, pipeline.ToolCall) (any, error) { panic("executor panic") },
			time.Minute),
	)

	resp, err := kit.Run(context.Background(), &pipeline.Request{
		Query:     "what happened?",
		ToolCalls: []pipeline.ToolCall{{Name: "demo", Args: map[string]any{"x": 1}}},
	})
	if err != nil {
		t.Fatalf("a panicking tool must not fail the turn: %v", err)
	}
	if resp.Content != "answer" {
		t.Fatalf("resp = %+v, want the model's answer", resp)
	}
	if model.Calls() != 1 {
		t.Fatalf("model calls = %d, want 1", model.Calls())
	}
	if rec.Count(toolcache.CounterPanic) != 1 {
		t.Errorf("a contained panic should be counted, got %d", rec.Count(toolcache.CounterPanic))
	}
}

// --- the three further instances found while verifying ----------------------

type panicRule struct{ onCanHandle bool }

func (panicRule) Name() string { return "panics" }

func (p panicRule) CanHandle(*pipeline.Request) bool {
	if p.onCanHandle {
		panic("CanHandle panic")
	}
	return true
}

func (p panicRule) Handle(*pipeline.Request) (*pipeline.Response, error) {
	panic("Handle panic")
}

func TestPanickingPreprocessRuleStillReachesTheModel(t *testing.T) {
	for name, rule := range map[string]panicRule{
		"panic in CanHandle": {onCanHandle: true},
		"panic in Handle":    {onCanHandle: false},
	} {
		t.Run(name, func(t *testing.T) {
			model := mock.New("m", "answer")
			kit := tokipe.New(model, config.WithPreprocess(rule))

			resp, err := kit.Run(context.Background(), &pipeline.Request{Query: "q"})
			if err != nil {
				t.Fatalf("a panicking rule must not fail the turn: %v", err)
			}
			if resp.Content != "answer" || model.Calls() != 1 {
				t.Fatalf("resp=%+v calls=%d, want the model to answer", resp, model.Calls())
			}
		})
	}
}

type panicCompressor struct{ onCanHandle bool }

func (p panicCompressor) CanHandle(string) bool {
	if p.onCanHandle {
		panic("CanHandle panic")
	}
	return true
}

func (p panicCompressor) Compress(string) (string, error) { panic("Compress panic") }

func TestPanickingCompressorLeavesChunkIntact(t *testing.T) {
	for name, c := range map[string]compress.Compressor{
		"panic in CanHandle": panicCompressor{onCanHandle: true},
		"panic in Compress":  panicCompressor{onCanHandle: false},
	} {
		t.Run(name, func(t *testing.T) {
			model := mock.New("m", "answer")
			kit := tokipe.New(model, config.WithCompression(c))

			req := &pipeline.Request{
				Query:           "q",
				RetrievedChunks: []pipeline.Chunk{{Content: "original content"}},
			}
			if _, err := kit.Run(context.Background(), req); err != nil {
				t.Fatalf("a panicking compressor must not fail the turn: %v", err)
			}
			if got := req.RetrievedChunks[0].Content; got != "original content" {
				t.Errorf("chunk = %q, want it left untouched", got)
			}
			if model.Calls() != 1 {
				t.Errorf("model calls = %d, want 1", model.Calls())
			}
		})
	}
}

type panicEmbedder struct{}

func (panicEmbedder) Embed(context.Context, string) ([]float32, error) { panic("embed panic") }

type panicStore struct{}

func (panicStore) Search(context.Context, []float32, int) ([]pipeline.Chunk, error) {
	panic("search panic")
}

type okEmbedder struct{}

func (okEmbedder) Embed(context.Context, string) ([]float32, error) { return []float32{1, 2, 3}, nil }

func TestPanickingRetrievalFallsOpen(t *testing.T) {
	cases := map[string]struct {
		embedder stores.Embedder
		store    stores.VectorStore
	}{
		"panic in Embed":  {panicEmbedder{}, panicStore{}},
		"panic in Search": {okEmbedder{}, panicStore{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			model := mock.New("m", "answer")
			kit := tokipe.New(model, config.WithRAG(tc.embedder, tc.store, 3))

			req := &pipeline.Request{Query: "q", NeedsRetrieval: true}
			if _, err := kit.Run(context.Background(), req); err != nil {
				t.Fatalf("panicking retrieval must not fail the turn: %v", err)
			}
			if len(req.RetrievedChunks) != 0 {
				t.Errorf("chunks = %v, want none after a failed retrieval", req.RetrievedChunks)
			}
			if model.Calls() != 1 {
				t.Errorf("model calls = %d, want 1", model.Calls())
			}
		})
	}
}

// A panicking cache backend is an optimization failure like any other.
type panicCache struct{}

func (panicCache) Get(context.Context, string, map[string]any) (toolcache.CachedResult, bool, error) {
	panic("cache backend panic")
}

func (panicCache) Set(context.Context, string, map[string]any, any, time.Duration) error {
	panic("cache backend panic")
}

func TestPanickingCacheBackendStillReachesTheModel(t *testing.T) {
	model := mock.New("m", "answer")
	kit := tokipe.New(model,
		config.WithToolCache(panicCache{},
			func(context.Context, pipeline.ToolCall) (any, error) { return "v", nil }, 0),
	)

	if _, err := kit.Run(context.Background(), &pipeline.Request{
		Query:     "q",
		ToolCalls: []pipeline.ToolCall{{Name: "t", Args: map[string]any{"a": 1}}},
	}); err != nil {
		t.Fatalf("a panicking cache must not fail the turn: %v", err)
	}
	if model.Calls() != 1 {
		t.Errorf("model calls = %d, want 1", model.Calls())
	}
}

// The whole point of fail-open: everything broken at once, turn still answered.
func TestEverythingPanicsAndTheTurnStillCompletes(t *testing.T) {
	model := mock.New("m", "answer")
	kit := tokipe.New(model,
		config.WithPreprocess(panicRule{}),
		config.WithToolCache(panicCache{},
			func(context.Context, pipeline.ToolCall) (any, error) { panic("exec") }, 0),
		config.WithRAG(panicEmbedder{}, panicStore{}, 3),
		config.WithCompression(panicCompressor{}),
		config.WithCacheAlignment(),
	)

	resp, err := kit.Run(context.Background(), &pipeline.Request{
		Query:          "q",
		NeedsRetrieval: true,
		ToolCalls:      []pipeline.ToolCall{{Name: "t", Args: map[string]any{"a": 1}}},
		Messages:       []pipeline.Message{{Role: "system", Content: "sys", Static: true}},
	})
	if err != nil {
		t.Fatalf("every optimization failing must still leave a working turn: %v", err)
	}
	if resp.Content != "answer" || model.Calls() != 1 {
		t.Fatalf("resp=%+v calls=%d", resp, model.Calls())
	}
}

// --- QA round 2 -------------------------------------------------------------

// R2-1: Name() is caller code too. Round 1 wrapped CanHandle and Handle but
// left Name outside the boundary, so a rule that had *already succeeded* could
// still take down the turn while being identified for metrics.
type panicNameRule struct{}

func (panicNameRule) CanHandle(*pipeline.Request) bool { return true }
func (panicNameRule) Handle(*pipeline.Request) (*pipeline.Response, error) {
	return &pipeline.Response{Content: "handled"}, nil
}
func (panicNameRule) Name() string { panic("rule name panic") }

func TestPanickingRuleNameStillShortCircuits(t *testing.T) {
	model := mock.New("m", "from-model")
	kit := tokipe.New(model, config.WithPreprocess(panicNameRule{}))

	resp, err := kit.Run(context.Background(), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("a panicking Name must not fail the turn: %v", err)
	}
	if resp.Content != "handled" || !resp.ShortCircuited {
		t.Fatalf("the rule's result must survive: %+v", resp)
	}
	if model.Calls() != 0 {
		t.Errorf("model calls = %d, want 0 — the rule handled it", model.Calls())
	}
}

// The same class, found while verifying R2-1 rather than reported: a
// ModelClient whose Name panics is used for routing metadata.
type panicNameClient struct{}

func (panicNameClient) Name() string { panic("client name panic") }
func (panicNameClient) Send(context.Context, *pipeline.Request) (*pipeline.Response, error) {
	return &pipeline.Response{Content: "routed"}, nil
}

func TestPanickingClientNameStillRoutes(t *testing.T) {
	kit := tokipe.New(mock.New("fallback", "no"),
		config.WithRouter(router.NewHeuristicRouter(
			router.Tier{Client: panicNameClient{}, MaxComplexity: 1.0})))

	req := &pipeline.Request{Query: "q"}
	resp, err := kit.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("a panicking client Name must not fail the turn: %v", err)
	}
	if resp.Content != "routed" {
		t.Fatalf("resp = %+v, want the routed client's answer", resp)
	}
	if got := req.Metadata["router.client"]; got != pipeline.UnnamedClient {
		t.Errorf("router.client = %v, want the %q fallback", got, pipeline.UnnamedClient)
	}
}

// R2-2: a panic must behave exactly like an error at each step. A Get error
// degrades to a miss and the tool runs, so a Get panic must too.
type panicGetCache struct{ sets int }

func (panicGetCache) Get(context.Context, string, map[string]any) (toolcache.CachedResult, bool, error) {
	panic("cache get panic")
}
func (c *panicGetCache) Set(context.Context, string, map[string]any, any, time.Duration) error {
	c.sets++
	return nil
}

func TestPanickingCacheGetDegradesToAMiss(t *testing.T) {
	execs := 0
	cache := &panicGetCache{}
	st := toolcache.NewStage(cache, func(context.Context, pipeline.ToolCall) (any, error) {
		execs++
		return "fresh value", nil
	})

	out, err := st.Process(context.Background(), &pipeline.Request{
		ToolCalls: []pipeline.ToolCall{{Name: "t", Args: map[string]any{"a": 1}}},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if execs != 1 {
		t.Fatalf("executor ran %d times, want 1 — a Get panic must be a miss, not a skip", execs)
	}
	results, ok := out.Metadata[toolcache.MetaResults].(map[string]any)
	if !ok || len(results) != 1 {
		t.Fatalf("the fresh result must still be published, got %v", out.Metadata[toolcache.MetaResults])
	}
	for _, v := range results {
		if v != "fresh value" {
			t.Errorf("got %v, want the executor's value", v)
		}
	}
}

// A Set panic must not discard a result the executor already produced.
type panicSetCache struct{}

func (panicSetCache) Get(context.Context, string, map[string]any) (toolcache.CachedResult, bool, error) {
	return toolcache.CachedResult{}, false, nil
}
func (panicSetCache) Set(context.Context, string, map[string]any, any, time.Duration) error {
	panic("cache set panic")
}

func TestPanickingCacheSetPreservesTheResult(t *testing.T) {
	st := toolcache.NewStage(panicSetCache{}, func(context.Context, pipeline.ToolCall) (any, error) {
		return "fresh value", nil
	})

	out, err := st.Process(context.Background(), &pipeline.Request{
		ToolCalls: []pipeline.ToolCall{{Name: "t", Args: map[string]any{"a": 1}}},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	results, ok := out.Metadata[toolcache.MetaResults].(map[string]any)
	if !ok || len(results) != 1 {
		t.Fatalf("a failed cache write must not lose the result, got %v", out.Metadata[toolcache.MetaResults])
	}
}
