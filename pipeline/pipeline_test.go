package pipeline_test

import (
	"context"
	"errors"
	"testing"

	"agentkit/pipeline"
	"agentkit/providers/mock"
)

func TestRunWithNoStagesCallsClientDirectly(t *testing.T) {
	client := mock.New("m1", "hello")
	p := pipeline.New(client)

	req := &pipeline.Request{Query: "hi"}
	resp, err := p.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Content != "hello" || resp.ModelUsed != "m1" {
		t.Fatalf("unexpected response %+v", resp)
	}
	if client.Calls() != 1 {
		t.Fatalf("want 1 client call, got %d", client.Calls())
	}
	if got := client.LastRequest(); got != req {
		t.Fatalf("request was not passed through unmodified")
	}
}

type stubStage struct {
	name string
	fn   func(*pipeline.Request) (*pipeline.Request, error)
}

func (s stubStage) Name() string { return s.name }
func (s stubStage) Process(_ context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	return s.fn(req)
}

func TestRunStagesInOrder(t *testing.T) {
	client := mock.New("m1", "ok")
	var order []string
	mk := func(n string) stubStage {
		return stubStage{name: n, fn: func(r *pipeline.Request) (*pipeline.Request, error) {
			order = append(order, n)
			return r, nil
		}}
	}
	p := pipeline.New(client, mk("a"), mk("b"), mk("c"))
	if _, err := p.Run(context.Background(), &pipeline.Request{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("stages ran out of order: %v", order)
	}
}

func TestStageErrorAbortsAndWraps(t *testing.T) {
	sentinel := errors.New("boom")
	client := mock.New("m1", "ok")
	p := pipeline.New(client, stubStage{name: "bad", fn: func(*pipeline.Request) (*pipeline.Request, error) {
		return nil, sentinel
	}})

	_, err := p.Run(context.Background(), &pipeline.Request{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped sentinel, got %v", err)
	}
	var se *pipeline.StageError
	if !errors.As(err, &se) || se.Stage != "bad" {
		t.Fatalf("want StageError naming the stage, got %v", err)
	}
	if client.Calls() != 0 {
		t.Fatalf("client must not be called after a stage error")
	}
}

func TestShortCircuitSkipsClientAndRemainingStages(t *testing.T) {
	client := mock.New("m1", "from-llm")
	short := &pipeline.Response{Content: "from-rule", ShortCircuited: true}

	ran := false
	p := pipeline.New(client,
		stubStage{name: "sc", fn: func(r *pipeline.Request) (*pipeline.Request, error) {
			r.SetMeta(pipeline.MetaShortCircuit, short)
			return r, nil
		}},
		stubStage{name: "later", fn: func(r *pipeline.Request) (*pipeline.Request, error) {
			ran = true
			return r, nil
		}},
	)

	resp, err := p.Run(context.Background(), &pipeline.Request{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp != short {
		t.Fatalf("want short-circuit response, got %+v", resp)
	}
	if ran {
		t.Fatal("stages after the short-circuit must not run")
	}
	if client.Calls() != 0 {
		t.Fatal("client must not be called on short-circuit")
	}
}

type stubRouter struct{ pick pipeline.ModelClient }

func (s stubRouter) Route(context.Context, *pipeline.Request) pipeline.RouteDecision {
	return pipeline.RouteDecision{Client: s.pick, Reason: "stub"}
}

func TestRouterOverridesFallbackClient(t *testing.T) {
	fallback := mock.New("fallback", "no")
	chosen := mock.New("chosen", "yes")

	p := pipeline.NewWithRouter(fallback, stubRouter{pick: chosen})
	resp, err := p.Run(context.Background(), &pipeline.Request{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Content != "yes" {
		t.Fatalf("router decision ignored: %+v", resp)
	}
	if fallback.Calls() != 0 {
		t.Fatal("fallback must not be called when the router picks a client")
	}
}

func TestNilRouteDecisionFallsBackOpen(t *testing.T) {
	fallback := mock.New("fallback", "yes")
	p := pipeline.NewWithRouter(fallback, stubRouter{pick: nil})
	resp, err := p.Run(context.Background(), &pipeline.Request{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Content != "yes" {
		t.Fatalf("want fallback client, got %+v", resp)
	}
}

func TestCanceledContextStopsBeforeStages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := mock.New("m1", "ok")
	p := pipeline.New(client, stubStage{name: "s", fn: func(r *pipeline.Request) (*pipeline.Request, error) {
		t.Fatal("stage must not run on a canceled context")
		return r, nil
	}})
	if _, err := p.Run(ctx, &pipeline.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestTurnTypeString(t *testing.T) {
	cases := map[pipeline.TurnType]string{
		pipeline.TurnNewQuestion:   "new_question",
		pipeline.TurnRoutineResume: "routine_resume",
		pipeline.TurnErrorRecovery: "error_recovery",
		pipeline.TurnUnknown:       "unknown",
		pipeline.TurnType(99):      "unknown",
	}
	for tt, want := range cases {
		if got := tt.String(); got != want {
			t.Errorf("TurnType(%d).String() = %q, want %q", tt, got, want)
		}
	}
}

func TestStageErrorFormatsAndUnwraps(t *testing.T) {
	inner := errors.New("inner")
	err := &pipeline.StageError{Stage: "compress", Err: inner}

	if got := err.Error(); got != "compress: inner" {
		t.Errorf("Error() = %q", got)
	}
	if !errors.Is(err, inner) {
		t.Error("StageError must unwrap to its cause")
	}
}

func TestBadShortCircuitValueIsAnErrorNotAPanic(t *testing.T) {
	client := mock.New("m", "ok")
	p := pipeline.New(client, stubStage{name: "bad", fn: func(r *pipeline.Request) (*pipeline.Request, error) {
		r.SetMeta(pipeline.MetaShortCircuit, "not a response")
		return r, nil
	}})

	_, err := p.Run(context.Background(), &pipeline.Request{})
	if err == nil {
		t.Fatal("a non-*Response short-circuit value must be reported, not ignored")
	}
	var se *pipeline.StageError
	if !errors.As(err, &se) || se.Stage != "bad" {
		t.Fatalf("want a StageError naming the stage, got %v", err)
	}
	if client.Calls() != 0 {
		t.Error("client must not be called after a malformed short-circuit")
	}
}

func TestStageReturningNilRequestKeepsThePrevious(t *testing.T) {
	client := mock.New("m", "ok")
	req := &pipeline.Request{Query: "keep me"}
	p := pipeline.New(client, stubStage{name: "nil", fn: func(*pipeline.Request) (*pipeline.Request, error) {
		return nil, nil
	}})

	if _, err := p.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := client.LastRequest(); got != req {
		t.Fatal("a stage returning nil must not lose the request")
	}
}
