package toolcache_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/toolcache"
)

func callsFor(names ...string) []pipeline.ToolCall {
	out := make([]pipeline.ToolCall, 0, len(names))
	for _, n := range names {
		out = append(out, pipeline.ToolCall{Name: n, Args: map[string]any{"a": 1}})
	}
	return out
}

func results(t *testing.T, req *pipeline.Request) map[string]any {
	t.Helper()
	v, ok := req.Metadata[toolcache.MetaResults]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want map[string]any", toolcache.MetaResults, v)
	}
	return m
}

func TestStageExecutesOnMissAndCachesResult(t *testing.T) {
	execs := 0
	st := toolcache.NewStage(toolcache.NewMemoryCache(), func(context.Context, pipeline.ToolCall) (any, error) {
		execs++
		return "value", nil
	}, toolcache.WithTTL(time.Minute))

	req := &pipeline.Request{ToolCalls: callsFor("search")}
	out, err := st.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if execs != 1 {
		t.Fatalf("executor ran %d times, want 1", execs)
	}
	if got := results(t, out); len(got) != 1 {
		t.Fatalf("want 1 result, got %v", got)
	}
}

func TestStageSecondIdenticalCallHitsCache(t *testing.T) {
	execs := 0
	rec := metrics.NewInMemory()
	cache := toolcache.NewMemoryCache()
	st := toolcache.NewStage(cache, func(context.Context, pipeline.ToolCall) (any, error) {
		execs++
		return "value", nil
	}, toolcache.WithTTL(time.Minute), toolcache.WithStageMetrics(rec))

	for range 2 {
		if _, err := st.Process(context.Background(), &pipeline.Request{ToolCalls: callsFor("search")}); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	if execs != 1 {
		t.Fatalf("executor ran %d times, want 1 (second call must hit the cache)", execs)
	}
	if rec.Count(toolcache.CounterHit) != 1 || rec.Count(toolcache.CounterMiss) != 1 {
		t.Fatalf("metrics: hit=%d miss=%d, want 1/1", rec.Count(toolcache.CounterHit), rec.Count(toolcache.CounterMiss))
	}
}

func TestStageFailsOpenOnExecutorError(t *testing.T) {
	st := toolcache.NewStage(toolcache.NewMemoryCache(), func(context.Context, pipeline.ToolCall) (any, error) {
		return nil, errors.New("tool exploded")
	})

	req := &pipeline.Request{ToolCalls: callsFor("search")}
	out, err := st.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("executor errors must not fail the stage, got %v", err)
	}
	if got := results(t, out); len(got) != 0 {
		t.Fatalf("failed call must not be published, got %v", got)
	}
}

type brokenCache struct{ err error }

func (b brokenCache) Get(context.Context, string, map[string]any) (toolcache.CachedResult, bool, error) {
	return toolcache.CachedResult{}, false, b.err
}
func (b brokenCache) Set(context.Context, string, map[string]any, any, time.Duration) error {
	return b.err
}

func TestStageFailsOpenOnCacheBackendError(t *testing.T) {
	execs := 0
	st := toolcache.NewStage(brokenCache{err: errors.New("redis down")}, func(context.Context, pipeline.ToolCall) (any, error) {
		execs++
		return "value", nil
	})

	out, err := st.Process(context.Background(), &pipeline.Request{ToolCalls: callsFor("search")})
	if err != nil {
		t.Fatalf("a dead cache backend must not fail the stage, got %v", err)
	}
	if execs != 1 {
		t.Fatal("a cache error must degrade to a miss and still execute the tool")
	}
	if len(results(t, out)) != 1 {
		t.Fatal("result must still be published when only the cache is broken")
	}
}

func TestStageNoOpWithoutToolCallsOrDeps(t *testing.T) {
	exec := func(context.Context, pipeline.ToolCall) (any, error) { return "v", nil }

	cases := map[string]*toolcache.Stage{
		"nil cache":    toolcache.NewStage(nil, exec),
		"nil executor": toolcache.NewStage(toolcache.NewMemoryCache(), nil),
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := st.Process(context.Background(), &pipeline.Request{ToolCalls: callsFor("search")})
			if err != nil {
				t.Fatalf("half-configured stage must be a no-op, got %v", err)
			}
			if len(results(t, out)) != 0 {
				t.Fatal("no-op stage must publish nothing")
			}
		})
	}

	st := toolcache.NewStage(toolcache.NewMemoryCache(), exec)
	out, err := st.Process(context.Background(), &pipeline.Request{})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if _, ok := out.Metadata[toolcache.MetaResults]; ok {
		t.Fatal("a request with no tool calls must not publish results")
	}
}

func TestStageStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	execs := 0
	st := toolcache.NewStage(toolcache.NewMemoryCache(), func(context.Context, pipeline.ToolCall) (any, error) {
		execs++
		return "v", nil
	})

	if _, err := st.Process(ctx, &pipeline.Request{ToolCalls: callsFor("a", "b")}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if execs != 0 {
		t.Fatalf("no tool may run on a canceled context, ran %d", execs)
	}
}

func TestStageImplementsPipelineStage(t *testing.T) {
	var s pipeline.Stage = toolcache.NewStage(toolcache.NewMemoryCache(), nil)
	if s.Name() != "toolcache" {
		t.Fatalf("Name = %q", s.Name())
	}
}
