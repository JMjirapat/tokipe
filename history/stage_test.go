package history_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/JMjirapat/tokipe/budget"
	"github.com/JMjirapat/tokipe/history"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
)

func conversation(turns int) []pipeline.Message {
	msgs := []pipeline.Message{{Role: "system", Content: "SYSTEM PROMPT", Static: true}}
	for i := range turns {
		msgs = append(msgs,
			pipeline.Message{Role: "user", Content: fmt.Sprintf("question %d %s", i, strings.Repeat("x ", 20))},
			pipeline.Message{Role: "assistant", Content: fmt.Sprintf("answer %d %s", i, strings.Repeat("y ", 20))},
		)
	}
	return msgs
}

func tokensOf(t *testing.T, req *pipeline.Request) int {
	t.Helper()
	cost, err := budget.CountRequest(context.Background(), budget.CharEstimator{}, req)
	if err != nil {
		t.Fatalf("CountRequest: %v", err)
	}
	return cost.Total
}

func run(t *testing.T, st *history.Stage, req *pipeline.Request) *pipeline.Request {
	t.Helper()
	out, err := st.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return out
}

func TestTrimsToFitTheBudget(t *testing.T) {
	req := &pipeline.Request{
		Query:    "the current question",
		Messages: conversation(20),
		TurnType: pipeline.TurnNewQuestion,
	}
	limit := tokensOf(t, req) / 3

	st := history.NewStage(budget.Policy{NewQuestionBudget: limit})
	out := run(t, st, req)

	if got := tokensOf(t, out); got > limit {
		t.Errorf("after trimming: %d tokens, budget %d", got, limit)
	}
	if out.Metadata[history.MetaDropped] == nil {
		t.Error("the stage should record how many messages it dropped")
	}
}

// The load-bearing property: the static prefix must be byte-identical no matter
// how much is trimmed, because cache.Aligner anchors its breakpoint to it.
func TestStaticPrefixIsNeverTouched(t *testing.T) {
	static := []pipeline.Message{
		{Role: "system", Content: "SYSTEM PROMPT", Static: true},
		{Role: "user", Content: "TOOL DEFINITIONS", Static: true},
		{Role: "system", Content: "POLICY"},
	}
	req := &pipeline.Request{
		Query:    "q",
		Messages: append(append([]pipeline.Message(nil), static...), conversation(30)[1:]...),
		TurnType: pipeline.TurnNewQuestion,
	}

	st := history.NewStage(budget.Policy{NewQuestionBudget: 20}) // brutally small
	out := run(t, st, req)

	var got []pipeline.Message
	for _, m := range out.Messages {
		if budget.IsStatic(m) {
			got = append(got, m)
		}
	}
	if len(got) != len(static) {
		t.Fatalf("static messages: got %d, want %d — trimming removed some", len(got), len(static))
	}
	for i := range static {
		if got[i] != static[i] {
			t.Errorf("static message %d changed:\n got %+v\nwant %+v", i, got[i], static[i])
		}
	}
}

// DoD: a long loop stays under budget and the prefix is identical every turn.
func TestHundredTurnLoopStaysUnderBudgetWithAStablePrefix(t *testing.T) {
	const limit = 900
	st := history.NewStage(budget.Policy{NewQuestionBudget: limit})

	staticPrefix := "SYSTEM PROMPT — stable for the whole session"
	msgs := []pipeline.Message{{Role: "system", Content: staticPrefix, Static: true}}

	var prefixes []string
	var maxSeen int

	for turn := range 100 {
		msgs = append(msgs, pipeline.Message{
			Role:    "user",
			Content: fmt.Sprintf("turn %d: %s", turn, strings.Repeat("q ", 15)),
		})

		req := &pipeline.Request{Messages: msgs, TurnType: pipeline.TurnNewQuestion}
		out := run(t, st, req)

		if got := tokensOf(t, out); got > limit {
			t.Fatalf("turn %d: %d tokens exceeds the %d budget", turn, got, limit)
		} else if got > maxSeen {
			maxSeen = got
		}

		// Record the static prefix as it would be sent this turn.
		var b strings.Builder
		for _, m := range out.Messages {
			if budget.IsStatic(m) {
				b.WriteString(m.Content)
			}
		}
		prefixes = append(prefixes, b.String())

		// The next turn continues from what was actually sent, as a real agent
		// loop does — otherwise the test would silently reset the growth.
		msgs = append(append([]pipeline.Message(nil), out.Messages...),
			pipeline.Message{Role: "assistant", Content: fmt.Sprintf("reply %d %s", turn, strings.Repeat("a ", 10))})
	}

	for i, p := range prefixes {
		if p != prefixes[0] {
			t.Fatalf("turn %d changed the static prefix:\n got %q\nwant %q", i, p, prefixes[0])
		}
	}
	t.Logf("100 turns, peak %d tokens against a %d budget, prefix stable", maxSeen, limit)
}

