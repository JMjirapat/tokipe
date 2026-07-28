package pipeline_test

import (
	"context"
	"errors"
	"testing"

	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/providers/mock"
)

// markerStage records that it ran and appends to the query, so a test can tell
// whether Prepare actually ran the stages or merely returned the input.
type markerStage struct {
	name string
	mark string
	ran  *bool
}

func (m markerStage) Name() string { return m.name }

func (m markerStage) Process(_ context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	if m.ran != nil {
		*m.ran = true
	}
	req.Query += m.mark
	return req, nil
}

// The whole point of Prepare: every stage runs, and the model is not called.
// A caller that intends to send the request itself must not be billed for a
// send it did not ask for.
func TestPrepareShapesTheRequestWithoutSending(t *testing.T) {
	client := mock.New("model", "should not be used")
	ran := false
	p := pipeline.New(client, markerStage{name: "marker", mark: "!", ran: &ran})

	prep, err := p.Prepare(context.Background(), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if !ran {
		t.Error("the stage did not run")
	}
	if prep.Request.Query != "q!" {
		t.Errorf("query = %q, want the stage's change applied", prep.Request.Query)
	}
	if client.Calls() != 0 {
		t.Errorf("the model was called %d times; Prepare must not send", client.Calls())
	}
	if prep.ShortCircuited() {
		t.Error("nothing short-circuited, but ShortCircuited() reports true")
	}
	if prep.Client == nil {
		t.Error("Client is nil; the caller has nothing to send with")
	}
}

// Prepare must hand back the routing decision, so a caller doing its own send
// still learns which model the pipeline would have chosen.
func TestPrepareReportsTheRoutedClient(t *testing.T) {
	cheap := mock.New("cheap", "")
	strong := mock.New("strong", "")

	p := pipeline.NewWithRouter(strong, routerFunc(func(context.Context, *pipeline.Request) pipeline.RouteDecision {
		return pipeline.RouteDecision{Client: cheap, Reason: "test"}
	}))

	prep, err := p.Prepare(context.Background(), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.Client != cheap {
		t.Errorf("Client = %v, want the router's choice", prep.Client.Name())
	}
	if cheap.Calls() != 0 || strong.Calls() != 0 {
		t.Error("Prepare called a model")
	}
}

// A short circuit leaves nothing to send. Client must be nil rather than a
// client the caller might use to send a request that was already answered.
func TestPrepareSurfacesAShortCircuit(t *testing.T) {
	client := mock.New("model", "from model")
	answer := &pipeline.Response{Content: "answered", ShortCircuited: true}

	p := pipeline.New(client, stageFn(func(_ context.Context, req *pipeline.Request) (*pipeline.Request, error) {
		req.SetMeta(pipeline.MetaShortCircuit, answer)
		return req, nil
	}))

	prep, err := p.Prepare(context.Background(), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !prep.ShortCircuited() {
		t.Fatal("ShortCircuited() = false for a short-circuited turn")
	}
	if prep.Response != answer {
		t.Errorf("Response = %+v, want the stage's answer", prep.Response)
	}
	if prep.Client != nil {
		t.Error("Client is set for a turn that needs no send")
	}
}

// Run and Prepare share one implementation; this pins the property that makes
// sharing worthwhile — what Run sends is what Prepare returns.
func TestRunSendsExactlyWhatPrepareReturns(t *testing.T) {
	stages := func() []pipeline.Stage {
		return []pipeline.Stage{markerStage{name: "a", mark: "-a"}, markerStage{name: "b", mark: "-b"}}
	}

	prepClient := mock.New("model", "")
	prep, err := pipeline.New(prepClient, stages()...).
		Prepare(context.Background(), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	runClient := mock.New("model", "")
	if _, err := pipeline.New(runClient, stages()...).
		Run(context.Background(), &pipeline.Request{Query: "q"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if runClient.Calls() != 1 {
		t.Fatalf("Run made %d model calls, want 1", runClient.Calls())
	}
	if sent := runClient.LastRequest(); sent.Query != prep.Request.Query {
		t.Errorf("Run sent %q but Prepare returned %q", sent.Query, prep.Request.Query)
	}
}

// A stage error is the caller's to see, not something to paper over with a
// half-shaped request.
func TestPrepareReturnsStageErrors(t *testing.T) {
	boom := errors.New("boom")
	p := pipeline.New(mock.New("model", ""), stageFn(func(context.Context, *pipeline.Request) (*pipeline.Request, error) {
		return nil, boom
	}))

	prep, err := p.Prepare(context.Background(), &pipeline.Request{Query: "q"})
	if err == nil {
		t.Fatal("want an error, got none")
	}
	if prep != nil {
		t.Errorf("want a nil Prepared alongside the error, got %+v", prep)
	}
	var se *pipeline.StageError
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a *StageError", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error does not wrap the stage's own error")
	}
}

func TestPrepareRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ran := false
	p := pipeline.New(mock.New("model", ""), markerStage{name: "marker", mark: "!", ran: &ran})

	if _, err := p.Prepare(ctx, &pipeline.Request{Query: "q"}); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if ran {
		t.Error("a stage ran after the context was already cancelled")
	}
}

// ShortCircuited must be safe on a nil receiver: it is the first thing a caller
// reaches for, often before checking the error.
func TestShortCircuitedOnNil(t *testing.T) {
	var prep *pipeline.Prepared
	if prep.ShortCircuited() {
		t.Error("(*Prepared)(nil).ShortCircuited() = true, want false")
	}
}

type stageFn func(context.Context, *pipeline.Request) (*pipeline.Request, error)

func (f stageFn) Name() string { return "stage_fn" }
func (f stageFn) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	return f(ctx, req)
}

type routerFunc func(context.Context, *pipeline.Request) pipeline.RouteDecision

func (f routerFunc) Route(ctx context.Context, req *pipeline.Request) pipeline.RouteDecision {
	return f(ctx, req)
}
