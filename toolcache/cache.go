// Package toolcache caches tool-call results keyed by a deterministic hash of
// the tool name and its arguments.
//
// Every backend is fail-open: an error from Get is returned alongside
// (CachedResult{}, false, err) so a caller that does not care can ignore the
// error and simply treat the lookup as a miss. No implementation in this
// package or its sub-modules may panic on a backend failure.
//
// See docs/spec.md §2.4.2.
package toolcache

import (
	"context"
	"time"
)

// CachedResult is a stored tool-call result.
type CachedResult struct {
	Value    any
	CachedAt time.Time
}

// Cache is the frozen tool-result cache contract. Ship an in-memory
// implementation here and a Redis-backed one under toolcache/redis.
type Cache interface {
	// Get returns the cached result for (toolName, args). found is false for a
	// miss or an expired entry. A backend error is reported as
	// (CachedResult{}, false, err) and callers may ignore it.
	Get(ctx context.Context, toolName string, args map[string]any) (CachedResult, bool, error)

	// Set stores value under (toolName, args) with a per-call TTL. A ttl <= 0
	// means the entry never expires.
	Set(ctx context.Context, toolName string, args map[string]any, value any, ttl time.Duration) error
}

// CounterHit and CounterMiss are the metric names backends emit.
// CounterCoalesced counts calls that waited on an identical in-flight call
// instead of executing — the stampede that singleflight prevented.
const (
	CounterHit       = "toolcache.hit"
	CounterMiss      = "toolcache.miss"
	CounterCoalesced = "toolcache.coalesced"
)

// compile-time assertion that the in-memory backend satisfies Cache.
var _ Cache = (*MemoryCache)(nil)
