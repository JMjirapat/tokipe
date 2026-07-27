package metrics

import (
	"fmt"
	"sync"
)

// Degradation reports that an optimization gave up without failing the turn.
//
// This is the other half of the fail-open guarantee. Fail-open is right for
// availability and wrong for operations: today a dead cache backend silently
// degrades every request and nothing says so. The turn must still succeed — but
// somebody has to be able to find out.
type Degradation struct {
	// Stage is the stage that degraded, e.g. "toolcache".
	Stage string

	// Reason is a short, stable, machine-groupable cause — "cache_get_failed",
	// "embed_failed", "counter_panicked". Stable because it will end up as a
	// metric label and in alert rules; a message that varies per occurrence
	// belongs in Err.
	Reason string

	// Err is the underlying cause, when there was one. A degradation can be
	// legitimate without an error — "nothing left to trim" is a decision, not
	// a failure — so this may be nil.
	Err error

	// Detail is optional free text for a human reading a log. Never used for
	// grouping.
	Detail string
}

func (d Degradation) String() string {
	s := d.Stage + ": " + d.Reason
	if d.Detail != "" {
		s += " (" + d.Detail + ")"
	}
	if d.Err != nil {
		s += ": " + d.Err.Error()
	}
	return s
}

// DegradationReporter is an OPTIONAL interface a Recorder may also implement.
//
// Attaching it to the Recorder rather than inventing a second sink is
// deliberate: one object, one wiring point, one place for a host to decide what
// to do. The library still chooses nothing — no logger, no format, no
// destination. It hands over a struct and the host decides.
type DegradationReporter interface {
	Recorder

	// Degraded is called on the request path, synchronously. An implementation
	// that blocks will slow every degraded turn, so buffer or drop rather than
	// doing I/O here.
	Degraded(Degradation)
}

// Degrade reports a degradation and counts it.
//
// Always call this rather than a bare counter at a fail-open site: the counter
// answers "how often", the report answers "why, and what was the error". Both
// matter, and a caller who wires only a counter still gets the count.
func Degrade(r Recorder, d Degradation) {
	rec := Or(r)

	labels := map[string]string{"stage": d.Stage, "reason": d.Reason}
	rec.Counter(StageDegraded).Inc(labels)

	dr, ok := rec.(DegradationReporter)
	if !ok {
		return
	}
	guard(func() { dr.Degraded(d) })
}

// DegradeFunc adapts a function into the reporter half of a Recorder.
//
// It wraps an existing Recorder so counters keep working:
//
//	rec := metrics.DegradeFunc(base, func(d metrics.Degradation) {
//	    slog.Warn("tokipe degraded", "stage", d.Stage, "reason", d.Reason, "err", d.Err)
//	})
func DegradeFunc(base Recorder, fn func(Degradation)) Recorder {
	return &funcReporter{Recorder: Or(base), fn: fn}
}

type funcReporter struct {
	Recorder
	fn func(Degradation)
}

func (f *funcReporter) Degraded(d Degradation) {
	if f.fn != nil {
		f.fn(d)
	}
}

// --- in-memory implementation ------------------------------------------------

// Observability is an InMemory recorder that also implements the histogram,
// gauge and degradation interfaces. Intended for tests, examples, and the
// terminal dashboard; safe for concurrent use.
type Observability struct {
	*InMemory

	mu           sync.Mutex
	observations map[string][]float64
	labelValues  map[string][]string
	gauges       map[string]float64
	degradations []Degradation
	maxEvents    int
}

var (
	_ HistogramRecorder   = (*Observability)(nil)
	_ GaugeRecorder       = (*Observability)(nil)
	_ DegradationReporter = (*Observability)(nil)
)

// DefaultMaxEvents bounds the retained degradation log. A long-running process
// must not accumulate events forever just because something is misconfigured.
const DefaultMaxEvents = 1024

