// Package budget expresses how much context a turn is allowed to spend, keyed
// by what kind of turn it is. It is pure policy: it holds no state, performs no
// I/O, and makes no decisions on the caller's behalf.
//
// See docs/spec.md §2.4.8.
package budget

import "agentkit/pipeline"

// Policy maps turn types to a per-turn context budget, in tokens.
type Policy struct {
	// RoutineStepBudget applies to a turn that resumes an in-flight routine —
	// typically the step right after a tool result came back. The model already
	// has the working context loaded, so this is the tightest budget.
	RoutineStepBudget int

	// NewQuestionBudget applies to a fresh user question. It is also the
	// default for any turn type the policy does not recognise.
	NewQuestionBudget int

	// ErrorRecoveryBudget applies to a turn that is trying to recover from a
	// failure. This is the most generous budget: recovery needs the error, the
	// attempt that produced it, and enough surrounding context to do better.
	ErrorRecoveryBudget int
}

// DefaultPolicy returns budgets suitable for a general-purpose agent on a
// ~200k-context model. They are starting points, not tuned constants:
//
//	routine resume  4000 — the model is mid-task; ship the delta, not the world
//	new question   12000 — a fresh question needs history plus retrieval
//	error recovery 20000 — recovery is where under-budgeting costs the most
func DefaultPolicy() Policy {
	return Policy{
		RoutineStepBudget:   4000,
		NewQuestionBudget:   12000,
		ErrorRecoveryBudget: 20000,
	}
}

// BudgetFor returns the token budget for turn type t. Unknown and
// TurnNewQuestion both yield NewQuestionBudget, so a caller that never
// classifies its turns gets the conservative default rather than zero.
func (p Policy) BudgetFor(t pipeline.TurnType) int {
	switch t {
	case pipeline.TurnRoutineResume:
		return p.RoutineStepBudget
	case pipeline.TurnErrorRecovery:
		return p.ErrorRecoveryBudget
	default:
		return p.NewQuestionBudget
	}
}
