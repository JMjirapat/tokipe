package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"agentkit/pipeline"
)

// Regressions for QA-REPORT.md findings 4 and 5, using the report's own inputs.
//
// Both had the same root cause: cache_control and the retrieved-context block
// were positioned against the logical message index, while Anthropic's wire
// order is "every system block, then every message". The translation now
// happens against the final wire order.

func build(t *testing.T, req *pipeline.Request) apiRequest {
	t.Helper()
	c, err := New(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c.buildRequest(req)
}

// texts flattens the wire payload into the order a reader of the JSON sees:
// system blocks first, then message blocks.
func texts(out apiRequest) []string {
	var got []string
	for _, b := range out.System {
		got = append(got, b.Text)
	}
	for _, m := range out.Messages {
		for _, b := range m.Content {
			got = append(got, b.Text)
		}
	}
	return got
}

// markedIndex reports the position, in wire order, of the last cache_control.
func markedIndex(out apiRequest) int {
	idx, pos := -1, 0
	for _, b := range out.System {
		if b.CacheControl != nil {
			idx = pos
		}
		pos++
	}
	for _, m := range out.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				idx = pos
			}
			pos++
		}
	}
	return idx
}

func TestBreakpointCoversCallerMarkedStaticMessages(t *testing.T) {
	// QA finding 4's exact input: a Static user message ordered before a
	// system message, with the breakpoint at the end of that static segment.
	out := build(t, &pipeline.Request{
		Messages: []pipeline.Message{
			{Role: "user", Content: "static tool definition", Static: true},
			{Role: "system", Content: "system policy"},
			{Role: "user", Content: "current question"},
		},
		CacheBreakpoints: []pipeline.CacheBreakpoint{{AfterMessageIndex: 1, Reason: "static_prefix"}},
	})

	wire := texts(out)
	marked := markedIndex(out)
	if marked < 0 {
		t.Fatal("no cache_control was emitted at all")
	}

	// Everything static must sit at or before the marker.
	for i, text := range wire {
		if text == "static tool definition" && i > marked {
			t.Fatalf("static content at wire position %d is outside the cached prefix (marker at %d)\nwire: %v",
				i, marked, wire)
		}
	}
	if wire[marked] != "static tool definition" {
		t.Errorf("marker sits on %q; it must be on the last static block in wire order\nwire: %v",
			wire[marked], wire)
	}
}

func TestRetrievedContextPrecedesTheNewestMessage(t *testing.T) {
	// QA finding 5's exact input: Query duplicated as the final message, which
	// used to suppress the append and leave the chunks after the question.
	out := build(t, &pipeline.Request{
		Query: "current question",
		Messages: []pipeline.Message{
			{Role: "system", Content: "stable"},
			{Role: "user", Content: "current question"},
		},
		RetrievedChunks: []pipeline.Chunk{{Content: "retrieved evidence"}},
	})

	wire := texts(out)
	evidence, question := -1, -1
	for i, text := range wire {
		if strings.Contains(text, "retrieved evidence") {
			evidence = i
		}
		if text == "current question" {
			question = i
		}
	}
	if evidence < 0 || question < 0 {
		t.Fatalf("missing content in wire: %v", wire)
	}
	if evidence > question {
		t.Fatalf("evidence at %d comes after the question at %d; contract is "+
			"static → history → retrieved → newest\nwire: %v", evidence, question, wire)
	}
}

// The two ways a caller can express the current turn must produce the same
// wire order, and the question must appear exactly once either way.
func TestBothRepresentationsOfTheCurrentTurnAgree(t *testing.T) {
	chunks := []pipeline.Chunk{{Content: "retrieved evidence"}}

	queryOnly := build(t, &pipeline.Request{
		Query:           "current question",
		Messages:        []pipeline.Message{{Role: "system", Content: "stable"}},
		RetrievedChunks: chunks,
	})
	duplicated := build(t, &pipeline.Request{
		Query: "current question",
		Messages: []pipeline.Message{
			{Role: "system", Content: "stable"},
			{Role: "user", Content: "current question"},
		},
		RetrievedChunks: chunks,
	})

	a, b := texts(queryOnly), texts(duplicated)
	if strings.Join(a, "|") != strings.Join(b, "|") {
		t.Fatalf("representations diverge:\n query-only: %v\n duplicated: %v", a, b)
	}

	count := 0
	for _, text := range b {
		if text == "current question" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the question appears %d times, want exactly 1: %v", count, b)
	}
}

// Whatever the reshuffling, the hard rule from spec §2.4.6 still holds.
func TestNoCacheControlOnRetrievedChunks(t *testing.T) {
	out := build(t, &pipeline.Request{
		Query: "q",
		Messages: []pipeline.Message{
			{Role: "system", Content: "stable", Static: true},
			{Role: "user", Content: "history"},
		},
		RetrievedChunks: []pipeline.Chunk{{Content: "dynamic evidence"}},
		CacheBreakpoints: []pipeline.CacheBreakpoint{
			{AfterMessageIndex: 0}, {AfterMessageIndex: 1},
		},
	})

	for _, m := range out.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, "dynamic evidence") && b.CacheControl != nil {
				t.Fatal("a retrieved chunk must never carry cache_control")
			}
			if b.Text == "Retrieved context:" && b.CacheControl != nil {
				t.Fatal("the retrieved-context header must never carry cache_control")
			}
		}
	}

	// And the payload must still be valid JSON with at least one message.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(out.Messages) == 0 {
		t.Fatalf("no messages in payload: %s", raw)
	}
}
