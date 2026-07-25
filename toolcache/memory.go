package toolcache

import (
	"context"
	"sync"
	"time"

	"agentkit/metrics"
)

// entry is one stored value plus its absolute expiry. A zero expiresAt means
// "never expires".
type entry struct {
	value     any
	cachedAt  time.Time
	expiresAt time.Time
}

func (e entry) expired(now time.Time) bool {
	return !e.expiresAt.IsZero() && !now.Before(e.expiresAt)
}

// MemoryCache is a process-local Cache backed by a map guarded by a
// sync.RWMutex.
//
// It deliberately starts no background goroutine: expiry is lazy (checked on
// Get) and bulk reclamation is the caller's decision via Purge. A constructor
// that spawns a janitor makes the cache impossible to garbage collect and ties
// its lifetime to a goroutine nobody owns, so this type leaves the schedule to
// whoever knows the application's lifecycle.
type MemoryCache struct {
	mu      sync.RWMutex
	items   map[string]entry
	metrics metrics.Recorder
	now     func() time.Time
}

// MemoryOption configures a MemoryCache.
type MemoryOption func(*MemoryCache)

// WithMetrics attaches a metrics.Recorder. A nil recorder is safe.
func WithMetrics(r metrics.Recorder) MemoryOption {
	return func(c *MemoryCache) { c.metrics = metrics.Or(r) }
}

// WithClock overrides the time source. Intended for tests.
func WithClock(now func() time.Time) MemoryOption {
	return func(c *MemoryCache) {
		if now != nil {
			c.now = now
		}
	}
}

// NewMemoryCache returns an empty in-memory cache.
func NewMemoryCache(opts ...MemoryOption) *MemoryCache {
	c := &MemoryCache{
		items:   map[string]entry{},
		metrics: metrics.Nop{},
		now:     time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// Get returns the cached result for (toolName, args). An entry past its TTL is
// reported as a miss and removed. A hashing failure is returned as an error
// with found == false; callers may ignore it and treat it as a miss.
func (c *MemoryCache) Get(ctx context.Context, toolName string, args map[string]any) (CachedResult, bool, error) {
	if err := ctxErr(ctx); err != nil {
		return CachedResult{}, false, err
	}
	key, err := HashToolCall(toolName, args)
	if err != nil {
		return CachedResult{}, false, err
	}

	now := c.now()

	c.mu.RLock()
	e, ok := c.items[key]
	c.mu.RUnlock()

	if !ok || e.expired(now) {
		if ok {
			// Lazy expiry: drop the dead entry.
			c.mu.Lock()
			if cur, still := c.items[key]; still && cur.expired(now) {
				delete(c.items, key)
			}
			c.mu.Unlock()
		}
		metrics.Inc(c.metrics, CounterMiss, map[string]string{"tool": toolName})
		return CachedResult{}, false, nil
	}

	metrics.Inc(c.metrics, CounterHit, map[string]string{"tool": toolName})
	return CachedResult{Value: e.value, CachedAt: e.cachedAt}, true, nil
}

// Set stores value with a per-call TTL. ttl <= 0 stores the entry without an
// expiry.
func (c *MemoryCache) Set(ctx context.Context, toolName string, args map[string]any, value any, ttl time.Duration) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	key, err := HashToolCall(toolName, args)
	if err != nil {
		return err
	}

	now := c.now()
	e := entry{value: value, cachedAt: now}
	if ttl > 0 {
		e.expiresAt = now.Add(ttl)
	}

	c.mu.Lock()
	if c.items == nil {
		c.items = map[string]entry{}
	}
	c.items[key] = e
	c.mu.Unlock()
	return nil
}

// Purge removes every expired entry and returns how many were removed. Call it
// from whatever scheduler the application already runs; the cache never starts
// one for you.
func (c *MemoryCache) Purge() int {
	now := c.now()
	removed := 0
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.items {
		if e.expired(now) {
			delete(c.items, k)
			removed++
		}
	}
	return removed
}

// Reset drops every entry, expired or not.
func (c *MemoryCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = map[string]entry{}
}

// Len returns the number of stored entries, including any that are expired but
// not yet reclaimed.
func (c *MemoryCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// ctxErr reports ctx cancellation, tolerating a nil context.
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
