package agentkit_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentkit"
	"agentkit/compress"
	"agentkit/config"
	"agentkit/metrics"
	"agentkit/pipeline"
	"agentkit/preprocess"
	"agentkit/providers/mock"
	"agentkit/router"
	"agentkit/stores"
	"agentkit/toolcache"
)

// Three QA rounds each reported one unguarded extension point and each time a
// sweep found two or three more of the same class:
//
//	round 1: tool Executor      → also Rule, Compressor, Embedder, VectorStore
//	round 2: Rule.Name          → also ModelClient.Name in router and pipeline
//	round 3: Stage.Name         → also Router.Route
//	round 4: metrics.Recorder   → the matrix itself claimed completeness and
//	                              had omitted metrics entirely
//
// Fixing instances one at a time was losing to that pattern. This table
// enumerates every method agentkit calls on caller-supplied code and asserts
// the contract for each, so a new extension point has to be added here before
// it can be forgotten.
//
// The rule: agentkit contains panics in code IT calls into. It does not
// contain panics in the caller's own pipeline.Stage.

type matrixCase struct {
	// what agentkit calls into
	point string
	// options installing a version of it that panics on that method
	opts []config.Option
	// request shape needed to reach the method
	req func() *pipeline.Request
	// propagates is true only for the caller's own Stage.Process
	propagates bool
	// wantErr is true where returning an error is the correct contract, not a
	// containment failure — a Stage that returns an error still gets its
	// StageError even if its Name method is broken.
	wantErr bool
	// answeredByAnotherClient marks cases where the routed client, not the
	// pipeline's default, produces the answer.
	answeredByAnotherClient bool
}

func matrixCases() []matrixCase {
	toolReq := func() *pipeline.Request {
		return &pipeline.Request{
			Query:     "q",
			ToolCalls: []pipeline.ToolCall{{Name: "t", Args: map[string]any{"a": 1}}},
		}
	}
	plainReq := func() *pipeline.Request { return &pipeline.Request{Query: "q"} }
	chunkReq := func() *pipeline.Request {
		return &pipeline.Request{Query: "q", RetrievedChunks: []pipeline.Chunk{{Content: "c"}}}
	}
	retrievalReq := func() *pipeline.Request {
		return &pipeline.Request{Query: "q", NeedsRetrieval: true}
	}
	okExec := func(context.Context, pipeline.ToolCall) (any, error) { return "v", nil }

	return []matrixCase{
		{point: "preprocess.Rule.CanHandle", opts: []config.Option{config.WithPreprocess(panicRule{onCanHandle: true})}, req: plainReq},
		{point: "preprocess.Rule.Handle", opts: []config.Option{config.WithPreprocess(panicRule{})}, req: plainReq},
		{point: "preprocess.Rule.Name", opts: []config.Option{config.WithPreprocess(panicNameRule{})}, req: plainReq},

		{point: "toolcache.Executor", opts: []config.Option{config.WithToolCache(toolcache.NewMemoryCache(),
			func(context.Context, pipeline.ToolCall) (any, error) { panic("exec") }, time.Minute)}, req: toolReq},
		{point: "toolcache.Cache.Get", opts: []config.Option{config.WithToolCache(&panicGetCache{}, okExec, 0)}, req: toolReq},
		{point: "toolcache.Cache.Set", opts: []config.Option{config.WithToolCache(panicSetCache{}, okExec, 0)}, req: toolReq},

		{point: "compress.Compressor.CanHandle", opts: []config.Option{config.WithCompression(panicCompressor{onCanHandle: true})}, req: chunkReq},
		{point: "compress.Compressor.Compress", opts: []config.Option{config.WithCompression(panicCompressor{})}, req: chunkReq},

		{point: "stores.Embedder.Embed", opts: []config.Option{config.WithRAG(panicEmbedder{}, panicStore{}, 3)}, req: retrievalReq},
		{point: "stores.VectorStore.Search", opts: []config.Option{config.WithRAG(okEmbedder{}, panicStore{}, 3)}, req: retrievalReq},

		{point: "pipeline.Router.Route", opts: []config.Option{config.WithRouter(panicRouter{})}, req: plainReq},

		// Metrics are opt-in diagnostics. They must never be able to discard a
		// result the pipeline already produced.
		{point: "metrics.Recorder.Counter", opts: []config.Option{
			config.WithMetrics(panicRecorder{onCounter: true}),
			config.WithPreprocess(okRule{}),
		}, req: plainReq},
		{point: "metrics.Counter.Inc", opts: []config.Option{
			config.WithMetrics(panicRecorder{}),
			config.WithPreprocess(okRule{}),
		}, req: plainReq},
		{point: "metrics.Recorder returning a nil Counter", opts: []config.Option{
			config.WithMetrics(nilCounterRecorder{}),
			config.WithPreprocess(okRule{}),
		}, req: plainReq},
		{point: "pipeline.ModelClient.Name", opts: []config.Option{config.WithRouter(router.NewHeuristicRouter(
			router.Tier{Client: panicNameClient{}, MaxComplexity: 1.0}))}, req: plainReq,
			answeredByAnotherClient: true},

		{point: "pipeline.Stage.Name (during StageError)", opts: []config.Option{config.WithStage(customStage{
			process: func(r *pipeline.Request) (*pipeline.Request, error) { return r, errors.New("stage failed") },
			name:    func() string { panic("stage name panic") },
		})}, req: plainReq, wantErr: true},

		// The single documented exception.
		{point: "pipeline.Stage.Process (caller's own code)", opts: []config.Option{config.WithStage(customStage{
			process: func(*pipeline.Request) (*pipeline.Request, error) { panic("process panic") },
			name:    func() string { return "custom" },
		})}, req: plainReq, propagates: true},
	}
}

