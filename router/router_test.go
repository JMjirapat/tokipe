package router

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/providers/mock"
)

// simpleReq is a short, plain-prose request: near the bottom of the score range.
func simpleReq() *pipeline.Request {
	return &pipeline.Request{Query: "what time is the standup?"}
}

// heavyReq is long, code-dense and chunk-heavy: near the top of the score range.
func heavyReq() *pipeline.Request {
	code := "```go\nfunc f(x []int) (int, error) { s := 0; for i := range x { s += x[i] }; return s, nil }\n```\n"
	chunks := make([]pipeline.Chunk, 12)
	for i := range chunks {
		chunks[i] = pipeline.Chunk{Content: code}
	}
	return &pipeline.Request{
		Query:           "refactor this and explain the allocation profile",
		Messages:        []pipeline.Message{{Role: "user", Content: strings.Repeat(code, 120)}},
		RetrievedChunks: chunks,
	}
}

func TestEstimateComplexityBounded(t *testing.T) {
	tests := []struct {
		name string
		req  *pipeline.Request
	}{
		{"nil", nil},
		{"empty", &pipeline.Request{}},
		{"simple", simpleReq()},
		{"heavy", heavyReq()},
		{"only chunks", &pipeline.Request{RetrievedChunks: make([]pipeline.Chunk, 500)}},
		{"only code", &pipeline.Request{Query: "{}[]<>();=+*/&|_#$@`~^%"}},
		{"huge text", &pipeline.Request{Query: strings.Repeat("a", 1_000_000)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := estimateComplexity(tc.req)
			if got < 0 || got > 1 {
				t.Fatalf("score %v out of [0,1]", got)
			}
			// Determinism: the same Request always scores the same.
			for i := 0; i < 5; i++ {
				if again := estimateComplexity(tc.req); again != got {
					t.Fatalf("non-deterministic: %v then %v", got, again)
				}
			}
		})
	}
}

func TestEstimateComplexityOrdering(t *testing.T) {
	simple := estimateComplexity(simpleReq())
	heavy := estimateComplexity(heavyReq())
	if !(simple < heavy) {
		t.Fatalf("simple (%v) should score below heavy (%v)", simple, heavy)
	}
	if simple > 0.1 {
		t.Fatalf("a one-line prose question should be near zero, got %v", simple)
	}
	if heavy < 0.9 {
		t.Fatalf("a long code-heavy multi-chunk request should be near one, got %v", heavy)
	}
}

func TestEstimateComplexitySignalsAreIndependent(t *testing.T) {
	r := NewHeuristicRouterWith(nil)
	base := r.estimateComplexity(&pipeline.Request{})
	if base != 0 {
		t.Fatalf("empty request should score 0, got %v", base)
	}

	w := DefaultWeights()
	total := w.Length + w.Chunks + w.Code

	// Each maxed-out signal contributes exactly its normalized weight, and the
	// expectations below are recomputed from the documented formula rather than
	// hard-coded, so a weight change does not silently invalidate the test.

	// Prose long enough to saturate length, with no symbols and no chunks.
	lengthOnly := r.estimateComplexity(&pipeline.Request{Query: strings.Repeat("word ", 4000)})
	assertClose(t, "length", lengthOnly, w.Length/total)

	// Chunks saturated, no text at all.
	chunksOnly := r.estimateComplexity(&pipeline.Request{RetrievedChunks: make([]pipeline.Chunk, 10)})
	assertClose(t, "chunks", chunksOnly, w.Chunks/total)

	// Two fences saturate the code signal; the snippet still carries its own
	// (tiny) length contribution, which the expectation accounts for.
	code := "```\na=b(c);\n```"
	codeOnly := r.estimateComplexity(&pipeline.Request{Query: code})
	wantCode := (w.Code*1 + w.Length*(float64(len(code))/DefaultLengthSaturation)) / total
	assertClose(t, "code", codeOnly, wantCode)
}

