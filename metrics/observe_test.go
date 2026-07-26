package metrics_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JMjirapat/tokipe/metrics"
)

// counterOnly implements Recorder and nothing else — the shape most callers
// have, and the case every optional feature must tolerate.
type counterOnly struct {
	mu sync.Mutex
	n  int
}

func (c *counterOnly) Counter(string) metrics.Counter { return c }
func (c *counterOnly) Inc(map[string]string)          { c.mu.Lock(); c.n++; c.mu.Unlock() }

func TestOptionalFeaturesAreNoOpsOnACounterOnlyRecorder(t *testing.T) {
	rec := &counterOnly{}
	// None of these may panic or error on a recorder that supports none of them.
	metrics.Observe(rec, "h", 1.5, map[string]string{"a": "b"})
	metrics.ObserveDuration(rec, "h", time.Second, nil)
	metrics.Set(rec, "g", 2.5, nil)
	metrics.Degrade(rec, metrics.Degradation{Stage: "s", Reason: "r"})

	// The degradation counter still lands, because Degrade counts as well as
	// reports — a caller who wires only a Recorder must still see the count.
	if rec.n == 0 {
		t.Error("Degrade should have incremented a counter even without a reporter")
	}
	if metrics.SupportsHistograms(rec) {
		t.Error("SupportsHistograms should be false for a counter-only recorder")
	}
}

// Regression for a bug caught during Phase 7: metrics.Or wraps recorders for
// panic safety, and the first wrapper implemented only Recorder. That HID the
// optional interfaces behind itself, so every histogram, gauge and degradation
// was silently discarded. Wrapping for safety must not cost capability.
func TestOrPreservesOptionalCapabilities(t *testing.T) {
	inner := metrics.NewObservability()
	wrapped := metrics.Or(inner)

	if _, ok := wrapped.(metrics.HistogramRecorder); !ok {
		t.Error("the safety wrapper hides HistogramRecorder")
	}
	if _, ok := wrapped.(metrics.GaugeRecorder); !ok {
		t.Error("the safety wrapper hides GaugeRecorder")
	}
	if _, ok := wrapped.(metrics.DegradationReporter); !ok {
		t.Error("the safety wrapper hides DegradationReporter")
	}

	// And the values must actually reach the inner recorder.
	metrics.Observe(wrapped, "lat", 12, nil)
	metrics.Set(wrapped, "rate", 0.75, nil)
	metrics.Degrade(wrapped, metrics.Degradation{Stage: "rag", Reason: "embed_failed"})

	if n, _, _ := inner.Summary("lat"); n != 1 {
		t.Errorf("observation did not reach the recorder (count %d)", n)
	}
	if got := inner.GaugeValue("rate"); got != 0.75 {
		t.Errorf("gauge = %v, want 0.75", got)
	}
	if len(inner.Degradations()) != 1 {
		t.Errorf("degradation did not reach the reporter: %v", inner.Degradations())
	}
}

// A recorder that supports Recorder but not histograms must not be reported as
// supporting them just because the wrapper implements the interface.
func TestSupportsHistogramsReflectsTheInnerRecorder(t *testing.T) {
	if metrics.SupportsHistograms(metrics.Or(&counterOnly{})) {
		t.Error("a wrapped counter-only recorder must not claim histogram support")
	}
	if !metrics.SupportsHistograms(metrics.Or(metrics.NewObservability())) {
		t.Error("a wrapped Observability must claim histogram support")
	}
	if metrics.SupportsHistograms(nil) {
		t.Error("a nil recorder must not claim histogram support")
	}
}

type panicObservability struct {
	*metrics.InMemory
	onHistogram bool
}

func (p *panicObservability) Histogram(string) metrics.Histogram {
	if p.onHistogram {
		panic("Histogram panic")
	}
	return panicHistogram{}
}
func (p *panicObservability) Gauge(string) metrics.Gauge   { return panicGauge{} }
func (p *panicObservability) Degraded(metrics.Degradation) { panic("Degraded panic") }

type panicHistogram struct{}

func (panicHistogram) Observe(float64, map[string]string) { panic("Observe panic") }

type panicGauge struct{}

func (panicGauge) Set(float64, map[string]string) { panic("Set panic") }

