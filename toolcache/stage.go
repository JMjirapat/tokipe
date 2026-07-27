package toolcache

import (
	"context"
	"errors"
	"time"

	"github.com/JMjirapat/tokipe/internal/safe"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
)

// Executor runs a tool call for real when the cache cannot answer it.
// It is supplied by the caller — tokipe never executes tools itself.
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

	single bool
	group  group
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

// WithSingleflight enables or disables coalescing of concurrent identical
// calls. Enabled by default.
//
// With it on, N goroutines that miss the same key at the same moment produce
// one execution, not N. Disable it only when a tool must genuinely run once
// per caller — but note that such a tool is already a poor fit for a cache,
// which serves one execution's result to every later caller regardless.
func WithSingleflight(enabled bool) StageOption {
	return func(s *Stage) { s.single = enabled }
}

// NewStage returns a Stage backed by cache, executing misses via exec.
// Both are required; a nil cache or exec makes the stage a no-op rather than
// a panic, since a half-configured optimization must never break a turn.
func NewStage(cache Cache, exec Executor, opts ...StageOption) *Stage {
	s := &Stage{cache: cache, exec: exec, rec: metrics.Nop{}, single: true}
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
			// Still behind the recover boundary — this path calls the same
			// caller-supplied Executor.
			v, execErr := safe.Value(func() (any, error) { return s.exec(ctx, call) })
			if execErr != nil {
				// Previously silent: an unhashable key already costs the cache,
				// and a failure here costs the result too.
				metrics.Degrade(s.rec, metrics.Degradation{
					Stage:  s.Name(),
					Reason: "uncacheable_tool_failed",
					Err:    execErr,
					Detail: call.Name,
				})
				continue
			}
			results[call.Name] = v
			continue
		}

		v, err, shared := s.resolve(ctx, key, call)
		if err != nil {
			reason := "tool_failed"
			var pe *safe.PanicError
			if errors.As(err, &pe) {
				reason = "tool_panicked"
				metrics.Inc(s.rec, CounterPanic, map[string]string{"tool": call.Name})
			}
			// An unresolved tool call means the model answers without data it
			// was supposed to have. Never silent.
			metrics.Degrade(s.rec, metrics.Degradation{
				Stage: s.Name(), Reason: reason, Err: err, Detail: call.Name,
			})
			continue // fail-open: leave this call unresolved
		}
		if shared {
			metrics.Inc(s.rec, CounterCoalesced, map[string]string{"tool": call.Name})
		}
		results[key] = v
	}

	if len(results) > 0 {
		req.SetMeta(MetaResults, results)
	}
	return req, nil
}

// resolve answers one tool call from the cache, or executes it. The whole
// lookup-execute-store sequence runs inside the singleflight, not just the
// execution: a follower that only skipped the exec would still race ahead of
// the leader's Set and miss the cache it was waiting for.
func (s *Stage) resolve(ctx context.Context, key string, call pipeline.ToolCall) (val any, err error, shared bool) {
	// Get, the Executor, and Set each get their OWN recover boundary.
	//
	// A single boundary around all three was wrong: a panic in Get aborted the
	// whole sequence, so the tool never ran — while an ordinary Get *error*
	// degrades to a miss and the tool does run. A panic must behave exactly
	// like an error at each step, or "fail-open" means something different
	// depending on how the backend chose to fail. Likewise a Set panic must
	// not discard a result the Executor already produced.
	work := func() (any, error) {
		if got, hit := s.get(ctx, call); hit {
			metrics.Inc(s.rec, CounterHit, map[string]string{"tool": call.Name})
			return got, nil
		}
		metrics.Inc(s.rec, CounterMiss, map[string]string{"tool": call.Name})

		v, err := safe.Value(func() (any, error) { return s.exec(ctx, call) })
		if err != nil {
			return nil, err
		}

		s.set(ctx, call, v) // best effort, never able to discard v
		return v, nil
	}

	if !s.single {
		v, err := work()
		return v, err, false
	}
	return s.group.Do(key, work)
}

// get consults the cache. Any failure — an error, a miss, or a panic — is
// reported identically as "not found", so the caller proceeds to execute.
func (s *Stage) get(ctx context.Context, call pipeline.ToolCall) (value any, hit bool) {
	var backendErr error
	err := safe.Do(func() error {
		got, ok, err := s.cache.Get(ctx, call.Name, call.Args)
		if err != nil {
			backendErr = err
			return nil
		}
		if !ok {
			return nil
		}
		value, hit = got.Value, true
		return nil
	})
	if err != nil {
		// A panicking backend is a broken backend; count it and treat the
		// lookup as a miss.
		metrics.Inc(s.rec, CounterPanic, map[string]string{"tool": call.Name, "op": "get"})
		metrics.Degrade(s.rec, metrics.Degradation{
			Stage: s.Name(), Reason: "cache_get_panicked", Err: err, Detail: call.Name,
		})
		return nil, false
	}
	// An ordinary backend error is a miss for control flow, but it is NOT the
	// same event as a miss and must not look like one. A dead cache backend
	// degrading every request was the motivating example for Phase 7, and it
	// was the one case still reported as an ordinary miss.
	if backendErr != nil {
		metrics.Degrade(s.rec, metrics.Degradation{
			Stage: s.Name(), Reason: "cache_get_failed", Err: backendErr, Detail: call.Name,
		})
	}
	return value, hit
}

// set stores a freshly executed result. Best effort in every sense: an error
// is ignored and a panic is contained, because the result is already in hand
// and losing the cache write is strictly better than losing the result.
func (s *Stage) set(ctx context.Context, call pipeline.ToolCall, v any) {
	if err := safe.Do(func() error {
		return s.cache.Set(ctx, call.Name, call.Args, v, s.ttl)
	}); err != nil {
		reason := "cache_set_failed"
		var pe *safe.PanicError
		if errors.As(err, &pe) {
			reason = "cache_set_panicked"
			metrics.Inc(s.rec, CounterPanic, map[string]string{"tool": call.Name, "op": "set"})
		}
		// The result survives; only the caching of it was lost. Worth knowing,
		// because it means the next identical call pays full price again.
		metrics.Degrade(s.rec, metrics.Degradation{
			Stage: s.Name(), Reason: reason, Err: err, Detail: call.Name,
		})
	}
}
