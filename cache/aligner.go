// Package cache implements the CacheAlignStage (docs/spec.md §2.4.6): it
// reorders a Request's messages so that the parts a provider can cache come
// first, then emits the cache breakpoints marking that cacheable prefix.
//
// It performs no I/O, holds no mutable state, and is safe for concurrent use
// by any number of goroutines.
//
// ORDERING CONTRACT: this stage must be the LAST context-shaping stage in the
// pipeline — after rag, compress and lazyload. See computeBreakpoints in
// breakpoint.go for why violating that ordering poisons the provider cache.
package cache

import (
	"context"

	"agentkit/metrics"
	"agentkit/pipeline"
)

// MetricBreakpoints is the counter name incremented once per emitted
// breakpoint.
const MetricBreakpoints = "cache_breakpoints"

// Aligner implements pipeline.Stage.
type Aligner struct {
	specs []BreakpointSpec
	rec   metrics.Recorder
}

// Option configures an Aligner. Options are applied after the specs, so they
// can never be shadowed by the variadic spec list.
type Option func(*Aligner)

// WithMetrics attaches a metrics.Recorder. A nil recorder is tolerated.
func WithMetrics(r metrics.Recorder) Option {
	return func(a *Aligner) { a.rec = metrics.Or(r) }
}

// NewAligner builds an Aligner. With no specs it uses DefaultSpecs().
//
// The signature matches the spec's `NewAligner(specs ...BreakpointSpec)`
// exactly; options are supplied through the variadic-free NewAlignerWith when
// a metrics.Recorder is wanted.
func NewAligner(specs ...BreakpointSpec) *Aligner {
	return NewAlignerWith(specs, nil)
}

// NewAlignerWith is the option-taking constructor. specs may be nil to accept
// DefaultSpecs().
func NewAlignerWith(specs []BreakpointSpec, opts ...Option) *Aligner {
	if len(specs) == 0 {
		specs = DefaultSpecs()
	}
	a := &Aligner{
		// Defensive copy: the caller's slice must not be able to mutate our
		// behaviour later (no shared mutable state).
		specs: append([]BreakpointSpec(nil), specs...),
		rec:   metrics.Nop{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a
}

// Name implements pipeline.Stage.
func (a *Aligner) Name() string { return "cache_align" }

// Process reorders req.Messages into
//
//	static -> append-only history -> [dynamic chunks] -> newest message
//
// and populates req.CacheBreakpoints.
//
// Fail-open (docs/spec.md §2.5 requirement 1): Process never returns a non-nil
// error. A request with no static content is returned completely unchanged
// with zero breakpoints — no static content means nothing is provably
// cacheable, and guessing would be worse than doing nothing.
func (a *Aligner) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	if req == nil {
		return req, nil
	}
	// No I/O happens here, but honour cancellation anyway rather than doing
	// work whose result will be discarded. Fail-open: no error is returned.
	if ctx != nil && ctx.Err() != nil {
		return req, nil
	}

	seg := partition(req.Messages)
	if seg.staticLen == 0 {
		req.CacheBreakpoints = nil
		return req, nil
	}

	req.Messages = seg.ordered
	req.CacheBreakpoints = computeBreakpoints(req, a.specs)

	for range req.CacheBreakpoints {
		a.rec.Counter(MetricBreakpoints).Inc(map[string]string{"stage": a.Name()})
	}
	return req, nil
}

// compile-time proof that Aligner satisfies the frozen Stage contract.
var _ pipeline.Stage = (*Aligner)(nil)
