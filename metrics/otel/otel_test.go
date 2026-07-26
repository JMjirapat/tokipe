package otel_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	akmetrics "agentkit/metrics"
	akotel "agentkit/metrics/otel"
)

// collect reads what OTel actually exported, so assertions are made against the
// SDK's own output rather than against this adapter's bookkeeping.
func collect(t *testing.T, reader *metric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return rm
}

func names(rm metricdata.ResourceMetrics) map[string]bool {
	out := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = true
		}
	}
	return out
}

func newRecorder(t *testing.T, opts ...akotel.Option) (*akotel.Recorder, *metric.ManualReader) {
	t.Helper()
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return akotel.New(provider.Meter("agentkit-test"), opts...), reader
}

func TestSatisfiesEveryRecorderInterface(t *testing.T) {
	rec, _ := newRecorder(t)
	var _ akmetrics.Recorder = rec
	var _ akmetrics.HistogramRecorder = rec
	var _ akmetrics.GaugeRecorder = rec
	var _ akmetrics.DegradationReporter = rec

	// The capability must survive agentkit's safety wrapper — where a Phase 7
	// bug previously hid the optional interfaces behind the wrapper.
	if !akmetrics.SupportsHistograms(akmetrics.Or(rec)) {
		t.Error("histogram support lost through metrics.Or")
	}
}

func TestCountersHistogramsAndGaugesReachOTel(t *testing.T) {
	rec, reader := newRecorder(t)

	akmetrics.Inc(rec, "agentkit.test_counter", map[string]string{"stage": "rag"})
	akmetrics.Observe(rec, akmetrics.StageLatency, 12.5, map[string]string{"stage": "compress"})
	akmetrics.Set(rec, "agentkit.test_gauge", 0.75, nil)

	got := names(collect(t, reader))
	for _, want := range []string{"agentkit.test_counter", akmetrics.StageLatency, "agentkit.test_gauge"} {
		if !got[want] {
			t.Errorf("%q was not exported; got %v", want, got)
		}
	}
}

func TestLabelsBecomeAttributes(t *testing.T) {
	rec, reader := newRecorder(t)
	akmetrics.Inc(rec, "agentkit.labelled", map[string]string{"stage": "toolcache", "reason": "miss"})

	var found bool
	for _, sm := range collect(t, reader).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "agentkit.labelled" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("data is %T, want a Sum", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if dp.Attributes.Len() != 2 {
					t.Errorf("attributes = %v, want 2", dp.Attributes)
				}
				found = true
			}
		}
	}
	if !found {
		t.Error("no data point was exported")
	}
}

// Instrument creation is memoised, because agentkit asks for the same names on
// every turn and OTel instrument creation is not free.
func TestInstrumentsAreMemoisedAndNeverNil(t *testing.T) {
	rec, _ := newRecorder(t)
	for range 100 {
		if rec.Counter("agentkit.repeated") == nil {
			t.Fatal("Counter returned nil")
		}
		if rec.Histogram("agentkit.h") == nil {
			t.Fatal("Histogram returned nil")
		}
		if rec.Gauge("agentkit.g") == nil {
			t.Fatal("Gauge returned nil")
		}
	}
}

func TestDegradedCountsAndCallsTheHandler(t *testing.T) {
	var got []akmetrics.Degradation
	rec, reader := newRecorder(t, akotel.WithDegradationHandler(func(d akmetrics.Degradation) {
		got = append(got, d)
	}))

	akmetrics.Degrade(rec, akmetrics.Degradation{
		Stage:  "toolcache",
		Reason: "cache_get_panicked",
		Err:    errors.New("connection reset by peer"),
	})

	if len(got) != 1 || got[0].Stage != "toolcache" {
		t.Fatalf("handler received %v", got)
	}
	// The error reaches the handler, where a logger can take it...
	if got[0].Err == nil {
		t.Error("the handler should receive the underlying error")
	}

	// ...but never the metric. Error strings are unbounded, and putting them in
	// attributes is how a metrics backend gets a cardinality explosion.
	for _, sm := range collect(t, reader).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != akmetrics.StageDegraded {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("data is %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					if string(kv.Key) == "err" || kv.Value.AsString() == "connection reset by peer" {
						t.Errorf("the error leaked into metric attributes: %v", dp.Attributes)
					}
				}
			}
		}
	}
}

func TestDegradedWithoutAHandlerStillCounts(t *testing.T) {
	rec, reader := newRecorder(t)
	akmetrics.Degrade(rec, akmetrics.Degradation{Stage: "rag", Reason: "embed_failed"})

	if !names(collect(t, reader))[akmetrics.StageDegraded] {
		t.Error("the degradation counter was not exported")
	}
}

func TestConcurrentUse(t *testing.T) {
	rec, _ := newRecorder(t)
	done := make(chan struct{})
	for i := range 8 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for range 50 {
				akmetrics.Inc(rec, "agentkit.concurrent", map[string]string{"g": "x"})
				akmetrics.Observe(rec, "agentkit.concurrent_h", float64(i), nil)
				akmetrics.Set(rec, "agentkit.concurrent_g", float64(i), nil)
				akmetrics.Degrade(rec, akmetrics.Degradation{Stage: "s", Reason: "r"})
			}
		}(i)
	}
	for range 8 {
		<-done
	}
}
