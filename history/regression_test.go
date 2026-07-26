package history_test

import (
	"context"
	"strings"
	"testing"

	"github.com/JMjirapat/tokipe/budget"
	"github.com/JMjirapat/tokipe/history"
	"github.com/JMjirapat/tokipe/pipeline"
)

// QA-REPORT-2 M1: the current turn was charged twice when Query restated the
// newest message — the shape both provider adapters collapse to one wire
// message. Over-counting made the stage drop history the real request had
// room for.
func TestDuplicatedCurrentTurnIsCountedOnce(t *testing.T) {
	q := strings.Repeat("q", 40) // 10 tokens
	req := &pipeline.Request{
		Query:    q,
		Messages: []pipeline.Message{{Role: "user", Content: q}},
	}
	cost, err := budget.CountRequest(context.Background(), budget.CharEstimator{}, req)
	if err != nil {
		t.Fatalf("CountRequest: %v", err)
	}
	if cost.Total != 10 {
		t.Errorf("Total = %d, want 10 — the provider sends this turn once", cost.Total)
	}

	// When Query is genuinely additional, it must still be charged.
	req2 := &pipeline.Request{
		Query:    q,
		Messages: []pipeline.Message{{Role: "user", Content: strings.Repeat("z", 40)}},
	}
	cost2, _ := budget.CountRequest(context.Background(), budget.CharEstimator{}, req2)
	if cost2.Total != 20 {
		t.Errorf("Total = %d, want 20 — a distinct Query is real content", cost2.Total)
	}
}

func TestDuplicatedTurnDoesNotCauseUnnecessaryTrimming(t *testing.T) {
	q := strings.Repeat("q", 40)
	msgs := []pipeline.Message{
		{Role: "system", Content: strings.Repeat("s", 40), Static: true},
		{Role: "user", Content: strings.Repeat("h", 40)},
		{Role: "user", Content: q},
	}
	req := &pipeline.Request{Query: q, Messages: msgs, TurnType: pipeline.TurnNewQuestion}

	// Real cost is 30 (static + history + newest). Budget of 32 fits.
	st := history.NewStage(budget.Policy{NewQuestionBudget: 32})
	out, err := st.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(out.Messages) != 3 {
		t.Errorf("messages = %d, want 3 — nothing needed dropping", len(out.Messages))
	}
}

// QA-REPORT-2 M2: an over-budget request whose only trimmable content is
// retrieved chunks returned early, before the chunk pass. That is the most
// ordinary one-turn RAG shape there is.
func TestChunksAreTrimmedEvenWithNoDroppableMessage(t *testing.T) {
	req := &pipeline.Request{
		Messages: []pipeline.Message{
			{Role: "system", Content: "sys", Static: true},
			{Role: "user", Content: "the current question"},
		},
		TurnType: pipeline.TurnNewQuestion,
		RetrievedChunks: []pipeline.Chunk{
			{Content: strings.Repeat("a ", 200), SourceURL: "low", Similarity: 0.1},
			{Content: strings.Repeat("b ", 200), SourceURL: "high", Similarity: 0.9},
		},
	}
	before := len(req.RetrievedChunks)

	st := history.NewStage(budget.Policy{NewQuestionBudget: 60})
	out, err := st.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(out.RetrievedChunks) >= before {
		t.Fatalf("kept %d chunks; no message was droppable, so chunks had to give", len(out.RetrievedChunks))
	}
	// The most relevant chunk survives.
	if out.RetrievedChunks[0].SourceURL != "high" {
		t.Errorf("kept %q, want the most similar chunk", out.RetrievedChunks[0].SourceURL)
	}
	if out.Metadata[history.MetaDropped] == nil {
		t.Error("the trim should be recorded even when only chunks were dropped")
	}
}
