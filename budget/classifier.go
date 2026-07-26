package budget

import (
	"strings"

	"github.com/JMjirapat/tokipe/pipeline"
)

// TurnClassifier guesses a Request's pipeline.TurnType from its shape.
//
// It is a *helper*, not a stage: docs/spec.md §2.4.8 makes classification the
// caller's responsibility (set Request.TurnType before Pipeline.Run). Nothing
// in agentkit calls this for you. Use it when you have no better signal and
// accept a heuristic; ignore it when your orchestrator already knows what kind
// of turn it is issuing.
//
// The rules, in strict precedence order:
//
//  1. The latest message (or Query, when there are no messages) contains an
//     error/retry marker  → TurnErrorRecovery.
//  2. The Request carries ToolCalls, or its latest message is a tool result
//     (a message whose Role is one of ToolRoles) → TurnRoutineResume.
//  3. Anything else → TurnNewQuestion.
//
// Rule 1 outranks rule 2 deliberately: a tool result that came back with a
// stack trace is a recovery turn, and recovery is the case where starving the
// model of context is most expensive. A tool result that succeeded still falls
// through to rule 2.
//
// The zero TurnClassifier is usable and behaves like NewTurnClassifier().
type TurnClassifier struct {
	// ErrorMarkers are lowercase substrings that mark a message as describing
	// a failure. Empty means DefaultErrorMarkers.
	ErrorMarkers []string

	// ToolRoles are Message.Role values that identify a tool-result message.
	// Empty means DefaultToolRoles.
	ToolRoles []string
}

// DefaultErrorMarkers is the marker set used when TurnClassifier.ErrorMarkers
// is empty. Matching is case-insensitive substring matching over the latest
// message only, which keeps a single mention of "error" deep in an old turn
// from re-classifying the whole conversation.
func DefaultErrorMarkers() []string {
	return []string{
		"error", "exception", "traceback", "stack trace",
		"panic:", "failed", "failure",
		"retry", "try again",
		"didn't work", "doesn't work", "did not work", "does not work",
	}
}

// DefaultToolRoles is the role set treated as "this message is a tool result".
// pipeline.Message documents Role as user/assistant/system, but tool-calling
// transports commonly add one of these.
func DefaultToolRoles() []string {
	return []string{"tool", "tool_result", "function", "function_result"}
}

// NewTurnClassifier returns a classifier with the default markers and roles.
func NewTurnClassifier() TurnClassifier {
	return TurnClassifier{
		ErrorMarkers: DefaultErrorMarkers(),
		ToolRoles:    DefaultToolRoles(),
	}
}

// Classify applies the documented rules to req. A nil Request classifies as
// TurnNewQuestion, the same conservative default BudgetFor uses.
func (c TurnClassifier) Classify(req *pipeline.Request) pipeline.TurnType {
	if req == nil {
		return pipeline.TurnNewQuestion
	}

	markers := c.ErrorMarkers
	if len(markers) == 0 {
		markers = DefaultErrorMarkers()
	}
	roles := c.ToolRoles
	if len(roles) == 0 {
		roles = DefaultToolRoles()
	}

	var latest pipeline.Message
	if n := len(req.Messages); n > 0 {
		latest = req.Messages[n-1]
	} else {
		latest = pipeline.Message{Role: "user", Content: req.Query}
	}

	// Rule 1: failure signalled by the latest message.
	lowered := strings.ToLower(latest.Content)
	for _, m := range markers {
		if m != "" && strings.Contains(lowered, m) {
			return pipeline.TurnErrorRecovery
		}
	}

	// Rule 2: mid-routine — an unresolved tool call, or a tool result to resume
	// from.
	if len(req.ToolCalls) > 0 {
		return pipeline.TurnRoutineResume
	}
	for _, r := range roles {
		if r != "" && strings.EqualFold(latest.Role, r) {
			return pipeline.TurnRoutineResume
		}
	}

	// Rule 3.
	return pipeline.TurnNewQuestion
}

// Classify is the package-level shorthand for NewTurnClassifier().Classify.
func Classify(req *pipeline.Request) pipeline.TurnType {
	return NewTurnClassifier().Classify(req)
}
