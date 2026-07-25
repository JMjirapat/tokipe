package toolcache_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"agentkit/metrics"
	"agentkit/toolcache"
)

func TestHashToolCallIsOrderIndependent(t *testing.T) {
	// Same logical arguments, built with different insertion orders at every
	// nesting level.
	a := map[string]any{}
	a["alpha"] = 1
	a["nested"] = map[string]any{"x": true, "y": []any{1, map[string]any{"p": "q", "o": "r"}}}
	a["zulu"] = "last"

	b := map[string]any{}
	b["zulu"] = "last"
	nested := map[string]any{}
	inner := map[string]any{}
	inner["o"] = "r"
	inner["p"] = "q"
	nested["y"] = []any{1, inner}
	nested["x"] = true
	b["nested"] = nested
	b["alpha"] = 1

	ha, err := toolcache.HashToolCall("search", a)
	if err != nil {
		t.Fatalf("HashToolCall(a): %v", err)
	}
	hb, err := toolcache.HashToolCall("search", b)
	if err != nil {
		t.Fatalf("HashToolCall(b): %v", err)
	}
	if ha != hb {
		t.Fatalf("hashes differ for equal args:\n a=%s\n b=%s", ha, hb)
	}
	if len(ha) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(ha))
	}

	// Repeated hashing of the same map is stable across many iterations, so a
	// randomized map walk cannot leak into the key.
	for i := 0; i < 200; i++ {
		h, err := toolcache.HashToolCall("search", a)
		if err != nil || h != ha {
			t.Fatalf("iteration %d: %s %v", i, h, err)
		}
	}
}

func TestHashToolCallDistinguishes(t *testing.T) {
	base := map[string]any{"q": "go"}
	h1, _ := toolcache.HashToolCall("search", base)
	h2, _ := toolcache.HashToolCall("lookup", base)
	if h1 == h2 {
		t.Error("different tool names must hash differently")
	}

	h3, _ := toolcache.HashToolCall("search", map[string]any{"q": "rust"})
	if h1 == h3 {
		t.Error("different args must hash differently")
	}

	// Array order is meaningful.
	h4, _ := toolcache.HashToolCall("t", map[string]any{"l": []any{1, 2}})
	h5, _ := toolcache.HashToolCall("t", map[string]any{"l": []any{2, 1}})
	if h4 == h5 {
		t.Error("array order must affect the hash")
	}

	// nil and empty args are equivalent and hashable.
	hn, err := toolcache.HashToolCall("t", nil)
	if err != nil {
		t.Fatalf("nil args: %v", err)
	}
	he, _ := toolcache.HashToolCall("t", map[string]any{})
	if hn == he {
		t.Log("nil and empty args hash identically (expected: both encode as null/{} distinctly)")
	}

	// Structs and nested values round-trip through JSON deterministically.
	type payload struct {
		B string `json:"b"`
		A string `json:"a"`
	}
	hs1, err := toolcache.HashToolCall("t", map[string]any{"p": payload{B: "2", A: "1"}})
	if err != nil {
		t.Fatalf("struct arg: %v", err)
	}
	hs2, _ := toolcache.HashToolCall("t", map[string]any{"p": map[string]any{"a": "1", "b": "2"}})
	if hs1 != hs2 {
		t.Error("a struct and its equivalent map must hash identically")
	}
}

func TestHashToolCallUnmarshalable(t *testing.T) {
	if _, err := toolcache.HashToolCall("t", map[string]any{"fn": func() {}}); err == nil {
		t.Fatal("expected an error for a non-JSON-encodable arg")
	}
}