func assertClose(t *testing.T, name string, got, want float64) {
	t.Helper()
	if d := got - want; d > 1e-9 || d < -1e-9 {
		t.Fatalf("%s signal = %v, want %v", name, got, want)
	}
}

func TestWeightOptions(t *testing.T) {
	req := &pipeline.Request{RetrievedChunks: make([]pipeline.Chunk, 10)}

	// Chunks-only weighting makes a chunk-saturated request score 1.
	r := NewHeuristicRouterWith(nil, WithWeights(Weights{Chunks: 1}))
	if got := r.estimateComplexity(req); got != 1 {
		t.Fatalf("chunks-only weights: got %v, want 1", got)
	}

	// Zeroing every weight degrades to "always cheapest".
	r = NewHeuristicRouterWith(nil, WithWeights(Weights{}))
	if got := r.estimateComplexity(heavyReq()); got != 0 {
		t.Fatalf("zero weights: got %v, want 0", got)
	}

	// Negative weights are clamped to zero, not allowed to unbalance the sum.
	r = NewHeuristicRouterWith(nil, WithWeights(Weights{Length: -5, Chunks: 1}))
	if got := r.estimateComplexity(req); got != 1 {
		t.Fatalf("negative weight: got %v, want 1", got)
	}
}

func TestSaturationOptions(t *testing.T) {
	// Raising the length saturation makes the same text look simpler.
	req := &pipeline.Request{Query: strings.Repeat("a", 4000)}
	tight := NewHeuristicRouterWith(nil, WithLengthSaturation(4000)).estimateComplexity(req)
	loose := NewHeuristicRouterWith(nil, WithLengthSaturation(80000)).estimateComplexity(req)
	if !(loose < tight) {
		t.Fatalf("looser saturation should lower the score: tight=%v loose=%v", tight, loose)
	}

	// Non-positive values are ignored, leaving the defaults in place.
	def := NewHeuristicRouterWith(nil).estimateComplexity(req)
	ignored := NewHeuristicRouterWith(nil,
		WithLengthSaturation(0),
		WithChunkSaturation(-1),
		WithFenceSaturation(0),
		WithSymbolDensity(0.5, 0.1), // floor >= ceil: rejected
		WithSymbolDensity(-1, 0.5),  // negative floor: rejected
		nil,
	).estimateComplexity(req)
	if ignored != def {
		t.Fatalf("invalid options should be ignored: %v != %v", ignored, def)
	}

	// Chunk and fence saturation are honoured.
	chunky := &pipeline.Request{RetrievedChunks: make([]pipeline.Chunk, 2)}
	if got := NewHeuristicRouterWith(nil, WithChunkSaturation(2), WithWeights(Weights{Chunks: 1})).
		estimateComplexity(chunky); got != 1 {
		t.Fatalf("chunk saturation: got %v, want 1", got)
	}
	fenced := &pipeline.Request{Query: "``` ``` ``` ```"} // 2 fenced blocks
	if got := NewHeuristicRouterWith(nil, WithFenceSaturation(2), WithWeights(Weights{Code: 1})).
		estimateComplexity(fenced); got != 1 {
		t.Fatalf("fence saturation: got %v, want 1", got)
	}

	// A widened density band makes symbol-dense prose look less code-like.
	dense := &pipeline.Request{Query: "a=b; c=d; e=f;"}
	narrow := NewHeuristicRouterWith(nil, WithWeights(Weights{Code: 1})).estimateComplexity(dense)
	wide := NewHeuristicRouterWith(nil, WithWeights(Weights{Code: 1}), WithSymbolDensity(0.5, 0.9)).
		estimateComplexity(dense)
	if !(wide < narrow) {
		t.Fatalf("wider density band should lower the code signal: narrow=%v wide=%v", narrow, wide)
	}
}

