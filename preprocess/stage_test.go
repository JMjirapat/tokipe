package preprocess_test

import (
	"context"
	"errors"
	"testing"

	"agentkit/metrics"
	"agentkit/pipeline"
	"agentkit/preprocess"
)

// stubRule is a Rule whose behaviour is fully controlled by its fields.
type stubRule struct {
	name    string
	can     bool
	resp    *pipeline.Response
	err     error
	handled int
}

func (r *stubRule) Name() string                     { return r.name }
func (r *stubRule) CanHandle(*pipeline.Request) bool { return r.can }
func (r *stubRule) Handle(*pipeline.Request) (*pipeline.Response, error) {
	r.handled++
	return r.resp, r.err
}

func shortCircuit(t *testing.T, req *pipeline.Request) (*pipeline.Response, bool) {
	t.Helper()
	v, ok := req.Metadata[pipeline.MetaShortCircuit]
	if !ok {
		return nil, false
	}
	resp, ok := v.(*pipeline.Response)
	if !ok {
		t.Fatalf("short-circuit metadata is %T, want *pipeline.Response", v)
	}
	return resp, true
}

func TestProcessMatchingRuleShortCircuits(t *testing.T) {
	rec := metrics.NewInMemory()
	rule := &stubRule{name: "answerer", can: true, resp: &pipeline.Response{Content: "42"}}
	st := preprocess.NewStageWith([]preprocess.Rule{rule}, preprocess.WithMetrics(rec))

	req := &pipeline.Request{Query: "what is 6*7"}
	got, err := st.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process returned error: %v", err)
	}

	resp, ok := shortCircuit(t, got)
	if !ok {
		t.Fatal("expected a short-circuit response in metadata")
	}
	if !resp.ShortCircuited {
		t.Error("ShortCircuited = false, want true")
	}
	if resp.Content != "42" {
		t.Errorf("Content = %q, want %q", resp.Content, "42")
	}
	if got.Metadata[preprocess.MetaMatchedRule] != "answerer" {
		t.Errorf("matched_rule = %v, want answerer", got.Metadata[preprocess.MetaMatchedRule])
	}
	if n := rec.Count(preprocess.CounterShortCircuit); n != 1 {
		t.Errorf("short_circuit counter = %d, want 1", n)
	}
}

