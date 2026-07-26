package budget_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/JMjirapat/tokipe/budget"
	"github.com/JMjirapat/tokipe/pipeline"
)

func TestCharEstimatorRoundsUpAndCountsRunes(t *testing.T) {
	c := budget.NewCharEstimator()
	cases := map[string]struct {
		in   string
		want int
	}{
		"empty":                 {"", 0},
		"one rune is one token": {"a", 1},
		"four ascii":            {"abcd", 1},
		"five ascii":            {"abcde", 2},
		// The reason it counts runes, not bytes: Thai is 3 bytes per rune, so a
		// byte-based estimate would over-count this by 3x and trim far too hard.
		"thai":  {"สวัสดี", 2},
		"emoji": {"🔑🚀", 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := c.CountTokens(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("CountTokens: %v", err)
			}
			if got != tc.want {
				t.Errorf("CountTokens(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestCharEstimatorRuneVsByteMatters(t *testing.T) {
	thai := strings.Repeat("ก", 400) // 400 runes, 1200 bytes
	got, _ := budget.CharEstimator{}.CountTokens(context.Background(), thai)
	if got != 100 {
		t.Errorf("count = %d, want 100 (runes/4); a byte-based estimate would give 300", got)
	}
}

func TestCharEstimatorInvalidDivisorFallsBackToDefault(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		got, err := budget.CharEstimator{CharsPerToken: n}.CountTokens(context.Background(), "abcdefgh")
		if err != nil {
			t.Fatalf("CountTokens: %v", err)
		}
		if got != 2 {
			t.Errorf("CharsPerToken=%d gave %d, want the default divisor's 2", n, got)
		}
	}
}

func TestOrSubstitutesAnEstimatorForNil(t *testing.T) {
	got, err := budget.Or(nil).CountTokens(context.Background(), "abcd")
	if err != nil || got != 1 {
		t.Errorf("Or(nil) = %d, %v; want a working estimator", got, err)
	}
	custom := budget.CharEstimator{CharsPerToken: 2}
	if budget.Or(custom) != budget.TokenCounter(custom) {
		t.Error("Or should return a non-nil counter unchanged")
	}
}

func TestCountRequestBreaksDownBySegment(t *testing.T) {
	req := &pipeline.Request{
		Query: strings.Repeat("q", 40), // 10
		Messages: []pipeline.Message{
			{Role: "system", Content: strings.Repeat("s", 40)},             // static 10
			{Role: "user", Content: strings.Repeat("t", 40), Static: true}, // static 10
			{Role: "user", Content: strings.Repeat("h", 40)},               // history 10
			{Role: "user", Content: strings.Repeat("n", 40)},               // newest 10
		},
		RetrievedChunks: []pipeline.Chunk{{Content: strings.Repeat("c", 36), SourceURL: "abcd"}}, // 10
	}

	cost, err := budget.CountRequest(context.Background(), budget.CharEstimator{}, req)
	if err != nil {
		t.Fatalf("CountRequest: %v", err)
	}
	if cost.Static != 20 {
		t.Errorf("Static = %d, want 20 (system + caller-marked)", cost.Static)
	}
	if cost.History != 10 {
		t.Errorf("History = %d, want 10", cost.History)
	}
	if cost.Newest != 10 {
		t.Errorf("Newest = %d, want 10", cost.Newest)
	}
	if cost.Query != 10 {
		t.Errorf("Query = %d, want 10", cost.Query)
	}
	if cost.Chunks != 10 {
		t.Errorf("Chunks = %d, want 10 (content + source url)", cost.Chunks)
	}
	if cost.Total != 60 {
		t.Errorf("Total = %d, want 60 (the sum)", cost.Total)
	}
}

// A half-counted request would under-report and cause over-trimming, which is
// worse than not trimming at all — so an error aborts rather than returning a
// partial cost.
func TestCountRequestAbortsOnCounterError(t *testing.T) {
	sentinel := errors.New("tokenizer down")
	cost, err := budget.CountRequest(context.Background(), failing{sentinel}, &pipeline.Request{
		Query:    "q",
		Messages: []pipeline.Message{{Role: "user", Content: "m"}},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the counter's error", err)
	}
	if cost != (budget.RequestCost{}) {
		t.Errorf("cost = %+v, want the zero value rather than a partial count", cost)
	}
}

type failing struct{ err error }

func (f failing) CountTokens(context.Context, string) (int, error) { return 0, f.err }

func TestCountRequestHandlesNilAndEmpty(t *testing.T) {
	if cost, err := budget.CountRequest(context.Background(), nil, nil); err != nil || cost.Total != 0 {
		t.Errorf("nil request = %+v, %v", cost, err)
	}
	// A nil counter must not panic; Or substitutes the estimator.
	if cost, err := budget.CountRequest(context.Background(), nil, &pipeline.Request{Query: "abcd"}); err != nil || cost.Total != 1 {
		t.Errorf("nil counter = %+v, %v", cost, err)
	}
}

func TestIsStaticTreatsSystemAndMarkedAlike(t *testing.T) {
	// Both count as static because cache.Aligner treats them alike; anything
	// else would let trimming move content the aligner considers stable.
	cases := map[pipeline.Message]bool{
		{Role: "system", Content: "x"}:             true,
		{Role: "SYSTEM", Content: "x"}:             true,
		{Role: "user", Content: "x", Static: true}: true,
		{Role: "user", Content: "x"}:               false,
		{Role: "assistant", Content: "x"}:          false,
	}
	for m, want := range cases {
		if got := budget.IsStatic(m); got != want {
			t.Errorf("IsStatic(%+v) = %v, want %v", m, got, want)
		}
	}
}

func TestNewestMessageSkipsSystemMessages(t *testing.T) {
	cases := map[string]struct {
		msgs []pipeline.Message
		want int
	}{
		"none":              {nil, -1},
		"only system":       {[]pipeline.Message{{Role: "system"}}, -1},
		"last is user":      {[]pipeline.Message{{Role: "system"}, {Role: "user"}}, 1},
		"system after user": {[]pipeline.Message{{Role: "user"}, {Role: "system"}}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := budget.NewestMessage(tc.msgs); got != tc.want {
				t.Errorf("NewestMessage = %d, want %d", got, tc.want)
			}
		})
	}
}
