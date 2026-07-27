package cache

import "github.com/JMjirapat/tokipe/pipeline"

// BreakpointSpec declares one place the caller wants a provider-side cache
// breakpoint anchored.
//
// v1 recognises exactly one anchor: the end of the static segment. A spec with
// AfterStaticContent == false is *ignored* rather than guessed at, because the
// only other boundaries in a Request are the append-only history boundary
// (marginal value) and the dynamic boundary (forbidden — see the comment on
// computeBreakpoints). Inventing an anchor we cannot prove is cache-safe would
// be worse than emitting nothing.
type BreakpointSpec struct {
	// AfterStaticContent places the breakpoint after content that should never
	// change within a session (system prompt, tool definitions).
	AfterStaticContent bool

	// Reason is copied onto the emitted pipeline.CacheBreakpoint for logs.
	Reason string
}

// DefaultSpecs is what NewAligner uses when the caller supplies no specs: a
// single breakpoint after the static prefix, which is the only universally
// correct anchor.
func DefaultSpecs() []BreakpointSpec {
	return []BreakpointSpec{{AfterStaticContent: true, Reason: "static_prefix"}}
}

// segments describes how a Request's messages partition into the three ordered
// regions the aligner cares about. Indices are into the *reordered* message
// slice: [0, staticLen) static, [staticLen, staticLen+historyLen) history,
// then the dynamic content (RetrievedChunks — not messages, so it occupies no
// index), then the newest message at index staticLen+historyLen if present.
type segments struct {
	ordered    []pipeline.Message
	staticLen  int
	historyLen int
	hasNewest  bool
}

// dynamicBoundary is the message index at which per-turn dynamic content
// (RetrievedChunks) is serialised into the outgoing prompt. Chunks are not
// messages, so they consume no index: they sit in the gap between the last
// history message (index dynamicBoundary-1) and the newest message (index
// dynamicBoundary).
//
// Consequently a breakpoint at AfterMessageIndex <= dynamicBoundary-1 is
// anchored strictly BEFORE the dynamic content and is safe, while any
// breakpoint at AfterMessageIndex >= dynamicBoundary is anchored at or after
// the dynamic content and is forbidden whenever chunks are present.
func (s segments) dynamicBoundary() int { return s.staticLen + s.historyLen }

// isStatic reports whether a message belongs in the cacheable static prefix.
// Caller-marked Static messages qualify; so do system-role messages, which are
// static by construction in every provider API tokipe targets.
func isStatic(m pipeline.Message) bool { return m.Static || m.Role == "system" }

// partition performs the reordering mandated by docs/spec.md §2.4.6:
//
//	static segment -> append-only history -> dynamic content -> newest message
//
// It is a stable partition: relative order *within* each segment is preserved
// exactly, so an append-only conversation keeps producing an append-only
// prompt prefix (the precondition for any provider-side cache hit).
//
// partition is idempotent: partition(partition(msgs)) == partition(msgs).
func partition(msgs []pipeline.Message) segments {
	n := len(msgs)
	if n == 0 {
		return segments{ordered: msgs}
	}

	// The newest message is the final message of the turn, unless that message
	// is itself static (all-static request), in which case there is no
	// separate "newest" segment to hold out.
	newestIdx := -1
	if !isStatic(msgs[n-1]) {
		newestIdx = n - 1
	}

	ordered := make([]pipeline.Message, 0, n)
	var staticLen, historyLen int
	for i, m := range msgs {
		if i == newestIdx || !isStatic(m) {
			continue
		}
		ordered = append(ordered, m)
		staticLen++
	}
	for i, m := range msgs {
		if i == newestIdx || isStatic(m) {
			continue
		}
		ordered = append(ordered, m)
		historyLen++
	}
	if newestIdx >= 0 {
		ordered = append(ordered, msgs[newestIdx])
	}

	return segments{
		ordered:    ordered,
		staticLen:  staticLen,
		historyLen: historyLen,
		hasNewest:  newestIdx >= 0,
	}
}

// computeBreakpoints is a pure, deterministic function of the Request shape:
// same messages + same specs in, same breakpoints out, every time, with no
// dependence on maps, time, randomness or I/O (docs/spec.md §2.5 requirement 4).
//
// It expects req.Messages to already be in partition order — but because
// partition is idempotent it produces identical indices either way.
//
// ############################ NON-NEGOTIABLE ############################
// This function must NEVER anchor a breakpoint inside, at, or after the
// dynamic segment (Request.RetrievedChunks).
//
// Why: a provider-side prompt cache is a *prefix* cache. Everything up to the
// breakpoint is hashed and stored; a hit requires that prefix to be
// byte-identical on the next turn. RetrievedChunks are recomputed per turn
// from the user's query — they differ on essentially every turn. Anchoring a
// breakpoint at or after them means the cached prefix contains per-turn-varying
// bytes, so the cache never hits, yet the provider still bills cache-WRITE
// tokens (25% surcharge on Anthropic) for every single turn. That is strictly
// worse than not caching at all: it converts an optimisation into a permanent
// tax. Hence "poisons the cache".
//
// This is also why the stage ordering is not negotiable: rag.Stage,
// compress.Stage and lazyload must ALL run BEFORE cache.Aligner. Each of them
// mutates content (populating chunks, rewriting chunk text, inlining lazily
// loaded refs). If any of them ran after the aligner, the bytes the
// breakpoints were computed against would no longer be the bytes sent to the
// provider, and the same cache poisoning would occur through the back door.
// ########################################################################
func computeBreakpoints(req *pipeline.Request, specs []BreakpointSpec) []pipeline.CacheBreakpoint {
	if req == nil {
		return nil
	}
	seg := partition(req.Messages)
	if seg.staticLen == 0 {
		// Fail-open: nothing provably static, so nothing is provably cacheable.
		return nil
	}

	hasDynamic := len(req.RetrievedChunks) > 0
	forbiddenFrom := seg.dynamicBoundary()

	var out []pipeline.CacheBreakpoint
	seen := map[int]bool{}
	for _, spec := range specs {
		if !spec.AfterStaticContent {
			continue
		}
		idx := seg.staticLen - 1

		// Defensive guard. With the current single anchor this can never fire
		// (staticLen-1 < staticLen+historyLen always), but it is deliberately
		// kept as an unconditional invariant check so that any future anchor
		// added to this loop cannot silently violate the rule above.
		if hasDynamic && idx >= forbiddenFrom {
			continue
		}
		if seen[idx] {
			continue
		}
		seen[idx] = true

		reason := spec.Reason
		if reason == "" {
			reason = "static_prefix"
		}
		out = append(out, pipeline.CacheBreakpoint{AfterMessageIndex: idx, Reason: reason})
	}
	return out
}
