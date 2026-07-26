package toolcache

import "sync"

// group deduplicates concurrent work by key: when several goroutines ask for
// the same key at once, exactly one runs the function and the rest wait for
// its result.
//
// This is the same idea as golang.org/x/sync/singleflight, reimplemented in
// ~40 lines because the core module is stdlib-only by contract (spec §2.6).
// It is not a general-purpose replacement — no Forget, no DoChan — just the
// piece Stage needs.
//
// The zero value is ready to use.
type group struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Do runs fn for key, ensuring only one execution is in flight at a time.
// Duplicate callers wait for the original to finish and receive its result.
// shared reports whether the result came from another caller's execution.
//
// Caveat inherited from every singleflight: fn runs with the *leader's*
// context. A follower whose own context is cancelled still waits for the
// leader. Stage keeps this acceptable by checking ctx before each call and by
// treating tool execution as idempotent — which the cache already assumes,
// since it serves one call's result to later callers anyway.
func (g *group) Do(key string, fn func() (any, error)) (val any, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*flight)
	}
	if f, ok := g.m[key]; ok {
		g.mu.Unlock()
		f.wg.Wait()
		return f.val, f.err, true
	}

	f := new(flight)
	f.wg.Add(1)
	g.m[key] = f
	g.mu.Unlock()

	// A panic in fn must not leave followers blocked on wg.Wait forever, so
	// the bookkeeping is deferred rather than run at the end of the happy path.
	defer func() {
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		f.wg.Done()
	}()

	f.val, f.err = fn()
	return f.val, f.err, false
}

// inFlight reports how many distinct keys are currently executing. Test helper.
func (g *group) inFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.m)
}
