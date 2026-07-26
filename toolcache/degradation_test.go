package toolcache_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/toolcache"
)

type erroringCache struct{ getErr, setErr error }

func (c erroringCache) Get(context.Context, string, map[string]any) (toolcache.CachedResult, bool, error) {
	return toolcache.CachedResult{}, false, c.getErr
}
func (c erroringCache) Set(context.Context, string, map[string]any, any, time.Duration) error {
	return c.setErr
}

// QA-REPORT-2 M4: an ordinary Cache.Get error was folded into the same branch
// as a normal miss and the error discarded. A dead cache backend degrading
// every request was Phase 7's motivating example, and it was the one case
// still indistinguishable from healthy behaviour.
func TestOrdinaryCacheGetErrorIsReported(t *testing.T) {
	rec := metrics.NewObservability()
	st := toolcache.NewStage(
		erroringCache{getErr: errors.New("cache backend unreachable")},
		func(context.Context, pipeline.ToolCall) (any, error) { return "v", nil },
		toolcache.WithStageMetrics(rec))

	out, err := st.Process(context.Background(), &pipeline.Request{
		ToolCalls: []pipeline.ToolCall{{Name: "t", Args: map[string]any{"a": 1}}}})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	events := rec.Degradations()
	if len(events) == 0 {
		t.Fatal("a cache backend error must be reported, not read as a miss")
	}
	if events[0].Reason != "cache_get_failed" {
		t.Errorf("Reason = %q, want cache_get_failed", events[0].Reason)
	}
	if events[0].Err == nil {
		t.Error("the backend error should reach the reporter")
	}
	// Fail-open still holds: the tool ran and the result is published.
	if _, ok := out.Metadata[toolcache.MetaResults]; !ok {
		t.Error("the tool should still have executed")
	}
}

// A healthy miss must NOT report a degradation, or the signal is worthless.
func TestPlainMissIsNotADegradation(t *testing.T) {
	rec := metrics.NewObservability()
	st := toolcache.NewStage(toolcache.NewMemoryCache(),
		func(context.Context, pipeline.ToolCall) (any, error) { return "v", nil },
		toolcache.WithStageMetrics(rec))

	_, _ = st.Process(context.Background(), &pipeline.Request{
		ToolCalls: []pipeline.ToolCall{{Name: "t", Args: map[string]any{"a": 1}}}})

	if got := rec.Degradations(); len(got) != 0 {
		t.Errorf("an ordinary miss reported %v", got)
	}
}

// An unhashable key already costs the cache; a failure there costs the result
// too, and used to be silent.
func TestUncacheableToolFailureIsReported(t *testing.T) {
	rec := metrics.NewObservability()
	st := toolcache.NewStage(toolcache.NewMemoryCache(),
		func(context.Context, pipeline.ToolCall) (any, error) { return nil, errors.New("tool down") },
		toolcache.WithStageMetrics(rec))

	_, _ = st.Process(context.Background(), &pipeline.Request{
		ToolCalls: []pipeline.ToolCall{
			{Name: "t", Args: map[string]any{"bad": string([]byte{0xff})}}}})

	var found bool
	for _, d := range rec.Degradations() {
		if strings.Contains(d.Reason, "uncacheable") {
			found = true
		}
	}
	if !found {
		t.Errorf("no degradation for a failed uncacheable call: %v", rec.Degradations())
	}
}