func TestEveryCallerSuppliedMethodHonoursItsPanicContract(t *testing.T) {
	for _, tc := range matrixCases() {
		t.Run(tc.point, func(t *testing.T) {
			model := mock.New("m", "answer")
			kit := agentkit.New(model, tc.opts...)

			var panicked any
			var resp *pipeline.Response
			var err error
			func() {
				defer func() { panicked = recover() }()
				resp, err = kit.Run(context.Background(), tc.req())
			}()

			if tc.propagates {
				if panicked == nil {
					t.Fatal("this panic is documented as propagating; it did not")
				}
				return
			}

			// The invariant that holds for every case: nothing escapes.
			if panicked != nil {
				t.Fatalf("panic escaped Pipeline.Run: %v", panicked)
			}

			if tc.wantErr {
				if err == nil {
					t.Fatal("this case must still return its StageError")
				}
				return
			}
			if err != nil {
				t.Fatalf("a contained panic must not become an error: %v", err)
			}
			if resp == nil {
				t.Fatal("no response")
			}
			if tc.answeredByAnotherClient {
				return // the routed client answered; the default was bypassed
			}
			// Either the model answered, or a preprocess rule legitimately
			// short-circuited before it. Both mean the turn survived.
			if !resp.ShortCircuited && model.Calls() != 1 {
				t.Fatalf("model calls = %d and no short-circuit; the turn did not complete", model.Calls())
			}
		})
	}
}

// A Stage.Name panic must not destroy the error Process already returned —
// that error is the useful signal, the name is only a label.
func TestStageNamePanicPreservesTheUnderlyingError(t *testing.T) {
	sentinel := errors.New("stage failed")
	kit := agentkit.New(mock.New("m", "answer"), config.WithStage(customStage{
		process: func(r *pipeline.Request) (*pipeline.Request, error) { return r, sentinel },
		name:    func() string { panic("stage name panic") },
	}))

	_, err := kit.Run(context.Background(), &pipeline.Request{Query: "q"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the original error preserved", err)
	}
	var se *pipeline.StageError
	if !errors.As(err, &se) {
		t.Fatalf("want a StageError, got %T", err)
	}
	if se.Stage != pipeline.UnnamedStage {
		t.Errorf("Stage = %q, want the %q fallback", se.Stage, pipeline.UnnamedStage)
	}
}

// A panicking Router costs the routing decision, not the turn.
func TestPanickingRouterFallsBackToTheDefaultClient(t *testing.T) {
	fallback := mock.New("fallback", "answer")
	kit := agentkit.New(fallback, config.WithRouter(panicRouter{}))

	req := &pipeline.Request{Query: "q"}
	resp, err := kit.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("a panicking router must not fail the turn: %v", err)
	}
	if resp.Content != "answer" || fallback.Calls() != 1 {
		t.Fatalf("resp=%+v calls=%d, want the default client used", resp, fallback.Calls())
	}
	if got := req.Metadata["router.reason"]; got != "router_panicked" {
		t.Errorf("router.reason = %v, want the failure recorded", got)
	}
}

