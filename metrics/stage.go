package metrics

import (
	"context"
	"time"

	"agentkit/pipeline"
)

// Timed wraps a stage so its Process duration is recorded, labelled by stage
// name. It answers the question a synthetic benchmark cannot: is this pipeline
// paying for itself on real traffic?
//
// The wrapper is transparent — same Name, same behaviour, same errors, same
// returned Request. It adds one time.Since per stage per turn.
//
// Returns inner unchanged when rec cannot record histograms, so wrapping is
// free for callers who have not opted into them.
func Timed(inner pipeline.Stage, rec Recorder) pipeline.Stage {
	if inner == nil {
		return nil
	}
	if !SupportsHistograms(rec) {
		return inner
	}
	return &timedStage{inner: inner, rec: Or(rec)}
}

// SupportsHistograms reports whether rec will actually record observations.
// Useful to skip building instrumentation a recorder would discard.
func SupportsHistograms(rec Recorder) bool {
	hr, ok := Or(rec).(HistogramRecorder)
	return ok && hr.Histogram(StageLatency) != nil
}

type timedStage struct {
	inner pipeline.Stage
	rec   Recorder
}

// Name reports the wrapped stage's name, so metrics and StageError text keep
// naming the real stage rather than the wrapper.
func (t *timedStage) Name() string {
	return safeStageName(t.inner)
}

func (t *timedStage) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	start := time.Now()
	// No recover here on purpose: a caller-supplied Stage's panic propagates by
	// design (see agentkit package docs), and instrumentation must not quietly
	// change that contract. The deferred observation still records the time
	// spent before the panic, which is exactly what an operator wants.
	defer func() {
		ObserveDuration(t.rec, StageLatency, time.Since(start),
			map[string]string{"stage": t.Name()})
	}()
	return t.inner.Process(ctx, req)
}

// Unwrap exposes the wrapped stage, so a caller can still type-assert to a
// concrete stage type through the decorator.
func (t *timedStage) Unwrap() pipeline.Stage { return t.inner }

func safeStageName(s pipeline.Stage) (name string) {
	defer func() {
		if recover() != nil {
			name = pipeline.UnnamedStage
		}
	}()
	if n := s.Name(); n != "" {
		return n
	}
	return pipeline.UnnamedStage
}
