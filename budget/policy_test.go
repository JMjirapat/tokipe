package budget

import (
	"testing"

	"github.com/JMjirapat/tokipe/pipeline"
)

func TestPolicyBudgetFor(t *testing.T) {
	p := Policy{RoutineStepBudget: 1, NewQuestionBudget: 2, ErrorRecoveryBudget: 3}

	tests := []struct {
		name string
		turn pipeline.TurnType
		want int
	}{
		{"routine resume", pipeline.TurnRoutineResume, 1},
		{"new question", pipeline.TurnNewQuestion, 2},
		{"error recovery", pipeline.TurnErrorRecovery, 3},
		{"unknown falls back to new question", pipeline.TurnUnknown, 2},
		{"out of range falls back to new question", pipeline.TurnType(99), 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.BudgetFor(tc.turn); got != tc.want {
				t.Fatalf("BudgetFor(%v) = %d, want %d", tc.turn, got, tc.want)
			}
		})
	}
}

func TestDefaultPolicyOrdering(t *testing.T) {
	p := DefaultPolicy()
	if !(p.RoutineStepBudget < p.NewQuestionBudget && p.NewQuestionBudget < p.ErrorRecoveryBudget) {
		t.Fatalf("default budgets should be routine < new question < error recovery, got %+v", p)
	}
	if p.BudgetFor(pipeline.TurnUnknown) != p.NewQuestionBudget {
		t.Fatal("TurnUnknown must map to NewQuestionBudget")
	}
}

func TestZeroPolicyIsZero(t *testing.T) {
	var p Policy
	for _, turn := range []pipeline.TurnType{
		pipeline.TurnUnknown, pipeline.TurnNewQuestion,
		pipeline.TurnRoutineResume, pipeline.TurnErrorRecovery,
	} {
		if got := p.BudgetFor(turn); got != 0 {
			t.Fatalf("zero Policy BudgetFor(%v) = %d, want 0", turn, got)
		}
	}
}

func TestTurnClassifierClassify(t *testing.T) {
	tests := []struct {
		name string
		req  *pipeline.Request
		want pipeline.TurnType
	}{
		{
			name: "nil request",
			req:  nil,
			want: pipeline.TurnNewQuestion,
		},
		{
			name: "plain query",
			req:  &pipeline.Request{Query: "what does this function return?"},
			want: pipeline.TurnNewQuestion,
		},
		{
			name: "empty request",
			req:  &pipeline.Request{},
			want: pipeline.TurnNewQuestion,
		},
		{
			name: "tool calls present",
			req: &pipeline.Request{
				Query:     "continue",
				ToolCalls: []pipeline.ToolCall{{Name: "read_file"}},
			},
			want: pipeline.TurnRoutineResume,
		},
		{
			name: "latest message is a tool result",
			req: &pipeline.Request{
				Messages: []pipeline.Message{
					{Role: "user", Content: "read the config"},
					{Role: "tool", Content: "port = 8080"},
				},
			},
			want: pipeline.TurnRoutineResume,
		},
		{
			name: "tool role matched case-insensitively",
			req: &pipeline.Request{
				Messages: []pipeline.Message{{Role: "TOOL_RESULT", Content: "ok"}},
			},
			want: pipeline.TurnRoutineResume,
		},
		{
			name: "latest message signals an error",
			req: &pipeline.Request{
				Messages: []pipeline.Message{
					{Role: "user", Content: "run the tests"},
					{Role: "user", Content: "That failed with a panic: index out of range"},
				},
			},
			want: pipeline.TurnErrorRecovery,
		},
		{
			name: "retry phrasing in the query",
			req:  &pipeline.Request{Query: "please try again, it doesn't work"},
			want: pipeline.TurnErrorRecovery,
		},
		{
			name: "errored tool result outranks routine resume",
			req: &pipeline.Request{
				ToolCalls: []pipeline.ToolCall{{Name: "build"}},
				Messages: []pipeline.Message{
					{Role: "tool", Content: "Traceback (most recent call last)"},
				},
			},
			want: pipeline.TurnErrorRecovery,
		},
		{
			name: "old error does not classify a fresh turn",
			req: &pipeline.Request{
				Messages: []pipeline.Message{
					{Role: "user", Content: "there was an error earlier"},
					{Role: "assistant", Content: "fixed it"},
					{Role: "user", Content: "now add a README section"},
				},
			},
			want: pipeline.TurnNewQuestion,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.req); got != tc.want {
				t.Fatalf("Classify() = %v, want %v", got, tc.want)
			}
			if got := NewTurnClassifier().Classify(tc.req); got != tc.want {
				t.Fatalf("NewTurnClassifier().Classify() = %v, want %v", got, tc.want)
			}
			// The zero value must behave like the configured default.
			var zero TurnClassifier
			if got := zero.Classify(tc.req); got != tc.want {
				t.Fatalf("zero TurnClassifier.Classify() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTurnClassifierCustomMarkersAndRoles(t *testing.T) {
	c := TurnClassifier{
		ErrorMarkers: []string{"kaputt"},
		ToolRoles:    []string{"werkzeug"},
	}

	if got := c.Classify(&pipeline.Request{Query: "everything is kaputt"}); got != pipeline.TurnErrorRecovery {
		t.Fatalf("custom marker: got %v", got)
	}
	// A default marker must no longer fire once markers are overridden.
	if got := c.Classify(&pipeline.Request{Query: "it failed"}); got != pipeline.TurnNewQuestion {
		t.Fatalf("overridden markers: got %v, want TurnNewQuestion", got)
	}
	if got := c.Classify(&pipeline.Request{
		Messages: []pipeline.Message{{Role: "werkzeug", Content: "ok"}},
	}); got != pipeline.TurnRoutineResume {
		t.Fatalf("custom tool role: got %v", got)
	}
	if got := c.Classify(&pipeline.Request{
		Messages: []pipeline.Message{{Role: "tool", Content: "ok"}},
	}); got != pipeline.TurnNewQuestion {
		t.Fatalf("overridden roles: got %v, want TurnNewQuestion", got)
	}
}

// Classification feeds a budget: the whole point of the helper.
func TestClassifyThenBudget(t *testing.T) {
	p := DefaultPolicy()
	req := &pipeline.Request{
		ToolCalls: []pipeline.ToolCall{{Name: "grep"}},
		Messages:  []pipeline.Message{{Role: "tool", Content: "3 matches"}},
	}
	req.TurnType = Classify(req)
	if got := p.BudgetFor(req.TurnType); got != p.RoutineStepBudget {
		t.Fatalf("BudgetFor(classified) = %d, want %d", got, p.RoutineStepBudget)
	}
}
