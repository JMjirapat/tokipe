package pipeline_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/providers/mock"
)

func drain(t *testing.T, seq iter.Seq2[pipeline.Delta, error]) (texts []string, resp *pipeline.Response, err error) {
	t.Helper()
	resp, err = pipeline.Collect(collectTexts(seq, &texts))
	return texts, resp, err
}

// collectTexts wraps a sequence to record each delta's text as it passes, so a
// test can assert on the increments and on the accumulated result at once.
func collectTexts(seq iter.Seq2[pipeline.Delta, error], out *[]string) iter.Seq2[pipeline.Delta, error] {
	return func(yield func(pipeline.Delta, error) bool) {
		for d, err := range seq {
			if d.Text != "" {
				*out = append(*out, d.Text)
			}
			if !yield(d, err) {
				return
			}
		}
	}
}

func TestRunStreamYieldsIncrementsAndAccumulates(t *testing.T) {
	client := mock.NewStream("m", "", "Hello", ", ", "world")
	client.Response.Usage = pipeline.Usage{InputTokens: 10, OutputTokens: 3}

	seq, err := pipeline.New(client).RunStream(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	texts, resp, err := drain(t, seq)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := strings.Join(texts, "|"); got != "Hello|, |world" {
		t.Errorf("deltas = %q, want three separate increments", got)
	}
	if resp.Content != "Hello, world" {
		t.Errorf("Content = %q, want the concatenation", resp.Content)
	}
	if resp.ModelUsed != "m" {
		t.Errorf("ModelUsed = %q", resp.ModelUsed)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want the final delta's usage", resp.Usage)
	}
	if client.StreamCalls() != 1 || client.Calls() != 0 {
		t.Errorf("stream calls=%d send calls=%d; want the streaming path only",
			client.StreamCalls(), client.Calls())
	}
}

// The point of StreamOne: a caller must not have to ask whether its client can
// stream. A plain ModelClient still works through RunStream.
func TestRunStreamAdaptsANonStreamingClient(t *testing.T) {
	client := mock.New("plain", "one shot answer")

	seq, err := pipeline.New(client).RunStream(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	texts, resp, err := drain(t, seq)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(texts) != 1 {
		t.Errorf("want exactly one delta from a non-streaming client, got %d", len(texts))
	}
	if resp.Content != "one shot answer" || resp.ModelUsed != "plain" {
		t.Errorf("resp = %+v", resp)
	}
	if client.Calls() != 1 {
		t.Errorf("Send calls = %d, want 1", client.Calls())
	}
}

// A short-circuited turn must look like any other stream: one delta, no model.
func TestRunStreamShortCircuitYieldsOneDelta(t *testing.T) {
	client := mock.NewStream("m", "from model")
	short := &pipeline.Response{Content: "from rule", ShortCircuited: true, ModelUsed: "none"}

	p := pipeline.New(client, stubStage{name: "sc", fn: func(r *pipeline.Request) (*pipeline.Request, error) {
		r.SetMeta(pipeline.MetaShortCircuit, short)
		return r, nil
	}})

	seq, err := p.RunStream(context.Background(), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	texts, resp, err := drain(t, seq)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(texts) != 1 || texts[0] != "from rule" {
		t.Errorf("deltas = %v, want the rule's single answer", texts)
	}
	if resp.Content != "from rule" {
		t.Errorf("Content = %q", resp.Content)
	}
	if client.StreamCalls() != 0 || client.Calls() != 0 {
		t.Error("a short-circuit must not reach the model at all")
	}
}

func TestRunStreamRunsStagesInOrderBeforeStreaming(t *testing.T) {
	var order []string
	mk := func(n string) stubStage {
		return stubStage{name: n, fn: func(r *pipeline.Request) (*pipeline.Request, error) {
			order = append(order, n)
			return r, nil
		}}
	}
	p := pipeline.New(mock.NewStream("m", "ok"), mk("a"), mk("b"), mk("c"))

	seq, err := p.RunStream(context.Background(), &pipeline.Request{})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if len(order) != 3 || order[0] != "a" || order[2] != "c" {
		t.Fatalf("stages ran out of order or late: %v", order)
	}
	if _, _, err := drain(t, seq); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func TestRunStreamStageErrorAbortsBeforeAnyDelta(t *testing.T) {
	sentinel := errors.New("boom")
	client := mock.NewStream("m", "ok")
	p := pipeline.New(client, stubStage{name: "bad", fn: func(*pipeline.Request) (*pipeline.Request, error) {
		return nil, sentinel
	}})

	seq, err := p.RunStream(context.Background(), &pipeline.Request{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the wrapped sentinel", err)
	}
	if seq != nil {
		t.Error("no sequence should be returned when a stage failed")
	}
	if client.StreamCalls() != 0 {
		t.Error("the client must not be reached after a stage error")
	}
}

// A mid-stream failure is yielded, not returned, and whatever arrived first is
// still available — a partial answer often beats none.
func TestMidStreamErrorPreservesPartialContent(t *testing.T) {
	sentinel := errors.New("connection dropped")
	client := mock.NewStream("m", "", "partial ", "answer")
	client.Err = sentinel

	seq, err := pipeline.New(client).RunStream(context.Background(), &pipeline.Request{})
	if err != nil {
		t.Fatalf("RunStream should not fail up front: %v", err)
	}
	_, resp, err := drain(t, seq)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the mid-stream error", err)
	}
	if resp.Content != "partial answer" {
		t.Errorf("Content = %q, want what arrived before the failure", resp.Content)
	}
}

func TestSendStreamErrorSurfacesUpFront(t *testing.T) {
	sentinel := errors.New("rejected")
	client := mock.NewStream("m", "ok")
	client.StreamErr = sentinel

	seq, err := pipeline.New(client).RunStream(context.Background(), &pipeline.Request{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the pre-stream error", err)
	}
	if seq != nil {
		t.Error("no sequence when SendStream itself failed")
	}
}

func TestRunStreamRoutesLikeRun(t *testing.T) {
	fallback := mock.NewStream("fallback", "no")
	chosen := mock.NewStream("chosen", "yes")

	p := pipeline.NewWithRouter(fallback, stubRouter{pick: chosen})
	seq, err := p.RunStream(context.Background(), &pipeline.Request{})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if _, resp, err := drain(t, seq); err != nil || resp.Content != "yes" {
		t.Fatalf("resp=%+v err=%v, want the routed client", resp, err)
	}
	if fallback.StreamCalls() != 0 {
		t.Error("fallback must not stream when the router picked another client")
	}
}

func TestRunStreamCanceledContextStopsBeforeStages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := pipeline.New(mock.NewStream("m", "ok"), stubStage{name: "s", fn: func(r *pipeline.Request) (*pipeline.Request, error) {
		t.Fatal("stage must not run on a canceled context")
		return r, nil
	}})
	if _, err := p.RunStream(ctx, &pipeline.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Abandoning the range loop must not panic or leak into later use.
func TestConsumerMayStopEarly(t *testing.T) {
	client := mock.NewStream("m", "", "a", "b", "c", "d")

	seq, err := pipeline.New(client).RunStream(context.Background(), &pipeline.Request{})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	seen := 0
	for range seq {
		seen++
		if seen == 2 {
			break
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d deltas, want to have stopped at 2", seen)
	}
}

func TestCollectHandlesNilAndEmpty(t *testing.T) {
	resp, err := pipeline.Collect(nil)
	if err != nil || resp == nil || resp.Content != "" {
		t.Fatalf("Collect(nil) = %+v, %v; want an empty response and no error", resp, err)
	}

	empty := func(yield func(pipeline.Delta, error) bool) {}
	if resp, err = pipeline.Collect(empty); err != nil || resp.Content != "" {
		t.Fatalf("Collect(empty) = %+v, %v", resp, err)
	}
}

func TestStreamOneOnNilResponseYieldsNothing(t *testing.T) {
	count := 0
	for range pipeline.StreamOne(nil) {
		count++
	}
	if count != 0 {
		t.Errorf("yielded %d deltas for a nil response, want 0", count)
	}
}

// Run and RunStream share prepare, so they must agree on the shaped request.
// This is the property that keeps the two paths from drifting.
func TestRunAndRunStreamShapeTheRequestIdentically(t *testing.T) {
	newReq := func() *pipeline.Request {
		return &pipeline.Request{
			Query:    "q",
			Messages: []pipeline.Message{{Role: "system", Content: "sys", Static: true}},
		}
	}
	marker := stubStage{name: "marker", fn: func(r *pipeline.Request) (*pipeline.Request, error) {
		r.SetMeta("stage.ran", true)
		return r, nil
	}}

	a := mock.New("a", "x")
	if _, err := pipeline.New(a, marker).Run(context.Background(), newReq()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	b := mock.NewStream("b", "x")
	seq, err := pipeline.New(b, marker).RunStream(context.Background(), newReq())
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if _, _, err := drain(t, seq); err != nil {
		t.Fatalf("drain: %v", err)
	}

	ra, rb := a.LastRequest(), b.LastRequest()
	if ra == nil || rb == nil {
		t.Fatal("both paths must reach a client")
	}
	if ra.Metadata["stage.ran"] != true || rb.Metadata["stage.ran"] != true {
		t.Error("both paths must run the stages")
	}
	if ra.Query != rb.Query || len(ra.Messages) != len(rb.Messages) {
		t.Errorf("paths disagree on the shaped request:\n Run: %+v\n RunStream: %+v", ra, rb)
	}
}
