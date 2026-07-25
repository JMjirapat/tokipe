package toolcache

import (
	"context"
	"time"

	"agentkit/metrics"
	"agentkit/pipeline"
)

// Executor runs a tool call for real when the cache cannot answer it.
// It is supplied by the caller — agentkit never executes tools itself.
type Executor func(ctx context.Context, call pipeline.ToolCall) (any, error)

// MetaResults is the Request.Metadata key under which Stage publishes tool
// results, keyed by the deterministic hash from HashToolCall.
const MetaResults = "toolcache.results"

// Stage resolves Request.ToolCalls through a Cache, executing only the calls
// that miss. This is the "ToolCache" box in the architecture diagram: the
// Cache interface alone is passive, and this Stage is what turns it into the
// pipeline step that stops duplicate tool calls from being re-executed and
// re-sent to the model.
//
// NOTE: an addition beyond spec §2.4.2, which specifies only the Cache
// interface. Kept in this package because it is meaningless without one.
//
// Fail-open in every direction: a cache backend error degrades to a miss, and
// an Executor error leaves that call unresolved rather than failing the turn.
type Stage struct {
	cache Cache
	exec  Executor
	ttl   time.Duration
	rec   metrics.Recorder
}

// StageOption configures a Stage.
type StageOption func(*Stage)

// WithStageMetrics attaches a metrics recorder.
func WithStageMetrics(r metrics.Recorder) StageOption {
	return func(s *Stage) { s.rec = metrics.Or(r) }
}

// WithTTL sets the TTL used when caching a freshly executed result.
// Zero or negative means "cache without expiry".
func WithTTL(ttl time.Duration) StageOption {
	return func(s *Stage) { s.ttl = ttl }
}

// NewStage returns a Stage backed by cache, executing misses via exec.
// Both are required; a nil cache or exec makes the stage a no-op rather than
// a panic, since a half-configured optimization must never break a turn.
func NewStage(cache Cache, exec Executor, opts ...StageOption) *Stage {
	s := &Stage{cache: cache, exec: exec, rec: metrics.Nop{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Stage) Name() string { return "toolcache" }

func (s *Stage) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	if s.cache == nil || s.exec == nil || len(req.ToolCalls) == 0 {
		return req, nil
	}

	results := make(map[string]any, len(req.ToolCalls))
	for _, call := range req.ToolCalls {
		if ctx.Err() != nil {
			break // fail-open: publish what we have, let the caller's ctx surface later
		}

		key, err := HashToolCall(call.Name, call.Args)
		if err != nil {
			// Unhashable args: execute without caching rather than skipping.
			if v, err := s.exec(ctx, call); err == nil {
				results[call.Name] = v
			}
			continue
		}

		if got, ok, err := s.cache.Get(ctx, call.Name, call.Args); err == nil && ok {
			metrics.Inc(s.rec, CounterHit, map[string]string{"tool": call.Name})
			results[key] = got.Value
			continue
		}
		metrics.Inc(s.rec, CounterMiss, map[string]string{"tool": call.Name})

		v, err := s.exec(ctx, call)
		if err != nil {
			continue // fail-open: leave this call unresolved
		}
		results[key] = v
		_ = s.cache.Set(ctx, call.Name, call.Args, v, s.ttl) // best effort
	}

	if len(results) > 0 {
		req.SetMeta(MetaResults, results)
	}
	return req, nil
}
