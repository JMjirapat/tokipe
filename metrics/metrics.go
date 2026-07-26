// Package metrics defines the minimal, provider-agnostic counter interface
// stages use to report what they did. The default Recorder is a no-op, so
// metrics are always opt-in and never a hard dependency.
package metrics

import (
	"maps"
	"sync"
)

type Counter interface {
	Inc(labels map[string]string)
}

type Recorder interface {
	Counter(name string) Counter
}

// Nop is a Recorder that discards everything. It satisfies the optional
// interfaces too, so a caller can pass it anywhere without capability checks.
type Nop struct{}

func (Nop) Counter(string) Counter     { return nopCounter{} }
func (Nop) Histogram(string) Histogram { return nil }
func (Nop) Gauge(string) Gauge         { return nil }
func (Nop) Degraded(Degradation)       {}

type nopCounter struct{}

func (nopCounter) Inc(map[string]string) {}

// Or returns r wrapped so that it can never break a turn, or a no-op Recorder
// when r is nil. Every stage constructor funnels its recorder through this.
//
// The wrapping is what makes "metrics are opt-in and never a hard dependency"
// true rather than aspirational. A Recorder is caller-supplied code called on
// the request path, so Counter and Inc get the same treatment as every other
// extension point: a panic is contained, and a nil Counter degrades to a
// no-op. Diagnostics must never be able to discard a result the pipeline has
// already produced.
//
// Guarding here rather than only in Inc means direct `rec.Counter(n).Inc(l)`
// call sites are covered too, without each one having to remember.
func Or(r Recorder) Recorder {
	switch r.(type) {
	case nil:
		return Nop{}
	case Nop, safeRecorder:
		return r // already harmless; do not wrap twice
	}
	return safeRecorder{inner: r}
}

// Inc is a nil-safe convenience: metrics.Inc(rec, "cache_hit", labels).
func Inc(r Recorder, name string, labels map[string]string) {
	Or(r).Counter(name).Inc(labels)
}

// safeRecorder contains panics from a caller-supplied Recorder.
//
// It implements every optional interface — histograms, gauges, degradation —
// and forwards each only if the wrapped recorder actually supports it. That is
// load-bearing: a wrapper implementing only Recorder would HIDE the optional
// interfaces behind itself, so every type assertion in Observe, Set and Degrade
// would fail and each observation would be silently dropped. Wrapping for safety
// must not cost capability.
type safeRecorder struct{ inner Recorder }

var (
	_ HistogramRecorder   = safeRecorder{}
	_ GaugeRecorder       = safeRecorder{}
	_ DegradationReporter = safeRecorder{}
)

func (s safeRecorder) Counter(name string) (c Counter) {
	defer func() {
		if recover() != nil {
			c = nopCounter{}
		}
	}()
	if got := s.inner.Counter(name); got != nil {
		return safeCounter{inner: got}
	}
	return nopCounter{}
}

// Histogram forwards to the wrapped recorder, or returns nil when it does not
// support histograms. Observe treats nil as "not supported" and drops the value.
func (s safeRecorder) Histogram(name string) Histogram {
	hr, ok := s.inner.(HistogramRecorder)
	if !ok {
		return nil
	}
	h := safeHistogram(hr, name)
	if h == nil {
		return nil
	}
	return safeHistogramWrapper{inner: h}
}

// Gauge forwards to the wrapped recorder, or returns nil when unsupported.
func (s safeRecorder) Gauge(name string) Gauge {
	gr, ok := s.inner.(GaugeRecorder)
	if !ok {
		return nil
	}
	g := safeGauge(gr, name)
	if g == nil {
		return nil
	}
	return safeGaugeWrapper{inner: g}
}

// Degraded forwards to the wrapped recorder when it reports degradations, and
// otherwise does nothing. Reporting is never required.
func (s safeRecorder) Degraded(d Degradation) {
	dr, ok := s.inner.(DegradationReporter)
	if !ok {
		return
	}
	guard(func() { dr.Degraded(d) })
}

type safeHistogramWrapper struct{ inner Histogram }

func (s safeHistogramWrapper) Observe(v float64, labels map[string]string) {
	guard(func() { s.inner.Observe(v, labels) })
}

type safeGaugeWrapper struct{ inner Gauge }

func (s safeGaugeWrapper) Set(v float64, labels map[string]string) {
	guard(func() { s.inner.Set(v, labels) })
}

// safeCounter contains panics from a caller-supplied Counter.
type safeCounter struct{ inner Counter }

func (s safeCounter) Inc(labels map[string]string) {
	defer func() { _ = recover() }()
	s.inner.Inc(labels)
}

// InMemory is a Recorder that tallies counts in memory. Intended for tests and
// examples; safe for concurrent use.
type InMemory struct {
	mu     sync.Mutex
	counts map[string]int
}

func NewInMemory() *InMemory { return &InMemory{counts: map[string]int{}} }

func (m *InMemory) Counter(name string) Counter { return &memCounter{rec: m, name: name} }

// Count returns the total increments recorded for name across all label sets.
func (m *InMemory) Count(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[name]
}

// Snapshot returns a copy of all counter totals.
func (m *InMemory) Snapshot() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.counts))
	maps.Copy(out, m.counts)
	return out
}

type memCounter struct {
	rec  *InMemory
	name string
}

func (c *memCounter) Inc(map[string]string) {
	c.rec.mu.Lock()
	defer c.rec.mu.Unlock()
	if c.rec.counts == nil {
		c.rec.counts = map[string]int{}
	}
	c.rec.counts[c.name]++
}
