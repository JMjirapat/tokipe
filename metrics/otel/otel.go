// Package otel adapts agentkit's metrics interfaces to OpenTelemetry.
//
// It is a separate Go module (see go.mod in this directory) for the same reason
// stores/pgvector and toolcache/redis are: the agentkit core is stdlib-only by
// contract (spec §2.6), and the OTel SDK is a large dependency tree. A caller
// who wants OTel opts into that tree; everyone else never sees it.
//
// # What maps to what
//
//	metrics.Counter    → an OTel Int64Counter
//	metrics.Histogram  → an OTel Float64Histogram
//	metrics.Gauge      → an OTel Float64Gauge
//	metrics.Degradation → a counter increment plus an optional callback
//
// Instruments are created lazily and memoised, because OTel instrument creation
// is not free and agentkit asks for the same handful of names on every turn.
package otel

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	akmetrics "github.com/JMjirapat/tokipe/metrics"
)

// Recorder implements agentkit's Recorder plus every optional interface.
type Recorder struct {
	meter metric.Meter

	// onDegraded, if set, is called for each degradation in addition to the
	// counter. OTel has no first-class "event" for this, and inventing one out
	// of a log bridge would make this package choose a logger for its host —
	// which the metrics contract explicitly refuses to do.
	onDegraded func(akmetrics.Degradation)

	mu         sync.RWMutex
	counters   map[string]metric.Int64Counter
	histograms map[string]metric.Float64Histogram
	gauges     map[string]metric.Float64Gauge
}

var (
	_ akmetrics.Recorder            = (*Recorder)(nil)
	_ akmetrics.HistogramRecorder   = (*Recorder)(nil)
	_ akmetrics.GaugeRecorder       = (*Recorder)(nil)
	_ akmetrics.DegradationReporter = (*Recorder)(nil)
)

// Option configures a Recorder.
type Option func(*Recorder)

// WithDegradationHandler sets a callback invoked for every degradation, in
// addition to the counter. Use it to bridge degradations into logs or traces —
// the destination is the host's choice, not this package's.
//
// It runs on the request path, synchronously. Do not block in it.
func WithDegradationHandler(fn func(akmetrics.Degradation)) Option {
	return func(r *Recorder) { r.onDegraded = fn }
}

// New returns a Recorder backed by meter.
func New(meter metric.Meter, opts ...Option) *Recorder {
	r := &Recorder{
		meter:      meter,
		counters:   map[string]metric.Int64Counter{},
		histograms: map[string]metric.Float64Histogram{},
		gauges:     map[string]metric.Float64Gauge{},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

func (r *Recorder) Counter(name string) akmetrics.Counter {
	r.mu.RLock()
	c, ok := r.counters[name]
	r.mu.RUnlock()
	if ok {
		return &counter{inst: c}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return &counter{inst: c}
	}
	inst, err := r.meter.Int64Counter(name)
	if err != nil {
		// An instrument agentkit cannot create is a metric it silently drops,
		// never a failed turn. Returning nil lets metrics.Or substitute a no-op.
		return nil
	}
	r.counters[name] = inst
	return &counter{inst: inst}
}

func (r *Recorder) Histogram(name string) akmetrics.Histogram {
	r.mu.RLock()
	h, ok := r.histograms[name]
	r.mu.RUnlock()
	if ok {
		return &histogram{inst: h}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.histograms[name]; ok {
		return &histogram{inst: h}
	}
	inst, err := r.meter.Float64Histogram(name)
	if err != nil {
		return nil
	}
	r.histograms[name] = inst
	return &histogram{inst: inst}
}

func (r *Recorder) Gauge(name string) akmetrics.Gauge {
	r.mu.RLock()
	g, ok := r.gauges[name]
	r.mu.RUnlock()
	if ok {
		return &gauge{inst: g}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return &gauge{inst: g}
	}
	inst, err := r.meter.Float64Gauge(name)
	if err != nil {
		return nil
	}
	r.gauges[name] = inst
	return &gauge{inst: inst}
}

// Degraded records the degradation's stage and reason as attributes on a
// dedicated counter, and calls the handler if one was configured.
//
// Err is deliberately NOT an attribute: error strings are unbounded and
// high-cardinality, and a metrics backend is the wrong place to put them. It
// reaches the handler instead, where a logger can take it.
func (r *Recorder) Degraded(d akmetrics.Degradation) {
	if c := r.Counter(akmetrics.StageDegraded); c != nil {
		c.Inc(map[string]string{"stage": d.Stage, "reason": d.Reason})
	}
	if r.onDegraded != nil {
		r.onDegraded(d)
	}
}

// --- instruments ------------------------------------------------------------

type counter struct{ inst metric.Int64Counter }

func (c *counter) Inc(labels map[string]string) {
	c.inst.Add(context.Background(), 1, metric.WithAttributes(attrs(labels)...))
}

type histogram struct{ inst metric.Float64Histogram }

func (h *histogram) Observe(v float64, labels map[string]string) {
	h.inst.Record(context.Background(), v, metric.WithAttributes(attrs(labels)...))
}

type gauge struct{ inst metric.Float64Gauge }

func (g *gauge) Set(v float64, labels map[string]string) {
	g.inst.Record(context.Background(), v, metric.WithAttributes(attrs(labels)...))
}

// attrs converts agentkit's label map into OTel attributes.
//
// agentkit's own labels are all low-cardinality by design — stage names, tool
// names, reasons — which is what makes them safe as metric dimensions. A caller
// passing a request id here would blow up their backend's cardinality, so that
// is worth knowing rather than guarding against: this package cannot tell the
// difference, and silently dropping labels would be worse.
func attrs(labels map[string]string) []attribute.KeyValue {
	if len(labels) == 0 {
		return nil
	}
	out := make([]attribute.KeyValue, 0, len(labels))
	for k, v := range labels {
		out = append(out, attribute.String(k, v))
	}
	return out
}
