package metrics

import "time"

// Histogram records a distribution of values — stage latency, tokens per turn,
// bytes saved by compression.
type Histogram interface {
	Observe(value float64, labels map[string]string)
}

// Gauge records a value that goes up and down — cache hit rate, in-flight
// requests, current context size.
type Gauge interface {
	Set(value float64, labels map[string]string)
}

// HistogramRecorder is an OPTIONAL interface a Recorder may also implement.
//
// It is separate rather than a method on Recorder because Recorder is the
// interface callers implement, and adding a method would break every existing
// implementation — including the one in this repository's own examples. The
// same reasoning that kept ModelClient frozen when streaming arrived applies
// here: an optional interface costs nothing to ignore.
type HistogramRecorder interface {
	Recorder
	Histogram(name string) Histogram
}

// GaugeRecorder is the optional gauge counterpart to HistogramRecorder.
type GaugeRecorder interface {
	Recorder
	Gauge(name string) Gauge
}

// Observe records value against name, if r supports histograms. A Recorder that
// does not is not an error — the observation is simply dropped, which is the
// same contract as a Nop counter.
func Observe(r Recorder, name string, value float64, labels map[string]string) {
	hr, ok := Or(r).(HistogramRecorder)
	if !ok {
		return
	}
	// A nil Histogram means the underlying recorder does not support them.
	if h := hr.Histogram(name); h != nil {
		guard(func() { h.Observe(value, labels) })
	}
}

// ObserveDuration is Observe for a latency, recorded in milliseconds.
//
// Milliseconds rather than seconds because every stage in this library operates
// on that scale, and a histogram of values around 0.0004 is harder to read in
// every backend that will consume it.
func ObserveDuration(r Recorder, name string, d time.Duration, labels map[string]string) {
	Observe(r, name, float64(d.Nanoseconds())/1e6, labels)
}

// Set records value against name, if r supports gauges.
func Set(r Recorder, name string, value float64, labels map[string]string) {
	gr, ok := Or(r).(GaugeRecorder)
	if !ok {
		return
	}
	if g := gr.Gauge(name); g != nil {
		guard(func() { g.Set(value, labels) })
	}
}

// Metric names emitted by tokipe itself. Callers are free to add their own;
// these are the ones the library guarantees.
const (
	// StageLatency is the wall time one stage's Process took, labelled by
	// stage. Includes the time a stage spent in a caller's own code.
	StageLatency = "tokipe.stage_latency_ms"

	// StageDegraded counts fail-open events, labelled by stage and reason.
	// This is the counter that makes silent degradation visible.
	StageDegraded = "tokipe.stage_degraded"
)

// safeHistogram and safeGauge contain a panic from a caller-supplied recorder,
// exactly as safeRecorder does for counters. Diagnostics must never be able to
// break a turn.
func safeHistogram(r HistogramRecorder, name string) (h Histogram) {
	defer func() {
		if recover() != nil {
			h = nil
		}
	}()
	return r.Histogram(name)
}

func safeGauge(r GaugeRecorder, name string) (g Gauge) {
	defer func() {
		if recover() != nil {
			g = nil
		}
	}()
	return r.Gauge(name)
}

func guard(fn func()) {
	defer func() { _ = recover() }()
	fn()
}
