package budget

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/JMjirapat/tokipe/pipeline"
)

// TokenCounter measures the token cost of text.
//
// It exists because "how many tokens is this?" has no answer that is both cheap
// and exact. A character estimate is free and wrong; a provider's tokenizer
// endpoint is exact and costs a round trip. Which trade-off is right depends on
// the caller, so the choice is theirs — and both satisfy this interface.
//
// Implementations must be safe for concurrent use.
type TokenCounter interface {
	// CountTokens returns the token count of text. An error means "unknown",
	// and every caller in tokipe treats that as a reason to skip the
	// optimization rather than to fail the turn.
	CountTokens(ctx context.Context, text string) (int, error)
}

// DefaultCharsPerToken is the divisor CharEstimator uses when unset.
//
// Four characters per token is the rule of thumb for English prose in
// BPE-family tokenizers. It is wrong for code (denser), wrong for CJK (much
// denser), and wrong for whitespace-heavy JSON (sparser). It is a starting
// point, not a measurement — see CharEstimator's own doc comment.
const DefaultCharsPerToken = 4

// CharEstimator estimates tokens by dividing rune count by CharsPerToken.
//
// The estimate is free and never fails, which makes it the right default for a
// library that must not add a network round trip to every turn. It is also
// systematically wrong in ways worth knowing:
//
//   - Code and punctuation-dense text use MORE tokens than this predicts.
//   - CJK text uses far more; a Han character is often a token by itself.
//   - Repetitive whitespace uses fewer.
//
// It counts runes rather than bytes, so multi-byte text is not double-counted —
// a byte-length estimate would over-count Thai or Japanese by a factor of three.
//
// If you are trimming close to a hard provider limit, use a real tokenizer
// instead. If you are trimming to control cost, this is usually enough.
type CharEstimator struct {
	// CharsPerToken defaults to DefaultCharsPerToken. Values below 1 are
	// treated as the default rather than producing absurd counts.
	CharsPerToken int
}

// NewCharEstimator returns a CharEstimator using DefaultCharsPerToken.
func NewCharEstimator() CharEstimator { return CharEstimator{} }

func (c CharEstimator) CountTokens(_ context.Context, text string) (int, error) {
	n := c.CharsPerToken
	if n < 1 {
		n = DefaultCharsPerToken
	}
	runes := utf8.RuneCountInString(text)
	if runes == 0 {
		return 0, nil
	}
	// Round up: a 1-rune string costs a token, not zero.
	return (runes + n - 1) / n, nil
}

// Or returns c, or a CharEstimator when c is nil. Constructors funnel their
// counter through this so a nil dependency degrades to an estimate rather than
// panicking — the same shape as metrics.Or.
func Or(c TokenCounter) TokenCounter {
	if c == nil {
		return CharEstimator{}
	}
	return c
}

// RequestCost is the token cost of a Request, broken down by segment.
//
// The breakdown is the point: a caller trimming to fit needs to know *where*
// the tokens are, and a single total cannot tell you whether to drop history or
// drop retrieved chunks.
type RequestCost struct {
	// Static is content the caller marked immutable for the session
	// (Message.Static, or the system role). Never a candidate for trimming:
	// it is the prefix provider-side caching is anchored to.
	Static int

	// History is every other message except the newest.
	History int

	// Newest is the final non-system message, when it exists.
	Newest int

	// Query is Request.Query, counted separately because a caller may express
	// the current turn there, in Messages, or in both.
	Query int

	// Chunks is the retrieved context.
	Chunks int

	// Total is the sum. It is what a Policy budget is compared against.
	Total int
}

// CountRequest measures a Request segment by segment.
//
// A counter error aborts with that error rather than returning a partial cost;
// a half-counted request would silently under-report and cause over-trimming,
// which is worse than not trimming at all.
func CountRequest(ctx context.Context, counter TokenCounter, req *pipeline.Request) (RequestCost, error) {
	var cost RequestCost
	if req == nil {
		return cost, nil
	}
	c := Or(counter)

	count := func(text string) (int, error) {
		if text == "" {
			return 0, nil
		}
		return c.CountTokens(ctx, text)
	}

	newest := NewestMessage(req.Messages)
	for i, m := range req.Messages {
		n, err := count(m.Content)
		if err != nil {
			return RequestCost{}, err
		}
		switch {
		case IsStatic(m):
			cost.Static += n
		case i == newest:
			cost.Newest += n
		default:
			cost.History += n
		}
	}

	// Query is charged only when it is NOT already present as the newest
	// message. Both provider adapters send the current turn exactly once in
	// that shape, so counting it twice over-reports the request the provider
	// will actually receive — and over-reporting causes history to be dropped
	// that the real request had room for.
	if !queryDuplicatesNewest(req, newest) {
		n, err := count(req.Query)
		if err != nil {
			return RequestCost{}, err
		}
		cost.Query = n
	}

	for _, ch := range req.RetrievedChunks {
		n, err := count(ch.Content + ch.SourceURL)
		if err != nil {
			return RequestCost{}, err
		}
		cost.Chunks += n
	}

	cost.Total = cost.Static + cost.History + cost.Newest + cost.Query + cost.Chunks
	return cost, nil
}

// queryDuplicatesNewest reports whether Request.Query restates the newest
// message verbatim — the shape both provider adapters collapse to a single
// wire message.
func queryDuplicatesNewest(req *pipeline.Request, newest int) bool {
	return req.Query != "" && newest >= 0 && req.Messages[newest].Content == req.Query
}

// IsStatic reports whether a message belongs to the immutable prefix: either
// the caller marked it, or it is a system message.
//
// The two are treated alike because cache.Aligner already treats them alike;
// anything else would let trimming move content the aligner considers stable.
func IsStatic(m pipeline.Message) bool {
	return m.Static || strings.EqualFold(m.Role, "system")
}

// NewestMessage returns the index of the last non-system message, or -1.
// That message is the current turn and is never a trimming candidate.
func NewestMessage(msgs []pipeline.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if !strings.EqualFold(msgs[i].Role, "system") {
			return i
		}
	}
	return -1
}
