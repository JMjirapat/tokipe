package pipeline_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/JMjirapat/tokipe/pipeline"
)

// The ordinal is an implementation detail of a const block; the name is the
// contract. A wire format that leaked the ordinal would make reordering the
// constants a breaking change for every external caller.
func TestTurnTypeMarshalsAsItsName(t *testing.T) {
	for _, tc := range []struct {
		turn pipeline.TurnType
		want string
	}{
		{pipeline.TurnUnknown, `"unknown"`},
		{pipeline.TurnNewQuestion, `"new_question"`},
		{pipeline.TurnRoutineResume, `"routine_resume"`},
		{pipeline.TurnErrorRecovery, `"error_recovery"`},
	} {
		t.Run(tc.want, func(t *testing.T) {
			got, err := json.Marshal(tc.turn)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal = %s, want %s", got, tc.want)
			}

			var back pipeline.TurnType
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if back != tc.turn {
				t.Errorf("round trip = %v, want %v", back, tc.turn)
			}
		})
	}
}

func TestTurnTypeUnmarshal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		want    pipeline.TurnType
		wantErr string
	}{
		{name: "name", input: `"error_recovery"`, want: pipeline.TurnErrorRecovery},
		{name: "empty string is unknown", input: `""`, want: pipeline.TurnUnknown},
		{name: "ordinal still accepted", input: `2`, want: pipeline.TurnRoutineResume},
		{name: "typo is rejected", input: `"new_qeustion"`, wantErr: "unknown turn type"},
		{name: "out of range ordinal", input: `99`, wantErr: "out of range"},
		{name: "wrong type", input: `{}`, wantErr: "must be a string or a number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got pipeline.TurnType
			err := json.Unmarshal([]byte(tc.input), &got)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error containing %q, got none", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// A silent typo in a turn type is a silent downgrade to the wrong budget, so a
// Request carrying one must fail to decode rather than arrive as "unknown".
func TestRequestWithBadTurnTypeIsRejected(t *testing.T) {
	var ext pipeline.ExternalRequest
	err := json.Unmarshal([]byte(`{"query":"q","turn_type":"routine-resume"}`), &ext)
	if err == nil {
		t.Fatal("want an error for a misspelled turn type, got none")
	}
	if !strings.Contains(err.Error(), "routine-resume") {
		t.Errorf("error = %q, want it to quote the offending value", err)
	}
}

// The short-circuit key holds a live *Response. It is the answer, and it travels
// as the response — not as a metadata field the far side has to know about.
func TestExternalizeDropsTheShortCircuitKey(t *testing.T) {
	req := &pipeline.Request{Query: "q"}
	req.SetMeta("router.reason", "static_prefix")
	req.SetMeta(pipeline.MetaShortCircuit, &pipeline.Response{Content: "answered"})

	ext := pipeline.Externalize(req)

	if _, ok := ext.Metadata[pipeline.MetaShortCircuit]; ok {
		t.Error("the short-circuit response crossed the boundary as metadata")
	}
	if got := ext.Metadata["router.reason"]; got != "static_prefix" {
		t.Errorf("ordinary metadata = %v, want it preserved", got)
	}
	if _, ok := req.Metadata[pipeline.MetaShortCircuit]; !ok {
		t.Error("Externalize mutated the caller's request")
	}
}

// One unmarshalable value in an open metadata bag must not fail the whole
// response, and must not vanish without explanation either.
func TestExternalizeElidesUnencodableMetadata(t *testing.T) {
	req := &pipeline.Request{Query: "q"}
	req.SetMeta("fine", 42)
	req.SetMeta("broken", make(chan int)) // channels do not marshal

	ext := pipeline.Externalize(req)

	if got := ext.Metadata["fine"]; got != 42 {
		t.Errorf("encodable value = %v, want 42", got)
	}
	got, _ := ext.Metadata["broken"].(string)
	if !strings.HasPrefix(got, "<elided:") {
		t.Errorf("unencodable value = %v, want an elision marker", ext.Metadata["broken"])
	}

	if _, err := json.Marshal(ext); err != nil {
		t.Errorf("the externalized request must always marshal: %v", err)
	}
}

// A request that goes out and comes back must be the same request.
func TestExternalRequestRoundTrip(t *testing.T) {
	req := &pipeline.Request{
		Query:            "why did the deploy fail?",
		Messages:         []pipeline.Message{{Role: "system", Content: "be brief", Static: true}},
		ToolCalls:        []pipeline.ToolCall{{Name: "logs", Args: map[string]any{"since": "1h"}}},
		NeedsRetrieval:   true,
		RetrievedChunks:  []pipeline.Chunk{{Content: "evidence", SourceURL: "u", Similarity: 0.9}},
		TurnType:         pipeline.TurnErrorRecovery,
		CacheBreakpoints: []pipeline.CacheBreakpoint{{AfterMessageIndex: 0, Reason: "static_prefix"}},
	}
	req.SetMeta("preprocess.matched_rule", "none")

	data, err := json.Marshal(pipeline.Externalize(req))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var ext pipeline.ExternalRequest
	if err := json.Unmarshal(data, &ext); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	back := ext.Internalize()

	if back.Query != req.Query || back.TurnType != req.TurnType || back.NeedsRetrieval != req.NeedsRetrieval {
		t.Errorf("scalar fields drifted: %+v", back)
	}
	if len(back.Messages) != 1 || !back.Messages[0].Static {
		t.Errorf("messages drifted: %+v", back.Messages)
	}
	if len(back.CacheBreakpoints) != 1 || back.CacheBreakpoints[0].Reason != "static_prefix" {
		t.Errorf("breakpoints drifted: %+v", back.CacheBreakpoints)
	}
	if len(back.RetrievedChunks) != 1 || back.RetrievedChunks[0].Similarity != 0.9 {
		t.Errorf("chunks drifted: %+v", back.RetrievedChunks)
	}
	if got := back.Metadata["preprocess.matched_rule"]; got != "none" {
		t.Errorf("metadata drifted: %v", got)
	}
}

func TestExternalizeNilIsNil(t *testing.T) {
	if got := pipeline.Externalize(nil); got != nil {
		t.Errorf("Externalize(nil) = %v, want nil", got)
	}
	var ext *pipeline.ExternalRequest
	if got := ext.Internalize(); got != nil {
		t.Errorf("(*ExternalRequest)(nil).Internalize() = %v, want nil", got)
	}
}