// The normalization helpers have defensive branches the option validators make
// unreachable from the public API; test them directly so they stay correct.
func TestNormalizationHelpers(t *testing.T) {
	nan := math.NaN()
	if got := ratio(5, 0); got != 1 {
		t.Fatalf("ratio(5, 0) = %v, want 1", got)
	}
	if got := ratio(0, 0); got != 0 {
		t.Fatalf("ratio(0, 0) = %v, want 0", got)
	}
	if got := ratio(4, -2); got != 1 {
		t.Fatalf("ratio(4, -2) = %v, want 1", got)
	}
	if got := clamp01(nan); got != 0 {
		t.Fatalf("clamp01(NaN) = %v, want 0", got)
	}
	if got := clamp01(2); got != 1 {
		t.Fatalf("clamp01(2) = %v, want 1", got)
	}
	if got := clamp01(-2); got != 0 {
		t.Fatalf("clamp01(-2) = %v, want 0", got)
	}
	if got := nonNeg(nan); got != 0 {
		t.Fatalf("nonNeg(NaN) = %v, want 0", got)
	}
	if got := symbolRatio(""); got != 0 {
		t.Fatalf("symbolRatio(\"\") = %v, want 0", got)
	}
	// NaN weights must not poison the score.
	r := NewHeuristicRouterWith(nil, WithWeights(Weights{Length: nan, Chunks: nan, Code: nan}))
	if got := r.estimateComplexity(heavyReq()); got != 0 {
		t.Fatalf("NaN weights: got %v, want 0", got)
	}
}

func TestRouteTierSelection(t *testing.T) {
	local := mock.New("local", "cheap answer")
	cloud := mock.New("cloud", "expensive answer")

	tests := []struct {
		name       string
		tiers      []Tier
		req        *pipeline.Request
		wantClient string
		wantReason string
	}{
		{
			name:       "below the lowest tier routes to that tier",
			tiers:      []Tier{{Client: local, MaxComplexity: 0.3}, {Client: cloud, MaxComplexity: 0.9}},
			req:        simpleReq(),
			wantClient: "local",
			wantReason: "heuristic",
		},
		{
			name:       "between tiers routes to the covering tier",
			tiers:      []Tier{{Client: local, MaxComplexity: 0.0}, {Client: cloud, MaxComplexity: 1.0}},
			req:        simpleReq(),
			wantClient: "cloud",
			wantReason: "heuristic",
		},
		{
			name:       "above every tier overflows to the last tier",
			tiers:      []Tier{{Client: local, MaxComplexity: 0.1}, {Client: cloud, MaxComplexity: 0.2}},
			req:        heavyReq(),
			wantClient: "cloud",
			wantReason: "heuristic_overflow",
		},
		{
			name:       "unsorted tiers are sorted defensively",
			tiers:      []Tier{{Client: cloud, MaxComplexity: 0.9}, {Client: local, MaxComplexity: 0.3}},
			req:        simpleReq(),
			wantClient: "local",
			wantReason: "heuristic",
		},
		{
			name:       "nil-client tiers are dropped",
			tiers:      []Tier{{Client: nil, MaxComplexity: 0.1}, {Client: local, MaxComplexity: 0.5}},
			req:        simpleReq(),
			wantClient: "local",
			wantReason: "heuristic",
		},
		{
			name:       "single tier always wins",
			tiers:      []Tier{{Client: cloud, MaxComplexity: 0.05}},
			req:        heavyReq(),
			wantClient: "cloud",
			wantReason: "heuristic_overflow",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewHeuristicRouter(tc.tiers...)
			d := r.Route(context.Background(), tc.req)
			if d.Client == nil {
				t.Fatal("expected a client")
			}
			if got := d.Client.Name(); got != tc.wantClient {
				t.Fatalf("client = %q, want %q", got, tc.wantClient)
			}
			if d.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", d.Reason, tc.wantReason)
			}
			if d.Confidence < 0 || d.Confidence > 1 {
				t.Fatalf("confidence %v out of [0,1]", d.Confidence)
			}
			if tc.wantReason == "heuristic_overflow" && d.Confidence != 0 {
				t.Fatalf("overflow confidence = %v, want 0", d.Confidence)
			}
		})
	}
}

