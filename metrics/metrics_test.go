package metrics_test

import (
	"sync"
	"testing"

	"agentkit/metrics"
)

// Regression for QA-REPORT.md round 4: a panicking metrics backend escaped
// Pipeline.Run and discarded results the pipeline had already produced.
// Metrics are diagnostics; they must never be able to do that.

type panicRecorder struct{ onCounter bool }

func (p panicRecorder) Counter(string) metrics.Counter {
	if p.onCounter {
		panic("Counter panic")
	}
	return panicCounter{}
}

type panicCounter struct{}

func (panicCounter) Inc(map[string]string) { panic("Inc panic") }

type nilCounterRecorder struct{}

func (nilCounterRecorder) Counter(string) metrics.Counter { return nil }

func TestOrContainsBackendPanics(t *testing.T) {
	cases := map[string]metrics.Recorder{
		"Counter panics":        panicRecorder{onCounter: true},
		"Inc panics":            panicRecorder{},
		"Counter returns nil":   nilCounterRecorder{},
		"nil recorder entirely": nil,
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic escaped: %v", r)
				}
			}()

			// Both call styles must be safe: the metrics.Inc helper, and the
			// direct rec.Counter(n).Inc(l) form used inside compress and cache.
			metrics.Inc(rec, "some_counter", map[string]string{"k": "v"})

			guarded := metrics.Or(rec)
			if guarded == nil {
				t.Fatal("Or must never return nil")
			}
			c := guarded.Counter("some_counter")
			if c == nil {
				t.Fatal("Counter must never return nil")
			}
			c.Inc(nil)
			c.Inc(map[string]string{"k": "v"})
		})
	}
}

func TestOrDoesNotWrapTwice(t *testing.T) {
	once := metrics.Or(metrics.NewInMemory())
	if twice := metrics.Or(once); twice != once {
		t.Error("re-wrapping an already-guarded recorder should be a no-op")
	}
	if got := metrics.Or(metrics.Nop{}); got != (metrics.Nop{}) {
		t.Errorf("Nop should pass through unwrapped, got %T", got)
	}
	if _, ok := metrics.Or(nil).(metrics.Nop); !ok {
		t.Error("a nil recorder should become Nop")
	}
}

// Guarding must not break the recorder that actually works.
func TestGuardedRecorderStillCounts(t *testing.T) {
	rec := metrics.NewInMemory()
	guarded := metrics.Or(rec)

	guarded.Counter("hits").Inc(map[string]string{"a": "1"})
	guarded.Counter("hits").Inc(map[string]string{"a": "2"})
	metrics.Inc(guarded, "misses", nil)

	if got := rec.Count("hits"); got != 2 {
		t.Errorf("hits = %d, want 2", got)
	}
	if got := rec.Count("misses"); got != 1 {
		t.Errorf("misses = %d, want 1", got)
	}
	snap := rec.Snapshot()
	if snap["hits"] != 2 || snap["misses"] != 1 {
		t.Errorf("snapshot = %v", snap)
	}
	// Snapshot must be a copy, not a live view.
	snap["hits"] = 99
	if rec.Count("hits") != 2 {
		t.Error("Snapshot leaked the internal map")
	}
}

func TestNopRecorderIsInert(t *testing.T) {
	c := metrics.Nop{}.Counter("anything")
	if c == nil {
		t.Fatal("Nop must still return a usable Counter")
	}
	c.Inc(nil) // must not panic
}

func TestInMemoryIsConcurrencySafe(t *testing.T) {
	rec := metrics.NewInMemory()
	guarded := metrics.Or(rec)

	const goroutines, each = 16, 50
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				guarded.Counter("shared").Inc(map[string]string{"g": "x"})
				_ = rec.Snapshot()
			}
		}()
	}
	wg.Wait()

	if got := rec.Count("shared"); got != goroutines*each {
		t.Errorf("count = %d, want %d", got, goroutines*each)
	}
}
