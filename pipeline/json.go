package pipeline

import (
	"encoding/json"
	"fmt"
)

// This file exists for one reason: a Request that crosses a process boundary.
//
// In-process, TurnType is an int and nobody notices. Encoded as JSON for an
// agent runtime on the other end of a socket, an int is a trap — TurnRoutineResume
// is 2 today because of where it sits in a const block, and a caller who hard-codes
// 2 is depending on declaration order rather than meaning. The names are the
// stable contract; the numbers never were. So TurnType marshals as its String()
// form and parses back from it.
//
// Unmarshal still accepts a number, because a client that round-trips a value it
// received from an older build should not break on it. Marshal only ever emits
// the name.

// ParseTurnType is the inverse of TurnType.String. It reports an error for an
// unrecognised name rather than silently returning TurnUnknown: over a wire
// protocol a typo'd "new_qeustion" that quietly becomes "unknown" is a silent
// downgrade to the wrong budget, which is exactly the kind of failure that only
// shows up on the bill.
func ParseTurnType(s string) (TurnType, error) {
	switch s {
	case "unknown", "":
		return TurnUnknown, nil
	case "new_question":
		return TurnNewQuestion, nil
	case "routine_resume":
		return TurnRoutineResume, nil
	case "error_recovery":
		return TurnErrorRecovery, nil
	default:
		return TurnUnknown, fmt.Errorf("pipeline: unknown turn type %q", s)
	}
}

// MarshalJSON emits the stable name, never the ordinal.
func (t TurnType) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

// UnmarshalJSON accepts the name, and tolerates the ordinal for compatibility
// with anything that captured a raw int from an earlier encoding.
func (t *TurnType) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := ParseTurnType(s)
		if err != nil {
			return err
		}
		*t = parsed
		return nil
	}

	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("pipeline: turn type must be a string or a number, got %s", b)
	}
	if n < int(TurnUnknown) || n > int(TurnErrorRecovery) {
		return fmt.Errorf("pipeline: turn type ordinal %d is out of range", n)
	}
	*t = TurnType(n)
	return nil
}

// ExternalRequest is a Request stripped of everything that cannot safely leave
// the process.
//
// Metadata is an open bag of `any`. In-process that is a feature; on the way out
// it is two problems. First, a stage may have parked a live *Response there under
// MetaShortCircuit, and a *Response is not something a caller on the far side of
// a socket has any business receiving as "metadata". Second, an arbitrary value
// put there by a caller's own stage may not marshal at all, and one unmarshalable
// value must not fail the whole response.
//
// So the boundary is explicit: values that survive a JSON round trip are kept,
// the reserved short-circuit key is dropped, and anything else is replaced by a
// string describing what was elided. Nothing is silently lost.
type ExternalRequest struct {
	Query            string            `json:"query"`
	Messages         []Message         `json:"messages,omitempty"`
	ToolCalls        []ToolCall        `json:"tool_calls,omitempty"`
	NeedsRetrieval   bool              `json:"needs_retrieval,omitempty"`
	RetrievedChunks  []Chunk           `json:"retrieved_chunks,omitempty"`
	TurnType         TurnType          `json:"turn_type,omitempty"`
	CacheBreakpoints []CacheBreakpoint `json:"cache_breakpoints,omitempty"`
	Metadata         map[string]any    `json:"metadata,omitempty"`
}

// Externalize converts req for transmission. It never mutates req.
func Externalize(req *Request) *ExternalRequest {
	if req == nil {
		return nil
	}
	return &ExternalRequest{
		Query:            req.Query,
		Messages:         req.Messages,
		ToolCalls:        req.ToolCalls,
		NeedsRetrieval:   req.NeedsRetrieval,
		RetrievedChunks:  req.RetrievedChunks,
		TurnType:         req.TurnType,
		CacheBreakpoints: req.CacheBreakpoints,
		Metadata:         exportableMetadata(req.Metadata),
	}
}

// Internalize is the inverse of Externalize.
func (e *ExternalRequest) Internalize() *Request {
	if e == nil {
		return nil
	}
	return &Request{
		Query:            e.Query,
		Messages:         e.Messages,
		ToolCalls:        e.ToolCalls,
		NeedsRetrieval:   e.NeedsRetrieval,
		RetrievedChunks:  e.RetrievedChunks,
		TurnType:         e.TurnType,
		CacheBreakpoints: e.CacheBreakpoints,
		Metadata:         exportableMetadata(e.Metadata),
	}
}

// elided is what replaces a metadata value that cannot cross the boundary. It
// names the reason, so an operator reading the far side sees "this was dropped
// and why" rather than an absence they have to explain.
func elided(reason string) string { return "<elided: " + reason + ">" }

func exportableMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if k == MetaShortCircuit {
			// The answer itself travels as the response, not as metadata.
			continue
		}
		if _, err := json.Marshal(v); err != nil {
			out[k] = elided("value is not JSON-encodable")
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
