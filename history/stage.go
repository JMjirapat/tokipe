// Package history trims a conversation to fit a token budget.
//
// It closes the gap Phase 6 of docs/ROADMAP.md identified: budget.Policy
// computed a number that nothing consumed, while Request.Messages grew without
// limit. For the spec's first target use case — a long-running agent loop with
// growing context (§1.5.1) — unbounded history is the dominant cost, and
// compression only ever touched retrieved chunks.
//
// # Why it trims the middle
//
// Trimming the OLDEST turns is the obvious approach and the wrong one. Providers
// that cache automatically match the longest common prefix across requests;
// dropping from the front changes the very first non-static bytes, so every turn
// presents a new prefix and nothing is ever reused. Dropping from the middle
// keeps head and tail intact, so the prefix stays stable for as long as the head
// does.
//
// It also reads better. The earliest turns usually carry the task framing, and
// the latest carry the live thread; the middle is where the redundancy lives.
//
// # What it never touches
//
//   - Static content (Message.Static, or the system role). That is the prefix
//     cache.Aligner anchors its breakpoint to. Moving one byte of it forfeits
//     every cache hit — worse than not trimming, since a cache write costs more
//     than an ordinary token.
//   - The newest message. It is the current turn; dropping it answers a
//     different question than the one asked.
package history

import (
	"context"
	"fmt"
	"strings"

	"agentkit/budget"
	"agentkit/internal/safe"
	"agentkit/metrics"
	"agentkit/pipeline"
)

// Metric names emitted by the stage.
const (
	// MetricTrimmed counts turns where something was dropped or summarised.
	MetricTrimmed = "history.trimmed"
	// MetricSkipped counts turns left alone: already within budget, or not
	// trimmable without breaking a rule above.
	MetricSkipped = "history.skipped"
	// MetricOverBudget counts turns still over budget after every legal trim.
	// A non-zero value here is the honest signal that the budget cannot be met
	// without touching content this stage refuses to touch.
	MetricOverBudget = "history.over_budget"
)

// Metadata keys the stage publishes so a caller can see what it did.
const (
	MetaDropped   = "history.dropped_messages"
	MetaTokensIn  = "history.tokens_before"
	MetaTokensOut = "history.tokens_after"
)

// Summarizer replaces dropped messages with a single message standing in for
// them. Optional: without one, the stage drops them outright.
//
// A Summarizer will usually call a model, which makes it the one part of this
// package that costs money and latency. That is why it is opt-in, and why an
// error from it degrades to dropping rather than failing the turn.
type Summarizer interface {
	Summarize(ctx context.Context, dropped []pipeline.Message) (pipeline.Message, error)
}

// Default head/tail retention. Two leading turns usually hold the task framing;
// four trailing turns usually hold the live exchange.
const (
	DefaultKeepHead = 2
	DefaultKeepTail = 4
)

// Stage implements pipeline.Stage.
//
// PLACEMENT: it must run after retrieval and compression, because it can only
// decide what to drop once it can see the real payload size — and after
// compression has already made the chunks as small as they are going to get.
// It must run before cache.Aligner, which computes breakpoints over the final
// message layout. config.Stages() enforces this.
type Stage struct {
	policy    budget.Policy
	counter   budget.TokenCounter
	keepHead  int
	keepTail  int
	summarize Summarizer
	rec       metrics.Recorder
}

// Option configures a Stage.
type Option func(*Stage)

// WithCounter sets the token counter. Defaults to budget.CharEstimator, which
// never fails and costs nothing; pass a provider-backed counter when trimming
// against a hard limit rather than for cost.
func WithCounter(c budget.TokenCounter) Option {
	return func(s *Stage) { s.counter = budget.Or(c) }
}

// WithRetention sets how many oldest and newest non-static messages survive
// regardless of budget. Values below zero are treated as zero.
func WithRetention(head, tail int) Option {
	return func(s *Stage) {
		s.keepHead, s.keepTail = max(head, 0), max(tail, 0)
	}
}

// WithSummarizer replaces dropped messages with a summary instead of removing
// them. The summary must be smaller than what it replaces, or the stage discards
// it and drops as usual — a "summary" that grows the prompt is a bug, not a
// trade-off.
func WithSummarizer(s Summarizer) Option {
	return func(st *Stage) { st.summarize = s }
}

// WithMetrics attaches a metrics recorder.
func WithMetrics(r metrics.Recorder) Option {
	return func(s *Stage) { s.rec = metrics.Or(r) }
}