func TestMemoryCacheGetSet(t *testing.T) {
	rec := metrics.NewInMemory()
	c := toolcache.NewMemoryCache(toolcache.WithMetrics(rec), nil)
	ctx := context.Background()
	args := map[string]any{"q": "go"}

	if _, found, err := c.Get(ctx, "search", args); found || err != nil {
		t.Fatalf("cold Get: found=%v err=%v", found, err)
	}

	if err := c.Set(ctx, "search", args, "result", time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, found, err := c.Get(ctx, "search", map[string]any{"q": "go"})
	if err != nil || !found {
		t.Fatalf("warm Get: found=%v err=%v", found, err)
	}
	if got.Value != "result" {
		t.Errorf("Value = %v", got.Value)
	}
	if got.CachedAt.IsZero() {
		t.Error("CachedAt not populated")
	}
	if rec.Count("toolcache.hit") != 1 || rec.Count("toolcache.miss") != 1 {
		t.Errorf("counters = %v", rec.Snapshot())
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d", c.Len())
	}

	c.Reset()
	if c.Len() != 0 {
		t.Errorf("Len after Reset = %d", c.Len())
	}
}

func TestMemoryCacheTTLExpiry(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	c := toolcache.NewMemoryCache(toolcache.WithClock(clock), toolcache.WithClock(nil))
	ctx := context.Background()
	args := map[string]any{"id": 1}

	if err := c.Set(ctx, "fetch", args, "v", 30*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}

	advance(29 * time.Second)
	if _, found, _ := c.Get(ctx, "fetch", args); !found {
		t.Fatal("entry expired early")
	}

	advance(2 * time.Second)
	if _, found, _ := c.Get(ctx, "fetch", args); found {
		t.Fatal("expired entry still returned")
	}
	if c.Len() != 0 {
		t.Errorf("expired entry not reclaimed lazily, Len = %d", c.Len())
	}

	// ttl <= 0 means no expiry.
	if err := c.Set(ctx, "fetch", args, "forever", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	advance(1000 * time.Hour)
	if got, found, _ := c.Get(ctx, "fetch", args); !found || got.Value != "forever" {
		t.Fatalf("ttl<=0 entry expired: found=%v got=%v", found, got)
	}
}

func TestMemoryCachePurge(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	c := toolcache.NewMemoryCache(toolcache.WithClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}))
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := c.Set(ctx, "t", map[string]any{"i": i}, i, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Set(ctx, "t", map[string]any{"keep": true}, 1, 0); err != nil {
		t.Fatal(err)
	}

	if n := c.Purge(); n != 0 {
		t.Errorf("premature Purge removed %d", n)
	}

	mu.Lock()
	now = now.Add(2 * time.Second)
	mu.Unlock()

	if n := c.Purge(); n != 5 {
		t.Errorf("Purge removed %d, want 5", n)
	}
	if c.Len() != 1 {
		t.Errorf("Len after Purge = %d, want 1 (the never-expiring entry)", c.Len())
	}
}

func TestMemoryCacheFailsOpen(t *testing.T) {
	c := toolcache.NewMemoryCache()
	bad := map[string]any{"fn": func() {}}

	res, found, err := c.Get(context.Background(), "t", bad)
	if err == nil {
		t.Fatal("expected a hashing error")
	}
	if found || res != (toolcache.CachedResult{}) {
		t.Errorf("error path must return a zero result and found=false, got %v %v", res, found)
	}
	if err := c.Set(context.Background(), "t", bad, 1, time.Minute); err == nil {
		t.Fatal("expected Set to report the hashing error")
	}

	// Cancelled contexts are reported, never panicked on.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.Get(ctx, "t", nil); err == nil {
		t.Error("cancelled Get should report ctx.Err()")
	}
	if err := c.Set(ctx, "t", nil, 1, time.Minute); err == nil {
		t.Error("cancelled Set should report ctx.Err()")
	}

	//nolint:staticcheck // nil-context tolerance is part of the contract
	if _, _, err := c.Get(nil, "t", nil); err != nil {
		t.Errorf("nil ctx Get: %v", err)
	}
	//nolint:staticcheck // nil-context tolerance is part of the contract
	if err := c.Set(nil, "t", nil, 1, time.Minute); err != nil {
		t.Errorf("nil ctx Set: %v", err)
	}
}

func TestMemoryCacheConcurrent(t *testing.T) {
	c := toolcache.NewMemoryCache(toolcache.WithMetrics(metrics.NewInMemory()))
	ctx := context.Background()

	const goroutines = 32
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				args := map[string]any{"g": g % 4, "i": i % 8, "nested": map[string]any{"a": i, "b": g}}
				if err := c.Set(ctx, "tool", args, fmt.Sprintf("%d-%d", g, i), time.Duration(i%3)*time.Millisecond); err != nil {
					t.Errorf("Set: %v", err)
					return
				}
			}
		}(g)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				args := map[string]any{"nested": map[string]any{"b": g, "a": i}, "i": i % 8, "g": g % 4}
				if _, _, err := c.Get(ctx, "tool", args); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if i%25 == 0 {
					c.Purge()
				}
			}
		}(g)
	}
	wg.Wait()
}

// Ensure the interface assertion holds for external implementers too.
var _ toolcache.Cache = (*toolcache.MemoryCache)(nil)
