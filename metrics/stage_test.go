package metrics_test

import (
	"context"
	"errors"
	"testing"

	"agentkit/metrics"
	"agentkit/pipeline"
)

type recordingStage struct {
	name  string
	calls int
	err   error
	panic bool
}

func (s *recordingStage) Name() string { return s.name }
func (s *recordingStage) Process(_ context.Context, r *pipeline.Request) (*pipeline.Request, error) {
	s.calls++
	if s.panic {
		panic("stage panic")
	}
	return r, s.err
}

func TestTimedRecordsLatencyPerStage(t *testing.T) {
	rec := metrics.NewObservability()
	inner := &recordingStage{name: "compress"}
	st := metrics.Timed(inner, rec)

	for range 3 {
		if _, err := st.Process(context.Background(), &pipeline.Request{}); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}

	if inner.calls != 3 {
		t.Errorf("inner ran %d times, want 3", inner.calls)
	}
	byStage := rec.SummaryBy(metrics.StageLatency, "stage")
	if s := byStage["compress"]; s.Count != 3 {
		t.Errorf("latency observations for compress = %d, want 3 (%v)", s.Count, byStage)
	}
}

// The wrapper must be invisible: same name, same errors, same request.
func TestTimedIsTransparent(t *testing.T) {
	rec := metrics.NewObservability()
	sentinel := errors.New("stage failed")
	inner := &recordingStage{name: "history", err: sentinel}
	st := metrics.Timed(inner, rec)

	if st.Name() != "history" {
		t.Errorf("Name = %q, want the wrapped stage's name", st.Name())
	}
	req := &pipeline.Request{Query: "q"}
	got, err := st.Process(context.Background(), req)
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the inner error unchanged", err)
	}
	if got != req {
		t.Error("the request must pass through unchanged")
	}
}

// Wrapping is free when the recorder cannot record histograms: the stage is
// returned as-is, so there is not even an allocation to pay for.
func TestTimedReturnsInnerWhenUnsupported(t *testing.T) {
	inner := &recordingStage{name: "rag"}
	for name, rec := range map[string]metrics.Recorder{
		"counter-only": &counterOnly{},
		"nil":          nil,
		"nop":          metrics.Nop{},
	} {
		t.Run(name, func(t *testing.T) {
			if got := metrics.Timed(inner, rec); got != pipeline.Stage(inner) {
				t.Errorf("Timed wrapped the stage for a recorder that cannot record it")
			}
		})
	}
}

func TestTimedHandlesANilStage(t *testing.T) {
	if got := metrics.Timed(nil, metrics.NewObservability()); got != nil {
		t.Errorf("Timed(nil) = %v, want nil", got)
	}
}

// A caller-supplied stage's panic propagates by design. Instrumentation must
// not quietly change that contract — but it should still record the time spent
// before the panic, which is what an operator needs to see.
func TestTimedDoesNotSwallowAPanicButStillRecords(t *testing.T) {
	rec := metrics.NewObservability()
	st := metrics.Timed(&recordingStage{name: "custom", panic: true}, rec)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic must propagate; instrumentation is not a recover boundary")
			}
		}()
		_, _ = st.Process(context.Background(), &pipeline.Request{})
	}()

	if s := rec.SummaryBy(metrics.StageLatency, "stage")["custom"]; s.Count != 1 {
		t.Errorf("latency was not recorded for the panicking stage: %+v", s)
	}
}

type unnamedStage struct{}

func (unnamedStage) Name() string { panic("Name panic") }
func (unnamedStage) Process(_ context.Context, r *pipeline.Request) (*pipeline.Request, error) {
	return r, nil
}

func TestTimedSurvivesAPanickingName(t *testing.T) {
	rec := metrics.NewObservability()
	st := metrics.Timed(unnamedStage{}, rec)

	if got := st.Name(); got != pipeline.UnnamedStage {
		t.Errorf("Name = %q, want the %q fallback", got, pipeline.UnnamedStage)
	}
	if _, err := st.Process(context.Background(), &pipeline.Request{}); err != nil {
		t.Errorf("Process: %v", err)
	}
}

// A caller may still need the concrete stage back through the decorator.
func TestTimedExposesTheWrappedStage(t *testing.T) {
	inner := &recordingStage{name: "toolcache"}
	st := metrics.Timed(inner, metrics.NewObservability())

	u, ok := st.(interface{ Unwrap() pipeline.Stage })
	if !ok {
		t.Fatal("the decorator should expose Unwrap")
	}
	if u.Unwrap() != pipeline.Stage(inner) {
		t.Error("Unwrap did not return the wrapped stage")
	}
}