// Trimming the middle rather than the front is what keeps a provider's
// automatic prefix caching working across turns.
func TestTrimsTheMiddleAndKeepsHeadAndTail(t *testing.T) {
	msgs := []pipeline.Message{{Role: "system", Content: "sys", Static: true}}
	for i := range 20 {
		msgs = append(msgs, pipeline.Message{
			Role:    "user",
			Content: fmt.Sprintf("MARKER-%02d %s", i, strings.Repeat("z ", 20)),
		})
	}
	req := &pipeline.Request{Messages: msgs, TurnType: pipeline.TurnNewQuestion}

	st := history.NewStage(budget.Policy{NewQuestionBudget: tokensOf(t, req) / 4},
		history.WithRetention(2, 3))
	out := run(t, st, req)

	var kept []string
	for _, m := range out.Messages {
		if i := strings.Index(m.Content, "MARKER-"); i >= 0 {
			kept = append(kept, m.Content[i:i+9])
		}
	}
	if len(kept) < 5 {
		t.Fatalf("kept only %v", kept)
	}
	// The two oldest movable turns survive.
	if kept[0] != "MARKER-00" || kept[1] != "MARKER-01" {
		t.Errorf("head not preserved: %v", kept[:2])
	}
	// The newest turn survives, and so do the tail turns before it.
	if last := kept[len(kept)-1]; last != "MARKER-19" {
		t.Errorf("newest message not preserved: %v", last)
	}
	// Something in the middle is gone.
	if strings.Contains(strings.Join(kept, ","), "MARKER-09") {
		t.Errorf("expected middle turns to be dropped, got %v", kept)
	}
}

func TestNewestMessageIsNeverDropped(t *testing.T) {
	msgs := append(conversation(30), pipeline.Message{Role: "user", Content: "THE CURRENT QUESTION"})
	req := &pipeline.Request{Messages: msgs, TurnType: pipeline.TurnNewQuestion}

	st := history.NewStage(budget.Policy{NewQuestionBudget: 10})
	out := run(t, st, req)

	last := out.Messages[len(out.Messages)-1]
	if last.Content != "THE CURRENT QUESTION" {
		t.Fatalf("last message = %q, want the current turn preserved", last.Content)
	}
}

func TestWithinBudgetIsLeftExactlyAlone(t *testing.T) {
	req := &pipeline.Request{Query: "q", Messages: conversation(3), TurnType: pipeline.TurnNewQuestion}
	before := len(req.Messages)

	rec := metrics.NewInMemory()
	st := history.NewStage(budget.Policy{NewQuestionBudget: 100_000}, history.WithMetrics(rec))
	out := run(t, st, req)

	if out != req {
		t.Error("an in-budget request should be returned unchanged, not copied")
	}
	if len(out.Messages) != before {
		t.Errorf("messages = %d, want %d untouched", len(out.Messages), before)
	}
	if rec.Count(history.MetricSkipped) != 1 {
		t.Error("a skipped turn should be counted")
	}
}

func TestZeroBudgetMeansUnlimited(t *testing.T) {
	req := &pipeline.Request{Messages: conversation(50), TurnType: pipeline.TurnNewQuestion}
	before := len(req.Messages)

	// An unset budget must not be read as "allow nothing".
	st := history.NewStage(budget.Policy{})
	out := run(t, st, req)

	if len(out.Messages) != before {
		t.Errorf("messages = %d, want %d — an unset budget must not trim", len(out.Messages), before)
	}
}