// NewObservability returns a recorder capturing counters, histograms, gauges and
// degradations in memory.
func NewObservability() *Observability {
	return &Observability{
		InMemory:     NewInMemory(),
		observations: map[string][]float64{},
		labelValues:  map[string][]string{},
		gauges:       map[string]float64{},
		maxEvents:    DefaultMaxEvents,
	}
}

func (o *Observability) Histogram(name string) Histogram { return &memHistogram{rec: o, name: name} }
func (o *Observability) Gauge(name string) Gauge         { return &memGauge{rec: o, name: name} }

func (o *Observability) Degraded(d Degradation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.degradations) >= o.maxEvents {
		// Drop the oldest rather than the newest: a degradation still happening
		// is more useful than the first one that ever did.
		o.degradations = append(o.degradations[:0], o.degradations[1:]...)
	}
	o.degradations = append(o.degradations, d)
}

// Degradations returns a copy of the retained events, oldest first.
func (o *Observability) Degradations() []Degradation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Degradation(nil), o.degradations...)
}

// Observations returns a copy of the values recorded for a histogram.
func (o *Observability) Observations(name string) []float64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]float64(nil), o.observations[name]...)
}

// GaugeValue returns the last value set for a gauge.
func (o *Observability) GaugeValue(name string) float64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gauges[name]
}

// Summary reports count, mean and max for a histogram. Enough for a dashboard;
// a real backend computes proper quantiles, which needs more than a slice.
func (o *Observability) Summary(name string) (count int, mean, maxValue float64) {
	vals := o.Observations(name)
	if len(vals) == 0 {
		return 0, 0, 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
		if v > maxValue {
			maxValue = v
		}
	}
	return len(vals), sum / float64(len(vals)), maxValue
}

// SummaryBy reports count, mean and max for each distinct value of one label,
// keyed by that value. This is what turns metrics.StageLatency into an actual
// per-stage breakdown.
func (o *Observability) SummaryBy(name, label string) map[string]struct {
	Count int
	Mean  float64
	Max   float64
} {
	o.mu.Lock()
	values := append([]string(nil), o.labelValues[name+"|"+label]...)
	o.mu.Unlock()

	out := map[string]struct {
		Count int
		Mean  float64
		Max   float64
	}{}
	for _, v := range values {
		count, mean, maxValue := o.Summary(name + "|" + label + "=" + v)
		out[v] = struct {
			Count int
			Mean  float64
			Max   float64
		}{count, mean, maxValue}
	}
	return out
}

// HistogramNames returns the recorded histogram names, unordered.
func (o *Observability) HistogramNames() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	names := make([]string, 0, len(o.observations))
	for n := range o.observations {
		names = append(names, n)
	}
	return names
}

type memHistogram struct {
	rec  *Observability
	name string
}

// Observe stores the value twice: once under the bare metric name for an
// aggregate view, and once under name+label so a per-label breakdown is
// available. Without the second, "per-stage latency" would be a metric the
// library emits and no in-tree recorder can actually show.
func (h *memHistogram) Observe(v float64, labels map[string]string) {
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	h.rec.observations[h.name] = append(h.rec.observations[h.name], v)
	for k, val := range labels {
		key := h.name + "|" + k + "=" + val
		h.rec.observations[key] = append(h.rec.observations[key], v)
		h.rec.labelValues[h.name+"|"+k] = appendUnique(h.rec.labelValues[h.name+"|"+k], val)
	}
}

func appendUnique(vals []string, v string) []string {
	for _, existing := range vals {
		if existing == v {
			return vals
		}
	}
	return append(vals, v)
}

type memGauge struct {
	rec  *Observability
	name string
}

func (g *memGauge) Set(v float64, _ map[string]string) {
	g.rec.mu.Lock()
	defer g.rec.mu.Unlock()
	g.rec.gauges[g.name] = v
}

// Ensure Degradation formatting stays useful under %v as well as String().
var _ fmt.Stringer = Degradation{}
