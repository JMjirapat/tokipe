package redis_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agentkit/metrics"
	"agentkit/toolcache"
	akredis "agentkit/toolcache/redis"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func newCache(t *testing.T, opts ...akredis.Option) (*akredis.Cache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	c, err := akredis.New(client, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, mr
}

func TestNewRejectsNilClient(t *testing.T) {
	if _, err := akredis.New(nil); err == nil {
		t.Fatal("expected an error for a nil client")
	}
}

func TestGetSetRoundTrip(t *testing.T) {
	rec := metrics.NewInMemory()
	c, _ := newCache(t, akredis.WithMetrics(rec), akredis.WithPrefix("test:"), nil)
	ctx := context.Background()
	args := map[string]any{"q": "go", "nested": map[string]any{"b": 2, "a": 1}}

	if _, found, err := c.Get(ctx, "search", args); found || err != nil {
		t.Fatalf("cold Get: found=%v err=%v", found, err)
	}

	want := map[string]any{"hits": float64(3), "title": "result"}
	if err := c.Set(ctx, "search", args, want, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Different insertion order must hit the same key.
	reordered := map[string]any{"nested": map[string]any{"a": 1, "b": 2}, "q": "go"}
	got, found, err := c.Get(ctx, "search", reordered)
	if err != nil || !found {
		t.Fatalf("warm Get: found=%v err=%v", found, err)
	}
	m, ok := got.Value.(map[string]any)
	if !ok {
		t.Fatalf("Value = %T, want map[string]any", got.Value)
	}
	if m["title"] != "result" || m["hits"] != float64(3) {
		t.Errorf("Value = %v", m)
	}
	if got.CachedAt.IsZero() {
		t.Error("CachedAt not populated")
	}
	if rec.Count(toolcache.CounterHit) != 1 || rec.Count(toolcache.CounterMiss) != 1 {
		t.Errorf("counters = %v", rec.Snapshot())
	}
}

func TestTTLExpiry(t *testing.T) {
	c, mr := newCache(t)
	ctx := context.Background()
	args := map[string]any{"id": 1}

	if err := c.Set(ctx, "fetch", args, "v", 30*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}

	mr.FastForward(29 * time.Second)
	if _, found, err := c.Get(ctx, "fetch", args); !found || err != nil {
		t.Fatalf("entry expired early: found=%v err=%v", found, err)
	}

	mr.FastForward(2 * time.Second)
	if _, found, err := c.Get(ctx, "fetch", args); found || err != nil {
		t.Fatalf("expired entry returned: found=%v err=%v", found, err)
	}

	// ttl <= 0 means no expiry.
	if err := c.Set(ctx, "fetch", args, "forever", -1); err != nil {
		t.Fatalf("Set: %v", err)
	}
	mr.FastForward(1000 * time.Hour)
	if got, found, err := c.Get(ctx, "fetch", args); !found || err != nil || got.Value != "forever" {
		t.Fatalf("ttl<=0 entry expired: found=%v err=%v got=%v", found, err, got)
	}
}

func TestKeyIsStableAndPrefixed(t *testing.T) {
	c, _ := newCache(t, akredis.WithPrefix("p:"))
	k1, err := c.Key("tool", map[string]any{"a": 1, "b": 2})
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	k2, _ := c.Key("tool", map[string]any{"b": 2, "a": 1})
	if k1 != k2 {
		t.Errorf("key not order independent: %s vs %s", k1, k2)
	}
	if len(k1) < len("p:tool:") || k1[:7] != "p:tool:" {
		t.Errorf("key = %q, want the p:tool: prefix", k1)
	}
	if _, err := c.Key("tool", map[string]any{"fn": func() {}}); err == nil {
		t.Error("expected a hashing error for an unencodable arg")
	}
}

func TestFailOpenPaths(t *testing.T) {
	c, mr := newCache(t)
	ctx := context.Background()
	bad := map[string]any{"fn": func() {}}

	res, found, err := c.Get(ctx, "t", bad)
	if err == nil || found || res != (toolcache.CachedResult{}) {
		t.Errorf("unhashable Get = %v, %v, %v", res, found, err)
	}
	if err := c.Set(ctx, "t", bad, 1, time.Minute); err == nil {
		t.Error("expected Set to report the hashing error")
	}
	if err := c.Set(ctx, "t", nil, func() {}, time.Minute); err == nil {
		t.Error("expected Set to report an unencodable value")
	}

	// Corrupt payload: reported as a miss plus an error, never a panic.
	key, err := c.Key("t", map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := mr.Set(key, "not json"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.Get(ctx, "t", map[string]any{"x": 1}); found || err == nil {
		t.Errorf("corrupt payload = found:%v err:%v", found, err)
	}
	// An envelope whose value is absent decodes to a nil value, not an error.
	if err := mr.Set(key, `{"cached_at":"2026-07-26T00:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	if got, found, err := c.Get(ctx, "t", map[string]any{"x": 1}); !found || err != nil || got.Value != nil {
		t.Errorf("empty value envelope = %v, found:%v err:%v", got.Value, found, err)
	}

	// Backend down.
	mr.Close()
	if _, found, err := c.Get(ctx, "t", nil); found || err == nil {
		t.Errorf("closed backend Get = found:%v err:%v", found, err)
	}
	if err := c.Set(ctx, "t", nil, 1, time.Minute); err == nil {
		t.Error("closed backend Set should report an error")
	}
}

func TestContextHandling(t *testing.T) {
	c, _ := newCache(t, akredis.WithClock(nil), akredis.WithClock(func() time.Time {
		return time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.Get(ctx, "t", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Get err = %v", err)
	}
	if err := c.Set(ctx, "t", nil, 1, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Set err = %v", err)
	}

	//nolint:staticcheck // nil-context tolerance is part of the contract
	if err := c.Set(nil, "t", nil, 1, time.Minute); err != nil {
		t.Errorf("nil ctx Set: %v", err)
	}
	//nolint:staticcheck // nil-context tolerance is part of the contract
	got, found, err := c.Get(nil, "t", nil)
	if err != nil || !found {
		t.Fatalf("nil ctx Get: found=%v err=%v", found, err)
	}
	if !got.CachedAt.Equal(time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("CachedAt = %v, want the injected clock value", got.CachedAt)
	}
}

func TestConcurrentGetSet(t *testing.T) {
	c, _ := newCache(t)
	ctx := context.Background()

	const goroutines = 16
	const iterations = 25

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				args := map[string]any{"g": g % 4, "i": i % 5}
				if err := c.Set(ctx, "tool", args, i, time.Minute); err != nil {
					t.Errorf("Set: %v", err)
					return
				}
			}
		}(g)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				args := map[string]any{"i": i % 5, "g": g % 4}
				if _, _, err := c.Get(ctx, "tool", args); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