// The stage must not mutate the caller's slice; an agent loop holds that history.
func TestCallerSliceIsNotMutated(t *testing.T) {
	msgs := conversation(20)
	original := append([]pipeline.Message(nil), msgs...)
	req := &pipeline.Request{Messages: msgs, TurnType: pipeline.TurnNewQuestion}

	st := history.NewStage(budget.Policy{NewQuestionBudget: 100})
	out := run(t, st, req)

	if len(out.Messages) >= len(original) {
		t.Fatal("nothing was trimmed; the test proves nothing")
	}
	// req.Messages now points at a new slice; the caller's `msgs` must not have
	// been written through.
	for i := range original {
		if msgs[i] != original[i] {
			t.Fatalf("the caller's slice was mutated at %d", i)
		}
	}
}

type failingCounter struct{ err error }

func (f failingCounter) CountTokens(context.Context, string) (int, error) { return 0, f.err }

type panicCounter struct{}

func (panicCounter) CountTokens(context.Context, string) (int, error) { panic("counter panic") }

func TestCounterFailureLeavesTheRequestAlone(t *testing.T) {
	cases := map[string]budget.TokenCounter{
		"error": failingCounter{err: errors.New("tokenizer down")},
		"panic": panicCounter{},
	}
	for name, counter := range cases {
		t.Run(name, func(t *testing.T) {
			req := &pipeline.Request{Messages: conversation(30), TurnType: pipeline.TurnNewQuestion}
			before := len(req.Messages)

			st := history.NewStage(budget.Policy{NewQuestionBudget: 50}, history.WithCounter(counter))
			out, err := st.Process(context.Background(), req)
			if err != nil {
				t.Fatalf("a broken counter must not fail the turn: %v", err)
			}
			if len(out.Messages) != before {
				t.Errorf("messages = %d, want %d untouched when counting fails", len(out.Messages), before)
			}
		})
	}
}

func TestSummarizerReplacesDroppedTurns(t *testing.T) {
	req := &pipeline.Request{Messages: conversation(20), TurnType: pipeline.TurnNewQuestion}
	st := history.NewStage(budget.Policy{NewQuestionBudget: tokensOf(t, req) / 3},
		history.WithSummarizer(history.ElisionSummarizer()))
	out := run(t, st, req)

	found := false
	for _, m := range out.Messages {
		if strings.Contains(m.Content, "earlier turns omitted") {
			found = true
			if m.Static {
				t.Error("a summary must never be marked static — it changes every turn")
			}
		}
	}
	if !found {
		t.Errorf("no elision note was inserted: %+v", out.Messages)
	}
}

// A summary that does not shrink the prompt is a pure loss, and one that fails
// must degrade to plain dropping.
func TestUselessOrBrokenSummarizerFallsBackToDropping(t *testing.T) {
	cases := map[string]history.Summarizer{
		"summary is larger than what it replaced": history.FuncSummarizer(
			func(context.Context, []pipeline.Message) (pipeline.Message, error) {
				return pipeline.Message{Role: "user", Content: strings.Repeat("bloat ", 5000)}, nil
			}),
		"summarizer errors": history.FuncSummarizer(
			func(context.Context, []pipeline.Message) (pipeline.Message, error) {
				return pipeline.Message{}, errors.New("model unavailable")
			}),
		"summarizer panics": history.FuncSummarizer(
			func(context.Context, []pipeline.Message) (pipeline.Message, error) {
				panic("summarizer panic")
			}),
		"summary is empty": history.FuncSummarizer(
			func(context.Context, []pipeline.Message) (pipeline.Message, error) {
				return pipeline.Message{Role: "user", Content: "   "}, nil
			}),
	}
	for name, sum := range cases {
		t.Run(name, func(t *testing.T) {
			req := &pipeline.Request{Messages: conversation(20), TurnType: pipeline.TurnNewQuestion}
			limit := tokensOf(t, req) / 3

			st := history.NewStage(budget.Policy{NewQuestionBudget: limit}, history.WithSummarizer(sum))
			out, err := st.Process(context.Background(), req)
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			if got := tokensOf(t, out); got > limit {
				t.Errorf("%d tokens exceeds the %d budget; the bad summary was kept", got, limit)
			}
		})
	}
}

