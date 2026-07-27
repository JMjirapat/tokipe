// Package redis provides a Redis-backed toolcache.Cache.
//
// It lives in its own Go module so the core tokipe module keeps its
// zero-third-party-dependency guarantee (docs/spec.md §2.6). Values are stored
// JSON-encoded under a key derived from toolcache.HashToolCall, so an in-memory
// and a Redis cache agree on what "the same tool call" means.
//
// Every failure path is fail-open: Get returns (toolcache.CachedResult{},
// false, err) and the caller is free to ignore the error and treat the lookup
// as a miss.
package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/toolcache"

	goredis "github.com/redis/go-redis/v9"
)

// DefaultPrefix is prepended to every key written by this cache.
const DefaultPrefix = "tokipe:toolcache:"

// Client is the subset of *goredis.Client this cache needs. Declaring it here
// keeps the cache testable and lets callers pass a cluster or ring client.
type Client interface {
	Get(ctx context.Context, key string) *goredis.StringCmd
	Set(ctx context.Context, key string, value any, ttl time.Duration) *goredis.StatusCmd
}

// storedValue is the JSON envelope written to Redis. Storing CachedAt
// explicitly means the caller sees the true insertion time rather than an
// approximation derived from the remaining TTL.
type storedValue struct {
	Value    json.RawMessage `json:"value"`
	CachedAt time.Time       `json:"cached_at"`
}

// Cache is a Redis-backed toolcache.Cache.
type Cache struct {
	client  Client
	prefix  string
	metrics metrics.Recorder
	now     func() time.Time
}

// Option configures a Cache.
type Option func(*Cache)

// WithPrefix overrides DefaultPrefix.
func WithPrefix(p string) Option {
	return func(c *Cache) { c.prefix = p }
}

// WithMetrics attaches a metrics.Recorder. A nil recorder is safe.
func WithMetrics(r metrics.Recorder) Option {
	return func(c *Cache) { c.metrics = metrics.Or(r) }
}

// WithClock overrides the time source. Intended for tests.
func WithClock(now func() time.Time) Option {
	return func(c *Cache) {
		if now != nil {
			c.now = now
		}
	}
}

// New returns a Cache backed by client. A nil client is rejected at
// construction rather than deferred to a nil dereference at request time —
// that is a programming error, not an optimization failure (docs/spec.md §2.5).
func New(client Client, opts ...Option) (*Cache, error) {
	if client == nil {
		return nil, errors.New("toolcache/redis: nil client")
	}
	c := &Cache{
		client:  client,
		prefix:  DefaultPrefix,
		metrics: metrics.Nop{},
		now:     time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c, nil
}

// Key returns the Redis key this cache uses for a given tool call.
func (c *Cache) Key(toolName string, args map[string]any) (string, error) {
	h, err := toolcache.HashToolCall(toolName, args)
	if err != nil {
		return "", err
	}
	return c.prefix + toolName + ":" + h, nil
}

// Get looks up a cached tool result. A cache miss, an expired key, and a
// corrupt stored payload all report found == false; the last also returns the
// decode error so a caller that wants to log it can.
func (c *Cache) Get(ctx context.Context, toolName string, args map[string]any) (toolcache.CachedResult, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return toolcache.CachedResult{}, false, err
	}

	key, err := c.Key(toolName, args)
	if err != nil {
		return toolcache.CachedResult{}, false, err
	}

	raw, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			c.count(toolcache.CounterMiss, toolName)
			return toolcache.CachedResult{}, false, nil
		}
		// Backend down: a miss the caller may ignore.
		return toolcache.CachedResult{}, false, fmt.Errorf("toolcache/redis: get %q: %w", toolName, err)
	}

	var sv storedValue
	if err := json.Unmarshal(raw, &sv); err != nil {
		c.count(toolcache.CounterMiss, toolName)
		return toolcache.CachedResult{}, false, fmt.Errorf("toolcache/redis: decode envelope for %q: %w", toolName, err)
	}

	var value any
	if len(sv.Value) > 0 {
		if err := json.Unmarshal(sv.Value, &value); err != nil {
			c.count(toolcache.CounterMiss, toolName)
			return toolcache.CachedResult{}, false, fmt.Errorf("toolcache/redis: decode value for %q: %w", toolName, err)
		}
	}

	c.count(toolcache.CounterHit, toolName)
	return toolcache.CachedResult{Value: value, CachedAt: sv.CachedAt}, true, nil
}

// Set stores value JSON-encoded with a per-call TTL. ttl <= 0 stores the key
// without an expiry.
func (c *Cache) Set(ctx context.Context, toolName string, args map[string]any, value any, ttl time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	key, err := c.Key(toolName, args)
	if err != nil {
		return err
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("toolcache/redis: encode value for %q: %w", toolName, err)
	}

	payload, err := json.Marshal(storedValue{Value: encoded, CachedAt: c.now()})
	if err != nil {
		return fmt.Errorf("toolcache/redis: encode envelope for %q: %w", toolName, err)
	}

	if ttl < 0 {
		ttl = 0 // go-redis treats 0 as "no expiry"
	}
	if err := c.client.Set(ctx, key, payload, ttl).Err(); err != nil {
		return fmt.Errorf("toolcache/redis: set %q: %w", toolName, err)
	}
	return nil
}

func (c *Cache) count(name, toolName string) {
	metrics.Inc(c.metrics, name, map[string]string{"tool": toolName, "backend": "redis"})
}

var _ toolcache.Cache = (*Cache)(nil)
