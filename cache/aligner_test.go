package cache

import (
	"context"
	"reflect"
	"testing"

	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
)

func msg(role, content string, static bool) pipeline.Message {
	return pipeline.Message{Role: role, Content: content, Static: static}
}

// baseRequest builds a request with a realistic mix: two static messages
// (interleaved with history so the reordering has something to do), three
// history turns, and a newest user message.
func baseRequest() *pipeline.Request {
	return &pipeline.Request{
		Query: "newest question",
		Messages: []pipeline.Message{
			msg("system", "you are a helpful assistant", false),
			msg("user", "h1", false),
			msg("user", "tool definitions", true),
			msg("assistant", "h2", false),
			msg("user", "h3", false),
			msg("user", "newest question", false),
		},
	}
}

func TestProcess_ReordersIntoSegments(t *testing.T) {
	req := baseRequest()
	got, err := NewAligner().Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process returned error: %v", err)
	}

	want := []string{
		"you are a helpful assistant", // static
		"tool definitions",            // static
		"h1", "h2", "h3",              // history, original relative order
		"newest question", // newest
	}
	if len(got.Messages) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got.Messages), len(want))
	}
	for i, w := range want {
		if got.Messages[i].Content != w {
			t.Errorf("message[%d] = %q, want %q", i, got.Messages[i].Content, w)
		}
	}
}

// Reordering must be a *stable* partition: within-segment order is preserved.
func TestPartition_StableWithinSegments(t *testing.T) {
	msgs := []pipeline.Message{
		msg("user", "s1", true),
		msg("user", "d1", false),
		msg("user", "s2", true),
		msg("user", "d2", false),
		msg("user", "s3", true),
		msg("user", "d3", false),
		msg("user", "newest", false),
	}
	seg := partition(msgs)

	var statics, history []string
	for i, m := range seg.ordered {
		switch {
		case i < seg.staticLen:
			statics = append(statics, m.Content)
		case i < seg.dynamicBoundary():
			history = append(history, m.Content)
		}
	}
	if want := []string{"s1", "s2", "s3"}; !reflect.DeepEqual(statics, want) {
		t.Errorf("static segment = %v, want %v", statics, want)
	}
	if want := []string{"d1", "d2", "d3"}; !reflect.DeepEqual(history, want) {
		t.Errorf("history segment = %v, want %v", history, want)
	}
	if !seg.hasNewest || seg.ordered[len(seg.ordered)-1].Content != "newest" {
		t.Errorf("newest message not last: %v", seg.ordered)
	}
}

func TestPartition_Idempotent(t *testing.T) {
	msgs := baseRequest().Messages
	once := partition(msgs)
	twice := partition(once.ordered)
	if !reflect.DeepEqual(once.ordered, twice.ordered) {
		t.Errorf("partition not idempotent:\n once=%v\ntwice=%v", once.ordered, twice.ordered)
	}
	if once.staticLen != twice.staticLen || once.historyLen != twice.historyLen {
		t.Errorf("segment lengths changed on second partition: %+v vs %+v", once, twice)
	}
}

func TestPartition_AllStatic(t *testing.T) {
	msgs := []pipeline.Message{msg("system", "a", false), msg("user", "b", true)}
	seg := partition(msgs)
	if seg.staticLen != 2 || seg.historyLen != 0 || seg.hasNewest {
		t.Fatalf("unexpected segments: %+v", seg)
	}
	// Idempotency must survive the "no newest message" shape too.
	if again := partition(seg.ordered); !reflect.DeepEqual(again.ordered, seg.ordered) {
		t.Errorf("not idempotent for all-static input")
	}
}

// NON-NEGOTIABLE (docs/spec.md §2.4.6): with RetrievedChunks populated, no
// breakpoint may land inside or after the dynamic segment.
func TestProcess_NoBreakpointInDynamicSegment(t *testing.T) {
	req := baseRequest()
	req.NeedsRetrieval = true
	req.RetrievedChunks = []pipeline.Chunk{
		{Content: "chunk A", SourceURL: "a", Similarity: 0.9},
		{Content: "chunk B", SourceURL: "b", Similarity: 0.8},
	}

	got, err := NewAligner().Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process returned error: %v", err)
	}
	if len(got.CacheBreakpoints) == 0 {
		t.Fatalf("expected at least one breakpoint for a request with static content")
	}

	seg := partition(got.Messages)
	forbiddenFrom := seg.dynamicBoundary()
	for _, bp := range got.CacheBreakpoints {
		if bp.AfterMessageIndex >= forbiddenFrom {
			t.Errorf("breakpoint %+v is at/after the dynamic segment (boundary %d) — this poisons the provider cache",
				bp, forbiddenFrom)
		}
		if bp.AfterMessageIndex >= seg.staticLen {
			t.Errorf("breakpoint %+v escaped the static prefix (staticLen=%d)", bp, seg.staticLen)
		}
	}
}