// Diagnostics must never break a turn, whichever method panics.
func TestPanickingObservabilityBackendIsContained(t *testing.T) {
	for name, rec := range map[string]metrics.Recorder{
		"panic in Histogram": &panicObservability{InMemory: metrics.NewInMemory(), onHistogram: true},
		"panic in Observe":   &panicObservability{InMemory: metrics.NewInMemory()},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic escaped: %v", r)
				}
			}()
			metrics.Observe(rec, "h", 1, nil)
			metrics.ObserveDuration(rec, "h", time.Millisecond, nil)
			metrics.Set(rec, "g", 1, nil)
			metrics.Degrade(rec, metrics.Degradation{Stage: "s", Reason: "r"})
		})
	}
}

func TestDegradationStringIncludesEverythingUseful(t *testing.T) {
	d := metrics.Degradation{
		Stage:  "toolcache",
		Reason: "cache_get_panicked",
		Detail: "run_tests",
		Err:    errors.New("connection reset"),
	}
	got := d.String()
	for _, want := range []string{"toolcache", "cache_get_panicked", "run_tests", "connection reset"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}

	// A degradation without an error is legitimate — a decision, not a failure.
	bare := metrics.Degradation{Stage: "history", Reason: "nothing_trimmable"}
	if strings.Contains(bare.String(), "<nil>") {
		t.Errorf("String() = %q; a nil Err must not render as <nil>", bare.String())
	}
}

func TestDegradeFuncWrapsWithoutLosingCounters(t *testing.T) {
	base := metrics.NewInMemory()
	var got []metrics.Degradation

	rec := metrics.DegradeFunc(base, func(d metrics.Degradation) { got = append(got, d) })
	metrics.Inc(rec, "some.counter", nil)
	metrics.Degrade(rec, metrics.Degradation{Stage: "rag", Reason: "embed_failed"})

	if base.Count("some.counter") != 1 {
		t.Error("wrapping must not lose counters")
	}
	if len(got) != 1 || got[0].Stage != "rag" {
		t.Errorf("callback received %v", got)
	}
	if base.Count(metrics.StageDegraded) != 1 {
		t.Error("Degrade must count as well as report")
	}
}

func TestDegradationLogIsBounded(t *testing.T) {
	rec := metrics.NewObservability()
	for i := range metrics.DefaultMaxEvents + 500 {
		rec.Degraded(metrics.Degradation{Stage: "s", Reason: "r", Detail: string(rune('a' + i%26))})
	}
	if got := len(rec.Degradations()); got > metrics.DefaultMaxEvents {
		t.Errorf("retained %d events, want at most %d", got, metrics.DefaultMaxEvents)
	}
}

func TestSummaryByBreaksDownOnALabel(t *testing.T) {
	rec := metrics.NewObservability()
	for _, v := range []float64{1, 3} {
		metrics.Observe(rec, metrics.StageLatency, v, map[string]string{"stage": "rag"})
	}
	metrics.Observe(rec, metrics.StageLatency, 10, map[string]string{"stage": "compress"})

	byStage := rec.SummaryBy(metrics.StageLatency, "stage")
	if len(byStage) != 2 {
		t.Fatalf("got %d stages, want 2: %v", len(byStage), byStage)
	}
	if s := byStage["rag"]; s.Count != 2 || s.Mean != 2 || s.Max != 3 {
		t.Errorf("rag = %+v, want count 2 mean 2 max 3", s)
	}
	if s := byStage["compress"]; s.Count != 1 || s.Max != 10 {
		t.Errorf("compress = %+v", s)
	}
	// The aggregate must still be available alongside the breakdown.
	if n, _, _ := rec.Summary(metrics.StageLatency); n != 3 {
		t.Errorf("aggregate count = %d, want 3", n)
	}
}

func TestObserveDurationRecordsMilliseconds(t *testing.T) {
	rec := metrics.NewObservability()
	metrics.ObserveDuration(rec, "lat", 250*time.Millisecond, nil)
	if _, mean, _ := rec.Summary("lat"); mean < 249 || mean > 251 {
		t.Errorf("mean = %v ms, want ~250", mean)
	}
}

func TestObservabilityIsConcurrencySafe(t *testing.T) {
	rec := metrics.NewObservability()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 50 {
				metrics.Inc(rec, "c", nil)
				metrics.Observe(rec, "h", float64(i), map[string]string{"stage": "s"})
				metrics.Set(rec, "g", float64(i), nil)
				metrics.Degrade(rec, metrics.Degradation{Stage: "s", Reason: "r"})
				_ = rec.Degradations()
				_ = rec.SummaryBy("h", "stage")
			}
		}(i)
	}
	wg.Wait()

	if rec.Count("c") != 16*50 {
		t.Errorf("counter = %d, want %d", rec.Count("c"), 16*50)
	}
}