// When messages alone cannot fit the budget, chunks go next — least similar
// first, since they are dynamic content that is never cached anyway.
func TestChunksAreTrimmedLeastSimilarFirst(t *testing.T) {
	req := &pipeline.Request{
		Messages: conversation(2),
		TurnType: pipeline.TurnNewQuestion,
		RetrievedChunks: []pipeline.Chunk{
			{Content: strings.Repeat("low ", 40), SourceURL: "low", Similarity: 0.10},
			{Content: strings.Repeat("high ", 40), SourceURL: "high", Similarity: 0.95},
			{Content: strings.Repeat("mid ", 40), SourceURL: "mid", Similarity: 0.50},
		},
	}
	// Captured before Process: the stage mutates req in place, so comparing
	// out against req afterwards would compare a pointer with itself.
	chunksBefore := len(req.RetrievedChunks)

	st := history.NewStage(budget.Policy{NewQuestionBudget: 120}, history.WithRetention(0, 1))
	out := run(t, st, req)

	if len(out.RetrievedChunks) == chunksBefore {
		t.Fatalf("no chunks were trimmed (still %d)", chunksBefore)
	}
	// Whatever survives, the most relevant chunk must be among it.
	var urls []string
	for _, ch := range out.RetrievedChunks {
		urls = append(urls, ch.SourceURL)
	}
	if !strings.Contains(strings.Join(urls, ","), "high") {
		t.Errorf("the most similar chunk was dropped first: kept %v", urls)
	}
}

func TestBudgetVariesByTurnType(t *testing.T) {
	policy := budget.Policy{RoutineStepBudget: 60, NewQuestionBudget: 200, ErrorRecoveryBudget: 100_000}
	st := history.NewStage(policy)

	sizes := map[pipeline.TurnType]int{}
	for _, tt := range []pipeline.TurnType{
		pipeline.TurnRoutineResume, pipeline.TurnNewQuestion, pipeline.TurnErrorRecovery,
	} {
		req := &pipeline.Request{Messages: conversation(20), TurnType: tt}
		sizes[tt] = len(run(t, st, req).Messages)
	}

	if sizes[pipeline.TurnRoutineResume] > sizes[pipeline.TurnNewQuestion] {
		t.Errorf("the tightest budget kept more messages: %v", sizes)
	}
	if sizes[pipeline.TurnErrorRecovery] <= sizes[pipeline.TurnNewQuestion] {
		t.Errorf("the most generous budget did not keep the most: %v", sizes)
	}
}

func TestNothingTrimmableIsReportedNotForced(t *testing.T) {
	// Only static content and one current turn: nothing may legally be dropped.
	req := &pipeline.Request{
		Messages: []pipeline.Message{
			{Role: "system", Content: strings.Repeat("huge ", 500), Static: true},
			{Role: "user", Content: "current"},
		},
		TurnType: pipeline.TurnNewQuestion,
	}
	rec := metrics.NewInMemory()
	st := history.NewStage(budget.Policy{NewQuestionBudget: 10}, history.WithMetrics(rec))
	out := run(t, st, req)

	if len(out.Messages) != 2 {
		t.Errorf("messages = %d; static content and the current turn must survive", len(out.Messages))
	}
	if rec.Count(history.MetricOverBudget) == 0 {
		t.Error("an unmeetable budget must be reported, not silently ignored")
	}
}

func TestImplementsPipelineStage(t *testing.T) {
	var s pipeline.Stage = history.NewStage(budget.DefaultPolicy())
	if s.Name() != "history" {
		t.Errorf("Name = %q", s.Name())
	}
	if out, err := s.Process(context.Background(), nil); err != nil || out != nil {
		t.Errorf("a nil request should pass through: %v %v", out, err)
	}
}