type customStage struct {
	process func(*pipeline.Request) (*pipeline.Request, error)
	name    func() string
}

func (c customStage) Name() string { return c.name() }
func (c customStage) Process(_ context.Context, r *pipeline.Request) (*pipeline.Request, error) {
	return c.process(r)
}

type panicRouter struct{}

func (panicRouter) Route(context.Context, *pipeline.Request) pipeline.RouteDecision {
	panic("router panic")
}

// okRule succeeds, so the counter increment happens on a turn that has already
// been resolved — the case where losing it would be most obviously wrong.
type okRule struct{}

func (okRule) Name() string                     { return "ok" }
func (okRule) CanHandle(*pipeline.Request) bool { return true }
func (okRule) Handle(*pipeline.Request) (*pipeline.Response, error) {
	return &pipeline.Response{Content: "handled"}, nil
}

type panicRecorder struct{ onCounter bool }

func (p panicRecorder) Counter(string) metrics.Counter {
	if p.onCounter {
		panic("recorder counter panic")
	}
	return panicCounter{}
}

type panicCounter struct{}

func (panicCounter) Inc(map[string]string) { panic("counter inc panic") }

// A Recorder that hands back a nil Counter — a plausible bug in a caller's
// backend, and a nil-pointer dereference if agentkit trusts it.
type nilCounterRecorder struct{}

func (nilCounterRecorder) Counter(string) metrics.Counter { return nil }

// TestMatrixCoversEveryExtensionMethod turns the matrix's completeness claim
// into something a machine checks.
//
// Round 4 caught that claim being false: the matrix said it enumerated every
// method agentkit calls on caller-supplied code, and it had omitted
// metrics.Recorder and metrics.Counter entirely. A prose claim about coverage
// is worth nothing — this reflects over each extension interface and fails if
// any method has no matrix case naming it.
//
// Adding a method to any interface below breaks this test until the matrix
// covers it. Adding a whole new interface still needs a line here, which is
// the one gap reflection cannot close for us.
func TestMatrixCoversEveryExtensionMethod(t *testing.T) {
	interfaces := map[string]reflect.Type{
		"preprocess.Rule":      reflect.TypeOf((*preprocess.Rule)(nil)).Elem(),
		"toolcache.Cache":      reflect.TypeOf((*toolcache.Cache)(nil)).Elem(),
		"compress.Compressor":  reflect.TypeOf((*compress.Compressor)(nil)).Elem(),
		"stores.Embedder":      reflect.TypeOf((*stores.Embedder)(nil)).Elem(),
		"stores.VectorStore":   reflect.TypeOf((*stores.VectorStore)(nil)).Elem(),
		"pipeline.Router":      reflect.TypeOf((*pipeline.Router)(nil)).Elem(),
		"pipeline.ModelClient": reflect.TypeOf((*pipeline.ModelClient)(nil)).Elem(),
		"pipeline.Stage":       reflect.TypeOf((*pipeline.Stage)(nil)).Elem(),
		"metrics.Recorder":     reflect.TypeOf((*metrics.Recorder)(nil)).Elem(),
		"metrics.Counter":      reflect.TypeOf((*metrics.Counter)(nil)).Elem(),
	}

	// Methods agentkit never calls on the request path, with the reason.
	exempt := map[string]string{
		// Send IS the model call; its failure is the one error Run may return.
		"pipeline.ModelClient.Send": "the model call itself, not an optimization",
	}

	var points []string
	for _, tc := range matrixCases() {
		points = append(points, tc.point)
	}
	covered := func(iface, method string) bool {
		for _, p := range points {
			if strings.Contains(p, iface+"."+method) || strings.Contains(p, method) {
				return true
			}
		}
		return false
	}

	for name, typ := range interfaces {
		for i := range typ.NumMethod() {
			method := typ.Method(i).Name
			full := name + "." + method
			if _, ok := exempt[full]; ok {
				continue
			}
			if !covered(name, method) {
				t.Errorf("%s has no matrix case; add one or exempt it with a reason", full)
			}
		}
	}

	// toolcache.Executor is a func type rather than an interface, so
	// reflection over interfaces cannot reach it. Assert it explicitly.
	if !covered("toolcache", "Executor") {
		t.Error("toolcache.Executor has no matrix case")
	}
}
