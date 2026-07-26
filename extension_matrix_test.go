package agentkit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentkit"
	"agentkit/config"
	"agentkit/pipeline"
	"agentkit/providers/mock"
	"agentkit/router"
	"agentkit/toolcache"
)

// Three QA rounds each reported one unguarded extension point and each time a
// sweep found two or three more of the same class:
//
//	round 1: tool Executor      → also Rule, Compressor, Embedder, VectorStore
//	round 2: Rule.Name          → also ModelClient.Name in router and pipeline
//	round 3: Stage.Name         → also Router.Route
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
