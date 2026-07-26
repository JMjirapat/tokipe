package toolcache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentkit/metrics"
	"agentkit/pipeline"
	"agentkit/toolcache"
)

// blockingExecutor returns an Executor that parks until release is closed, so
// a test can guarantee every goroutine is inside the call at the same moment.
// Without that guarantee a "stampede" test can pass by accident, simply
// because the first call finished before the others started.
func blockingExecutor(release <-chan struct{}, calls *atomic.Int64) toolcache.Executor {
	return func(context.Context, pipeline.ToolCall) (any, error) {
		calls.Add(1)
		<-release
		return "value", nil
	}
}

func TestSingleflightCollapsesConcurrentIdenticalCalls(t *testing.T) {
	const goroutines = 50

	var execs atomic.Int64
	release := make(chan struct{})
	rec := metrics.NewInMemory()

	st := toolcache.NewStage(toolcache.NewMemoryCache(), blockingExecutor(release, &execs),
		toolcache.WithTTL(time.Minute), toolcache.WithStageMetrics(rec))

	call := pipeline.ToolCall{Name: "search", Args: map[string]any{"q": "same"}}

	var started, wg sync.WaitGroup
	started.Add(goroutines)
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			started.Done()
			_, err := st.Process(context.Background(), &pipeline.Request{
				ToolCalls: []pipeline.ToolCall{call},
			})
			if err != nil {
				t.Errorf("Process: %v", err)
			}
		}()
	}

	// Let every goroutine reach Process, then unblock the one that got in.
	started.Wait()
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := execs.Load(); n != 1 {
		t.Errorf("executor ran %d times for %d concurrent identical calls, want 1", n, goroutines)
	}
	if got := rec.Count(toolcache.CounterCoalesced); got == 0 {
		t.Error("coalesced calls should be counted")
	}
}

func TestSingleflightDoesNotCollapseDifferentKeys(t *testing.T) {
	const goroutines = 20

	var execs atomic.Int64
	release := make(chan struct{})
	st := toolcache.NewStage(toolcache.NewMemoryCache(), blockingExecutor(release, &execs))

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = st.Process(context.Background(), &pipeline.Request{
				ToolCalls: []pipeline.ToolCall{{Name: "search", Args: map[string]any{"q": i}}},
			})
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := execs.Load(); n != goroutines {
		t.Errorf("executor ran %d times for %d distinct calls, want %d", n, goroutines, goroutines)
	}
}

func TestSingleflightSharesResultsWithFollowers(t *testing.T) {
	const goroutines = 30

	release := make(chan struct{})
	var execs atomic.Int64
	st := toolcache.NewStage(toolcache.NewMemoryCache(), func(context.Context, pipeline.ToolCall) (any, error) {
		execs.Add(1)
		<-release
		return "the-one-result", nil
	})

	got := make([]any, goroutines)
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := &pipeline.Request{ToolCalls: []pipeline.ToolCall{{Name: "t", Args: map[string]any{"a": 1}}}}
			out, err := st.Process(context.Background(), req)
			if err != nil {
				t.Errorf("Process: %v", err)
				return
			}
			for _, v := range out.Metadata[toolcache.MetaResults].(map[string]any) {
				got[i] = v
			}
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, v := range got {
		if v != "the-one-result" {
			t.Fatalf("goroutine %d got %v, want every follower to receive the leader's result", i, v)
		}
	}
}

// A failing leader must not poison later callers: the flight is discarded, and
// the next call is free to try again.
func TestSingleflightErrorIsNotCachedForLaterCallers(t *testing.T) {
	var attempts atomic.Int64
	st := toolcache.NewStage(toolcache.NewMemoryCache(), func(context.Context, pipeline.ToolCall) (any, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("transient failure")
		}
		return "recovered", nil
	})

	call := pipeline.ToolCall{Name: "t", Args: map[string]any{"a": 1}}

	first, err := st.Process(context.Background(), &pipeline.Request{ToolCalls: []pipeline.ToolCall{call}})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if _, ok := first.Metadata[toolcache.MetaResults]; ok {
		t.Fatal("a failed call must publish no result")
	}

	second, err := st.Process(context.Background(), &pipeline.Request{ToolCalls: []pipeline.ToolCall{call}})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	results, ok := second.Metadata[toolcache.MetaResults].(map[string]any)
	if !ok || len(results) != 1 {
		t.Fatalf("retry should succeed, got %v", second.Metadata[toolcache.MetaResults])
	}
	for _, v := range results {
		if v != "recovered" {
			t.Errorf("got %v, want the retry's result", v)
		}
	}
}

// A panicking tool must not leave followers blocked on the flight forever.
func TestSingleflightPanicDoesNotWedgeFollowers(t *testing.T) {
	st := toolcache.NewStage(toolcache.NewMemoryCache(), func(context.Context, pipeline.ToolCall) (any, error) {
		panic("tool exploded")
	})
	call := pipeline.ToolCall{Name: "t", Args: map[string]any{"a": 1}}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic should propagate to the caller, not be swallowed")
			}
		}()
		_, _ = st.Process(context.Background(), &pipeline.Request{ToolCalls: []pipeline.ToolCall{call}})
	}()

	// The key must be free again; this would block forever if it were not.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		_, _ = st.Process(context.Background(), &pipeline.Request{ToolCalls: []pipeline.ToolCall{call}})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a later caller is wedged on the panicked flight")
	}
}

func TestSingleflightCanBeDisabled(t *testing.T) {
	const goroutines = 20

	var execs atomic.Int64
	release := make(chan struct{})
	st := toolcache.NewStage(toolcache.NewMemoryCache(), blockingExecutor(release, &execs),
		toolcache.WithSingleflight(false))

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = st.Process(context.Background(), &pipeline.Request{
				ToolCalls: []pipeline.ToolCall{{Name: "t", Args: map[string]any{"a": 1}}},
			})
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := execs.Load(); n <= 1 {
		t.Errorf("with singleflight off, concurrent misses should each execute; got %d", n)
	}
}