// Local vs. cloud: the headline behaviour of the package.
func TestRouteLocalVsCloud(t *testing.T) {
	local := mock.New("local", "")
	cloud := mock.New("cloud", "")
	r := NewHeuristicRouter(
		Tier{Client: local, MaxComplexity: 0.35},
		Tier{Client: cloud, MaxComplexity: 1.0},
	)

	if got := r.Route(context.Background(), simpleReq()).Client.Name(); got != "local" {
		t.Fatalf("short simple request went to %q, want local", got)
	}
	if got := r.Route(context.Background(), heavyReq()).Client.Name(); got != "cloud" {
		t.Fatalf("long code-heavy request went to %q, want cloud", got)
	}
}

func TestRouteEmptyTiers(t *testing.T) {
	for _, r := range []*HeuristicRouter{
		NewHeuristicRouter(),
		NewHeuristicRouter(Tier{Client: nil, MaxComplexity: 1}),
	} {
		d := r.Route(context.Background(), heavyReq())
		if d.Client != nil {
			t.Fatalf("expected nil Client, got %v", d.Client)
		}
		if d != (RouteDecision{}) {
			t.Fatalf("expected the zero RouteDecision, got %+v", d)
		}
	}
}

// The pipeline treats a nil Client as "use the fallback" — the fail-open path.
func TestRouteFailsOpenThroughPipeline(t *testing.T) {
	fallback := mock.New("fallback", "ok")
	p := pipeline.NewWithRouter(fallback, NewHeuristicRouter())
	resp, err := p.Run(context.Background(), simpleReq())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.ModelUsed != "fallback" {
		t.Fatalf("ModelUsed = %q, want fallback", resp.ModelUsed)
	}
}

func TestRouterSatisfiesPipelineRouter(t *testing.T) {
	var _ Router = NewHeuristicRouter()
	var _ pipeline.Router = NewHeuristicRouter()

	local := mock.New("local", "hi")
	cloud := mock.New("cloud", "hi")
	p := pipeline.NewWithRouter(cloud, NewHeuristicRouter(
		Tier{Client: local, MaxComplexity: 0.35},
		Tier{Client: cloud, MaxComplexity: 1.0},
	))
	if _, err := p.Run(context.Background(), simpleReq()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if local.Calls() != 1 || cloud.Calls() != 0 {
		t.Fatalf("expected the local tier to serve: local=%d cloud=%d", local.Calls(), cloud.Calls())
	}
}

func TestRouteMetrics(t *testing.T) {
	rec := metrics.NewInMemory()
	r := NewHeuristicRouterWith(
		[]Tier{{Client: mock.New("local", ""), MaxComplexity: 0.35}},
		WithMetrics(rec),
	)
	r.Route(context.Background(), simpleReq())
	r.Route(context.Background(), heavyReq())
	if got := rec.Count("router_decision"); got != 2 {
		t.Fatalf("router_decision = %d, want 2", got)
	}

	// A nil recorder must not panic.
	NewHeuristicRouterWith(
		[]Tier{{Client: mock.New("local", ""), MaxComplexity: 1}},
		WithMetrics(nil),
	).Route(context.Background(), simpleReq())
}

func TestConstructorDoesNotMutateCallerSlice(t *testing.T) {
	local := mock.New("local", "")
	cloud := mock.New("cloud", "")
	tiers := []Tier{{Client: cloud, MaxComplexity: 0.9}, {Client: local, MaxComplexity: 0.3}}
	NewHeuristicRouter(tiers...)
	if tiers[0].Client != cloud || tiers[1].Client != local {
		t.Fatal("constructor reordered the caller's slice")
	}
}

func TestRouteConcurrent(t *testing.T) {
	r := NewHeuristicRouter(
		Tier{Client: mock.New("local", ""), MaxComplexity: 0.35},
		Tier{Client: mock.New("cloud", ""), MaxComplexity: 1.0},
	)
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				r.Route(context.Background(), simpleReq())
				r.Route(context.Background(), heavyReq())
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}
