package router

import (
	"context"
	"sort"
	"strings"

	"agentkit/internal/safe"
	"agentkit/metrics"
	"agentkit/pipeline"
)

// Weights are the relative importances of the three complexity signals. They
// need not sum to 1: the score is divided by their sum, so any set of
// non-negative weights produces a score in [0,1].
//
// Negative weights are treated as 0.
type Weights struct {
	// Length weights how much raw prompt text the request carries.
	Length float64
	// Chunks weights how many RAG chunks were retrieved.
	Chunks float64
	// Code weights how code-like the prompt looks.
	Code float64
}

// DefaultWeights: length dominates because it is the signal that correlates
// most directly with both cost and difficulty; code presence is the strongest
// *qualitative* hint that a bigger model is warranted; chunk count is a weaker,
// partly redundant signal (chunk text already shows up in the length term when
// the caller has folded chunks into messages).
func DefaultWeights() Weights {
	return Weights{Length: 0.5, Code: 0.3, Chunks: 0.2}
}

// Saturation points. Each raw signal is divided by its saturation constant and
// clamped to 1, so a request twice as long as DefaultLengthSaturation is not
// twice as "complex" — it is simply maxed out on that axis.
const (
	// DefaultLengthSaturation is the character count at which the length
	// signal reaches 1.0. ~8000 chars ≈ 2000 tokens of English prose.
	DefaultLengthSaturation = 8000
	// DefaultChunkSaturation is the retrieved-chunk count at which the chunk
	// signal reaches 1.0.
	DefaultChunkSaturation = 10
	// DefaultFenceSaturation is the number of ``` fenced blocks at which the
	// fence half of the code signal reaches 1.0.
	DefaultFenceSaturation = 2
	// DefaultDensityFloor is the symbol-character ratio below which text is
	// considered plain prose (contributes 0 to the density signal).
	DefaultDensityFloor = 0.03
	// DefaultDensityCeil is the symbol-character ratio at or above which text
	// is considered unambiguously code (density signal 1.0).
	DefaultDensityCeil = 0.15
)

// symbolChars are the characters whose frequency distinguishes source code from
// prose. Deliberately excludes the period, comma, apostrophe and double quote,
// which are just as common in English prose.
const symbolChars = "{}()[]<>;=+*/\\&|_#$@`~^%"

// HeuristicRouter picks a Tier by scoring the request with estimateComplexity
// and choosing the cheapest tier whose MaxComplexity covers that score.
//
// The spec says tiers "must be sorted ascending by MaxComplexity by the
// caller". HeuristicRouter defensively sorts its own copy of the slice at
// construction time anyway, so a mis-ordered caller gets correct routing rather
// than silent misbehaviour. The caller's slice is never mutated.
//
// A HeuristicRouter is immutable after construction and safe for concurrent
// use.
type HeuristicRouter struct {
	tiers []Tier

	weights          Weights
	lengthSaturation int
	chunkSaturation  int
	fenceSaturation  int
	densityFloor     float64
	densityCeil      float64

	rec metrics.Recorder
}

// Option configures a HeuristicRouter.
type Option func(*HeuristicRouter)

// WithWeights overrides the relative weights of the three signals.
func WithWeights(w Weights) Option {
	return func(r *HeuristicRouter) { r.weights = w }
}

// WithLengthSaturation sets the character count at which the length signal
// maxes out. Values <= 0 are ignored.
func WithLengthSaturation(chars int) Option {
	return func(r *HeuristicRouter) {
		if chars > 0 {
			r.lengthSaturation = chars
		}
	}
}

// WithChunkSaturation sets the retrieved-chunk count at which the chunk signal
// maxes out. Values <= 0 are ignored.
func WithChunkSaturation(chunks int) Option {
	return func(r *HeuristicRouter) {
		if chunks > 0 {
			r.chunkSaturation = chunks
		}
	}
}

// WithFenceSaturation sets the number of ``` fenced blocks at which the fence
// half of the code signal maxes out. Values <= 0 are ignored.
func WithFenceSaturation(fences int) Option {
	return func(r *HeuristicRouter) {
		if fences > 0 {
			r.fenceSaturation = fences
		}
	}
}

// WithSymbolDensity sets the floor/ceiling of the symbol-density half of the
// code signal. Ignored unless 0 <= floor < ceil.
func WithSymbolDensity(floor, ceil float64) Option {
	return func(r *HeuristicRouter) {
		if floor >= 0 && floor < ceil {
			r.densityFloor, r.densityCeil = floor, ceil
		}
	}
}

// WithMetrics attaches a Recorder. Route increments the "router_decision"
// counter with labels {"client": <chosen client name>, "reason": <decision
// reason>}. A nil Recorder is fine (metrics are always opt-in).
func WithMetrics(rec metrics.Recorder) Option {
	return func(r *HeuristicRouter) { r.rec = rec }
}

// NewHeuristicRouter builds a router over the given tiers. Tiers with a nil
// Client are dropped: routing to them would produce a decision the pipeline
// would have to fall back out of anyway.
func NewHeuristicRouter(tiers ...Tier) *HeuristicRouter {
	return NewHeuristicRouterWith(tiers)
}