// The property that keeps provider caching effective: identical static+history
// with DIFFERENT dynamic chunks must produce identical breakpoints.
func TestComputeBreakpoints_InvariantToRetrievedChunks(t *testing.T) {
	a := baseRequest()
	a.RetrievedChunks = []pipeline.Chunk{{Content: "turn-1 retrieval"}}

	b := baseRequest()
	b.RetrievedChunks = []pipeline.Chunk{
		{Content: "totally different turn-2 retrieval"},
		{Content: "and another one"},
		{Content: "and a third"},
	}

	c := baseRequest() // no chunks at all

	al := NewAligner()
	ra, _ := al.Process(context.Background(), a)
	rb, _ := al.Process(context.Background(), b)
	rc, _ := al.Process(context.Background(), c)

	if !reflect.DeepEqual(ra.CacheBreakpoints, rb.CacheBreakpoints) {
		t.Errorf("breakpoints differ with different chunks: %v vs %v",
			ra.CacheBreakpoints, rb.CacheBreakpoints)
	}
	if !reflect.DeepEqual(ra.CacheBreakpoints, rc.CacheBreakpoints) {
		t.Errorf("breakpoints differ between chunks and no chunks: %v vs %v",
			ra.CacheBreakpoints, rc.CacheBreakpoints)
	}
	// The static+history prefix itself must also be byte-identical.
	prefix := ra.CacheBreakpoints[0].AfterMessageIndex + 1
	if !reflect.DeepEqual(ra.Messages[:prefix], rb.Messages[:prefix]) {
		t.Errorf("cacheable prefix differs across turns: %v vs %v",
			ra.Messages[:prefix], rb.Messages[:prefix])
	}
}

func TestComputeBreakpoints_Deterministic(t *testing.T) {
	specs := []BreakpointSpec{
		{AfterStaticContent: true, Reason: "static_prefix"},
		{AfterStaticContent: false, Reason: "ignored"},
		{AfterStaticContent: true, Reason: "duplicate_index"},
	}
	var first []pipeline.CacheBreakpoint
	for i := 0; i < 200; i++ {
		req := baseRequest()
		req.RetrievedChunks = []pipeline.Chunk{{Content: "varies"}}
		got := computeBreakpoints(req, specs)
		if i == 0 {
			first = got
			continue
		}
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("run %d differs: %v vs %v", i, got, first)
		}
	}
	if len(first) != 1 {
		t.Errorf("expected duplicate indices deduped and non-static specs ignored, got %v", first)
	}
}

func TestComputeBreakpoints_NilRequest(t *testing.T) {
	if got := computeBreakpoints(nil, DefaultSpecs()); got != nil {
		t.Errorf("computeBreakpoints(nil) = %v, want nil", got)
	}
}

func TestComputeBreakpoints_DefaultReason(t *testing.T) {
	req := &pipeline.Request{Messages: []pipeline.Message{msg("system", "s", false), msg("user", "q", false)}}
	got := computeBreakpoints(req, []BreakpointSpec{{AfterStaticContent: true}})
	if len(got) != 1 || got[0].Reason != "static_prefix" {
		t.Errorf("got %v, want one breakpoint with default reason", got)
	}
}

// Fail-open: no static content -> zero breakpoints, request unchanged, no error.
func TestProcess_FailOpenNoStaticContent(t *testing.T) {
	original := []pipeline.Message{
		msg("user", "a", false),
		msg("assistant", "b", false),
		msg("user", "c", false),
	}
	req := &pipeline.Request{Messages: append([]pipeline.Message(nil), original...)}

	got, err := NewAligner().Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process returned error: %v", err)
	}
	if len(got.CacheBreakpoints) != 0 {
		t.Errorf("expected zero breakpoints, got %v", got.CacheBreakpoints)
	}
	if !reflect.DeepEqual(got.Messages, original) {
		t.Errorf("messages were modified: %v want %v", got.Messages, original)
	}
}

func TestProcess_EmptyAndNilRequests(t *testing.T) {
	if got, err := NewAligner().Process(context.Background(), nil); got != nil || err != nil {
		t.Errorf("Process(nil) = %v, %v", got, err)
	}
	req := &pipeline.Request{}
	got, err := NewAligner().Process(context.Background(), req)
	if err != nil || len(got.CacheBreakpoints) != 0 {
		t.Errorf("empty request: got %v, %v", got.CacheBreakpoints, err)
	}
}

func TestProcess_CanceledContextFailsOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := baseRequest()
	got, err := NewAligner().Process(ctx, req)
	if err != nil {
		t.Fatalf("canceled context must fail open, got error %v", err)
	}
	if len(got.CacheBreakpoints) != 0 {
		t.Errorf("expected no work done on canceled context, got %v", got.CacheBreakpoints)
	}
}

func TestNameAndStageContract(t *testing.T) {
	var s pipeline.Stage = NewAligner()
	if s.Name() != "cache_align" {
		t.Errorf("Name() = %q, want %q", s.Name(), "cache_align")
	}
}

func TestNewAligner_DefaultsAndSpecCopy(t *testing.T) {
	specs := []BreakpointSpec{{AfterStaticContent: true, Reason: "mine"}}
	a := NewAligner(specs...)
	specs[0].Reason = "mutated after construction"

	req := baseRequest()
	got, _ := a.Process(context.Background(), req)
	if len(got.CacheBreakpoints) != 1 || got.CacheBreakpoints[0].Reason != "mine" {
		t.Errorf("caller mutation leaked into the aligner: %v", got.CacheBreakpoints)
	}
}

func TestWithMetrics(t *testing.T) {
	rec := metrics.NewInMemory()
	a := NewAlignerWith(nil, WithMetrics(rec), nil)
	if _, err := a.Process(context.Background(), baseRequest()); err != nil {
		t.Fatal(err)
	}
	if got := rec.Count(MetricBreakpoints); got != 1 {
		t.Errorf("counter %s = %d, want 1", MetricBreakpoints, got)
	}
	// nil recorder must not panic.
	NewAlignerWith(nil, WithMetrics(nil)).Process(context.Background(), baseRequest())
}

func TestConcurrentProcess(t *testing.T) {
	a := NewAligner()
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				req := baseRequest()
				req.RetrievedChunks = []pipeline.Chunk{{Content: "x"}}
				if _, err := a.Process(context.Background(), req); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}