// NewStage returns a Stage enforcing policy.
func NewStage(policy budget.Policy, opts ...Option) *Stage {
	s := &Stage{
		policy:   policy,
		counter:  budget.CharEstimator{},
		keepHead: DefaultKeepHead,
		keepTail: DefaultKeepTail,
		rec:      metrics.Nop{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Stage) Name() string { return "history" }

// Process trims req to fit the budget for its TurnType.
//
// Fail-open throughout: an unknown budget, a counter error, or nothing legal to
// drop all leave the request exactly as it arrived. The turn is never failed for
// being too large — that is the provider's decision to make, not ours.
func (s *Stage) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	if req == nil {
		return req, nil
	}
	limit := s.policy.BudgetFor(req.TurnType)
	if limit <= 0 {
		// No budget configured for this turn type means "unlimited", not zero.
		metrics.Inc(s.rec, MetricSkipped, map[string]string{"reason": "no_budget"})
		return req, nil
	}
	if err := ctx.Err(); err != nil {
		return req, nil
	}

	cost, err := s.count(ctx, req)
	if err != nil {
		metrics.Inc(s.rec, MetricSkipped, map[string]string{"reason": "count_failed"})
		return req, nil
	}
	if cost.Total <= limit {
		metrics.Inc(s.rec, MetricSkipped, map[string]string{"reason": "within_budget"})
		return req, nil
	}

	before := cost.Total
	trimmed, dropped := s.trim(ctx, req, limit, cost)
	if dropped == 0 {
		metrics.Inc(s.rec, MetricOverBudget, map[string]string{"reason": "nothing_trimmable"})
		return req, nil
	}

	after, err := s.count(ctx, trimmed)
	if err != nil {
		// The trim already happened and is valid; only the accounting failed.
		after = cost
	}

	trimmed.SetMeta(MetaDropped, dropped)
	trimmed.SetMeta(MetaTokensIn, before)
	trimmed.SetMeta(MetaTokensOut, after.Total)
	metrics.Inc(s.rec, MetricTrimmed, map[string]string{"turn": req.TurnType.String()})
	if after.Total > limit {
		metrics.Inc(s.rec, MetricOverBudget, map[string]string{"reason": "still_over"})
	}
	return trimmed, nil
}

// trimTarget is the size trimming aims for, which is below the budget when a
// Summarizer is configured.
//
// Without this, a summariser can never fire: trimming stops the instant the
// request fits, leaving no room for the summary, so the summary is rejected for
// pushing back over budget — every time. Reserving headroom up front is the fix.
// The reserve is deliberately small; a summary that needs more than this is not
// summarising.
func (s *Stage) trimTarget(limit int) int {
	if s.summarize == nil {
		return limit
	}
	reserve := max(limit/20, MinSummaryReserve) // 5%, floor MinSummaryReserve
	if reserve >= limit {
		return limit // the budget is too small to reserve from
	}
	return limit - reserve
}

// MinSummaryReserve is the floor on tokens held back for a summary when one is
// configured. Large enough for an elision note plus a short recap.
const MinSummaryReserve = 48

// count runs CountRequest behind a recover boundary: the counter is
// caller-supplied code on the request path, so a panic in it must cost us the
// trim, not the turn (spec §2.5.1).
func (s *Stage) count(ctx context.Context, req *pipeline.Request) (budget.RequestCost, error) {
	return safe.Value(func() (budget.RequestCost, error) {
		return budget.CountRequest(ctx, s.counter, req)
	})
}

// trim drops middle messages until the request fits, then trims retrieved
// chunks if messages alone were not enough.
//
// It mutates req in place and returns it, like every other stage in agentkit. An
// earlier version returned a copy, which looked safer and was worse: the
// caller's own pointer never saw the trim, so Metadata written here was
// invisible and a caller inspecting req after Run saw the untrimmed request.
// Safety comes from building NEW slices rather than from copying the Request —
// the caller's backing arrays are never written to.
func (s *Stage) trim(
	ctx context.Context, req *pipeline.Request, limit int, cost budget.RequestCost,
) (*pipeline.Request, int) {
	// Trimming aims below the budget when a summariser is configured, but the
	// summary is checked against the real budget. Conflating the two was a bug:
	// checking the summary against the reduced target consumed the very reserve
	// that was meant to make room for it, so no summary ever fit.
	target := s.trimTarget(limit)

	// A new slice, so the caller's backing array is never written to.
	messages := append([]pipeline.Message(nil), req.Messages...)

	// Indices of messages that may be dropped: non-static, not the newest, and
	// outside the head/tail retention window.
	candidates := s.candidates(messages)
	if len(candidates) == 0 {
		return req, 0
	}

	// Drop oldest-first *within the middle*, so the surviving head keeps the
	// task framing and the prefix stays stable for as long as possible.
	var droppedMsgs []pipeline.Message
	remaining := cost.Total
	keep := make([]bool, len(messages))
	for i := range keep {
		keep[i] = true
	}

	for _, idx := range candidates {
		if remaining <= target {
			break
		}
		n, err := s.counter.CountTokens(ctx, messages[idx].Content)
		if err != nil {
			break // counting stopped working; stop trimming
		}
		keep[idx] = false
		droppedMsgs = append(droppedMsgs, messages[idx])
		remaining -= n
	}
	if len(droppedMsgs) == 0 {
		return req, 0
	}

	kept := make([]pipeline.Message, 0, len(messages))
	inserted := false
	for i, m := range messages {
		if keep[i] {
			kept = append(kept, m)
			continue
		}
		// A summary, if any, takes the position of the first dropped message so
		// the conversation stays in chronological order.
		if !inserted && s.summarize != nil {
			if sum, ok := s.summary(ctx, droppedMsgs, remaining, limit); ok {
				kept = append(kept, sum)
			}
			inserted = true
		}
	}
	req.Messages = kept

	// Messages alone may not be enough. Retrieved chunks are dynamic content —
	// never cached, always re-sent — so they are the next cheapest thing to
	// give up, lowest-similarity first.
	if remaining > target && len(req.RetrievedChunks) > 0 {
		req.RetrievedChunks, _ = s.trimChunks(ctx, req.RetrievedChunks, remaining, target)
	}

	return req, len(droppedMsgs)
}

// summary asks the Summarizer for a stand-in. It is rejected unless it actually
// helps: a summary no smaller than what it replaced would be a pure loss, and a
// failing summarizer must degrade to plain dropping.
func (s *Stage) summary(
	ctx context.Context, dropped []pipeline.Message, remaining, limit int,
) (pipeline.Message, bool) {
	msg, err := safe.Value(func() (pipeline.Message, error) {
		return s.summarize.Summarize(ctx, dropped)
	})
	if err != nil || strings.TrimSpace(msg.Content) == "" {
		return pipeline.Message{}, false
	}
	n, err := s.counter.CountTokens(ctx, msg.Content)
	if err != nil || remaining+n > limit {
		return pipeline.Message{}, false
	}
	if msg.Static {
		// A summary must never enter the static prefix: it changes whenever the
		// conversation does, which is the definition of not-static.
		msg.Static = false
	}
	return msg, true
}

// trimChunks drops retrieved chunks, least similar first, until the request
// fits or none are left. It never drops the last remaining chunk if that alone
// would not bring the request under budget — losing all evidence to still be
// over budget is the worst of both outcomes.
func (s *Stage) trimChunks(
	ctx context.Context, chunks []pipeline.Chunk, remaining, limit int,
) ([]pipeline.Chunk, int) {
	order := make([]int, len(chunks))
	for i := range order {
		order[i] = i
	}
	// Ascending similarity: the least useful chunk goes first.
	for i := 1; i < len(order); i++ {
		for j := i; j > 0 && chunks[order[j]].Similarity < chunks[order[j-1]].Similarity; j-- {
			order[j], order[j-1] = order[j-1], order[j]
		}
	}

	drop := make(map[int]bool)
	for _, idx := range order {
		if remaining <= limit || len(drop) == len(chunks)-1 {
			break
		}
		n, err := s.counter.CountTokens(ctx, chunks[idx].Content+chunks[idx].SourceURL)
		if err != nil {
			break
		}
		drop[idx] = true
		remaining -= n
	}

	kept := make([]pipeline.Chunk, 0, len(chunks)-len(drop))
	for i, ch := range chunks {
		if !drop[i] {
			kept = append(kept, ch)
		}
	}
	return kept, remaining
}

// candidates returns the droppable message indices, oldest-first, excluding
// static content, the newest message, and the head/tail retention windows.
func (s *Stage) candidates(msgs []pipeline.Message) []int {
	newest := budget.NewestMessage(msgs)

	var movable []int
	for i, m := range msgs {
		if budget.IsStatic(m) || i == newest {
			continue
		}
		movable = append(movable, i)
	}
	if len(movable) <= s.keepHead+s.keepTail {
		return nil // the retention windows cover everything movable
	}
	return movable[s.keepHead : len(movable)-s.keepTail]
}

// FuncSummarizer adapts a function into a Summarizer.
type FuncSummarizer func(ctx context.Context, dropped []pipeline.Message) (pipeline.Message, error)

func (f FuncSummarizer) Summarize(ctx context.Context, dropped []pipeline.Message) (pipeline.Message, error) {
	return f(ctx, dropped)
}

// ElisionSummarizer replaces dropped turns with a one-line note saying how many
// were removed. It calls no model, costs nothing, and never fails.
//
// It is not a summary in any useful sense — it preserves no content. Its purpose
// is to stop the model inferring a false continuity between turns that are no
// longer adjacent, which is a real failure mode of silent trimming.
func ElisionSummarizer() Summarizer {
	return FuncSummarizer(func(_ context.Context, dropped []pipeline.Message) (pipeline.Message, error) {
		return pipeline.Message{
			Role:    "user",
			Content: fmt.Sprintf("[%d earlier turns omitted to fit the context budget]", len(dropped)),
		}, nil
	})
}