func TestProcessShortCircuitFlowsThroughPipeline(t *testing.T) {
	st := preprocess.NewStage(&stubRule{name: "r", can: true, resp: &pipeline.Response{Content: "done"}})
	p := pipeline.New(explodingClient{t}, st)

	resp, err := p.Run(context.Background(), &pipeline.Request{Query: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !resp.ShortCircuited || resp.Content != "done" {
		t.Fatalf("got %+v, want short-circuited 'done'", resp)
	}
}

type explodingClient struct{ t *testing.T }

func (c explodingClient) Name() string { return "exploding" }
func (c explodingClient) Send(context.Context, *pipeline.Request) (*pipeline.Response, error) {
	c.t.Fatal("model client must not be called after a short circuit")
	return nil, nil
}

func TestProcessSkipsFailingRules(t *testing.T) {
	cases := []struct {
		name  string
		first *stubRule
	}{
		{"erroring rule", &stubRule{name: "boom", can: true, err: errors.New("kaboom")}},
		{"nil response", &stubRule{name: "empty", can: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			second := &stubRule{name: "good", can: true, resp: &pipeline.Response{Content: "ok"}}
			st := preprocess.NewStage(tc.first, second)

			req := &pipeline.Request{Query: "q"}
			got, err := st.Process(context.Background(), req)
			if err != nil {
				t.Fatalf("Process must never return an error, got %v", err)
			}
			if tc.first.handled != 1 {
				t.Errorf("first rule Handle calls = %d, want 1", tc.first.handled)
			}
			resp, ok := shortCircuit(t, got)
			if !ok {
				t.Fatal("expected the later rule to short-circuit")
			}
			if resp.Content != "ok" || !resp.ShortCircuited {
				t.Errorf("got %+v, want short-circuited 'ok'", resp)
			}
			if got.Metadata[preprocess.MetaMatchedRule] != "good" {
				t.Errorf("matched_rule = %v, want good", got.Metadata[preprocess.MetaMatchedRule])
			}
		})
	}
}

func TestProcessNoMatchLeavesRequestUnchanged(t *testing.T) {
	skipper := &stubRule{name: "skip", can: false, resp: &pipeline.Response{Content: "never"}}
	st := preprocess.NewStage(nil, skipper)

	req := &pipeline.Request{Query: "hello", Messages: []pipeline.Message{{Role: "user", Content: "hello"}}}
	got, err := st.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got != req {
		t.Error("Process returned a different Request pointer")
	}
	if skipper.handled != 0 {
		t.Errorf("Handle called %d times on a non-matching rule", skipper.handled)
	}
	if len(got.Metadata) != 0 {
		t.Errorf("Metadata = %v, want empty", got.Metadata)
	}
	if got.Query != "hello" || len(got.Messages) != 1 {
		t.Errorf("request mutated: %+v", got)
	}
}

func TestProcessNilAndCancelled(t *testing.T) {
	rule := &stubRule{name: "r", can: true, resp: &pipeline.Response{Content: "x"}}
	st := preprocess.NewStage(rule)

	if got, err := st.Process(context.Background(), nil); got != nil || err != nil {
		t.Errorf("Process(nil) = %v, %v", got, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := &pipeline.Request{Query: "q"}
	got, err := st.Process(ctx, req)
	if err != nil {
		t.Fatalf("cancelled Process must fail open, got %v", err)
	}
	if _, ok := shortCircuit(t, got); ok {
		t.Error("cancelled context should not short-circuit")
	}
	if rule.handled != 0 {
		t.Error("rule ran despite cancelled context")
	}

	//nolint:staticcheck // exercising the nil-context tolerance explicitly
	if _, err := st.Process(nil, &pipeline.Request{}); err != nil {
		t.Errorf("nil ctx: %v", err)
	}
}

func TestStageName(t *testing.T) {
	if got := preprocess.NewStage().Name(); got != "preprocess" {
		t.Errorf("Name() = %q", got)
	}
	if n := len(preprocess.NewStage(&stubRule{name: "a"}).Rules()); n != 1 {
		t.Errorf("Rules() len = %d, want 1", n)
	}
}

func TestRuleFunc(t *testing.T) {
	calls := 0
	f := preprocess.RuleFunc{
		RuleName: "fn",
		Fn: func(*pipeline.Request) (*pipeline.Response, error) {
			calls++
			return &pipeline.Response{Content: "hi"}, nil
		},
	}
	if f.Name() != "fn" {
		t.Errorf("Name = %q", f.Name())
	}
	if !f.CanHandle(&pipeline.Request{}) {
		t.Error("CanHandle with nil Match should be true")
	}
	if _, err := f.Handle(&pipeline.Request{}); err != nil || calls != 1 {
		t.Errorf("Handle: err=%v calls=%d", err, calls)
	}

	matched := preprocess.RuleFunc{RuleName: "m", Match: func(*pipeline.Request) bool { return false }}
	if matched.CanHandle(&pipeline.Request{}) {
		t.Error("Match=false should make CanHandle false")
	}
	if resp, err := matched.Handle(&pipeline.Request{}); resp != nil || err != nil {
		t.Errorf("nil Fn should return nil,nil; got %v,%v", resp, err)
	}
}

func TestRegistryOrderAndOverride(t *testing.T) {
	a := &stubRule{name: "a", can: false}
	b := &stubRule{name: "b", can: true, resp: &pipeline.Response{Content: "from-b"}}
	reg := preprocess.NewRegistry(a, b)

	// Nil rules and unnamed rules are ignored.
	reg.Register(nil, &stubRule{name: ""})
	if reg.Len() != 2 {
		t.Fatalf("Len = %d, want 2", reg.Len())
	}
	if names := reg.Names(); names[0] != "a" || names[1] != "b" {
		t.Fatalf("Names = %v", names)
	}

	// Override keeps position.
	replacement := &stubRule{name: "a", can: true, resp: &pipeline.Response{Content: "from-a"}}
	reg.Register(replacement)
	if reg.Len() != 2 || reg.Names()[0] != "a" {
		t.Fatalf("override changed order: %v", reg.Names())
	}
	if got, ok := reg.Lookup("a"); !ok || got != preprocess.Rule(replacement) {
		t.Fatalf("Lookup(a) = %v, %v", got, ok)
	}
	if _, ok := reg.Lookup("zzz"); ok {
		t.Error("Lookup of missing rule returned ok")
	}

	req := &pipeline.Request{Query: "q"}
	got, err := reg.Stage().Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	resp, ok := shortCircuit(t, got)
	if !ok || resp.Content != "from-a" {
		t.Fatalf("first registered rule should win, got %v (ok=%v)", resp, ok)
	}

	if !reg.Remove("a") || reg.Remove("a") {
		t.Error("Remove semantics wrong")
	}
	if len(reg.Rules()) != 1 || reg.Rules()[0].Name() != "b" {
		t.Errorf("Rules after remove = %v", reg.Names())
	}
}

func TestRegistryStageIsSnapshot(t *testing.T) {
	reg := preprocess.NewRegistry(&stubRule{name: "a", can: false})
	st := reg.Stage(preprocess.WithMetrics(nil))
	reg.Register(&stubRule{name: "b", can: true, resp: &pipeline.Response{Content: "late"}})

	got, _ := st.Process(context.Background(), &pipeline.Request{})
	if _, ok := shortCircuit(t, got); ok {
		t.Error("rules registered after Stage() must not affect it")
	}
}

func TestWithRulesOption(t *testing.T) {
	st := preprocess.NewStageWith(nil, preprocess.WithRules(&stubRule{
		name: "opt", can: true, resp: &pipeline.Response{Content: "yes"},
	}), nil)
	got, _ := st.Process(context.Background(), &pipeline.Request{})
	if resp, ok := shortCircuit(t, got); !ok || resp.Content != "yes" {
		t.Fatalf("WithRules did not register the rule: %v %v", resp, ok)
	}
}