// NewHeuristicRouterWith is NewHeuristicRouter with functional options. It is a
// separate constructor because the spec freezes NewHeuristicRouter's signature
// as variadic over Tier, which leaves no room for a variadic Option.
func NewHeuristicRouterWith(tiers []Tier, opts ...Option) *HeuristicRouter {
	r := &HeuristicRouter{
		weights:          DefaultWeights(),
		lengthSaturation: DefaultLengthSaturation,
		chunkSaturation:  DefaultChunkSaturation,
		fenceSaturation:  DefaultFenceSaturation,
		densityFloor:     DefaultDensityFloor,
		densityCeil:      DefaultDensityCeil,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	r.rec = metrics.Or(r.rec)

	// Defensive copy + sort: never mutate the caller's slice, never trust its
	// ordering.
	cp := make([]Tier, 0, len(tiers))
	for _, t := range tiers {
		if t.Client != nil {
			cp = append(cp, t)
		}
	}
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].MaxComplexity < cp[j].MaxComplexity })
	r.tiers = cp
	return r
}

// Route scores req and returns the cheapest tier that can serve it.
//
// With no usable tiers it returns the zero RouteDecision (nil Client), which
// pipeline.Run reads as "use the fallback client" — the fail-open behaviour
// required by docs/spec.md §2.5.
func (r *HeuristicRouter) Route(_ context.Context, req *pipeline.Request) RouteDecision {
	if len(r.tiers) == 0 {
		return RouteDecision{}
	}
	score := r.estimateComplexity(req)
	for _, t := range r.tiers {
		if score <= t.MaxComplexity {
			return r.record(RouteDecision{Client: t.Client, Reason: "heuristic", Confidence: 1 - score})
		}
	}
	last := r.tiers[len(r.tiers)-1]
	return r.record(RouteDecision{Client: last.Client, Reason: "heuristic_overflow", Confidence: 0})
}

func (r *HeuristicRouter) record(d RouteDecision) RouteDecision {
	metrics.Inc(r.rec, "router_decision", map[string]string{
		"client": safe.Name(d.Client.Name, UnnamedClient),
		"reason": d.Reason,
	})
	return d
}

// estimateComplexity scores req in [0,1]. The exact formula:
//
//	text        = req.Query + every req.Messages[i].Content, concatenated
//	lengthSig   = min(1, len(text) / lengthSaturation)
//	chunkSig    = min(1, len(req.RetrievedChunks) / chunkSaturation)
//	fenceSig    = min(1, count(text, "```") / 2 / fenceSaturation)
//	densitySig  = clamp01((symbolRatio(text) - densityFloor) /
//	                      (densityCeil - densityFloor))
//	codeSig     = max(fenceSig, densitySig)
//
//	score = (wLength*lengthSig + wChunks*chunkSig + wCode*codeSig)
//	        / (wLength + wChunks + wCode)
//
// where symbolRatio is the fraction of text made up of symbolChars, and every
// weight is clamped to >= 0. Because each signal is already in [0,1] and the
// weighted sum is divided by the total weight, score is always in [0,1]. With
// zero total weight the score is 0 (every request goes to the cheapest tier).
//
// The formula is a pure function of the Request, so it is deterministic:
// identical Requests always produce identical scores and identical decisions.
func (r *HeuristicRouter) estimateComplexity(req *pipeline.Request) float64 {
	if req == nil {
		return 0
	}

	var b strings.Builder
	b.Grow(len(req.Query))
	b.WriteString(req.Query)
	for _, m := range req.Messages {
		b.WriteString(m.Content)
	}
	text := b.String()

	lengthSig := ratio(float64(len(text)), float64(r.lengthSaturation))
	chunkSig := ratio(float64(len(req.RetrievedChunks)), float64(r.chunkSaturation))

	fences := float64(strings.Count(text, "```")) / 2
	fenceSig := ratio(fences, float64(r.fenceSaturation))
	densitySig := clamp01((symbolRatio(text) - r.densityFloor) / (r.densityCeil - r.densityFloor))
	codeSig := max(fenceSig, densitySig)

	wLen := nonNeg(r.weights.Length)
	wChunk := nonNeg(r.weights.Chunks)
	wCode := nonNeg(r.weights.Code)
	total := wLen + wChunk + wCode
	if total <= 0 {
		return 0
	}
	return clamp01((wLen*lengthSig + wChunk*chunkSig + wCode*codeSig) / total)
}

// estimateComplexity is the package-level form the spec names, using the
// default weights and saturation points.
func estimateComplexity(req *pipeline.Request) float64 { //nolint:unused // spec-named helper, exercised by tests
	return NewHeuristicRouterWith(nil).estimateComplexity(req)
}

// symbolRatio is the fraction of s composed of symbolChars, in [0,1].
func symbolRatio(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	n := 0
	for _, c := range s {
		if strings.ContainsRune(symbolChars, c) {
			n++
		}
	}
	return float64(n) / float64(len([]rune(s)))
}

// ratio is v/saturation clamped to [0,1], with a non-positive saturation
// meaning "signal is always maxed out when v > 0".
func ratio(v, saturation float64) float64 {
	if saturation <= 0 {
		if v > 0 {
			return 1
		}
		return 0
	}
	return clamp01(v / saturation)
}

func clamp01(v float64) float64 {
	switch {
	case v != v: // NaN
		return 0
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

func nonNeg(v float64) float64 {
	if v < 0 || v != v {
		return 0
	}
	return v
}
