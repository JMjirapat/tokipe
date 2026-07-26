// Package preprocess resolves deterministic requests without invoking an LLM.
//
// A Rule inspects the incoming *pipeline.Request and, if it can answer it on
// its own, produces the final *pipeline.Response. Stage walks its rules in
// order and hands the first successful Response back to the pipeline through
// the reserved pipeline.MetaShortCircuit metadata key.
//
// Fail-open is absolute here: a rule that errors, or that returns a nil
// Response, is treated as "did not match". It never aborts the pipeline.
//
// See docs/spec.md §2.4.1.
package preprocess

import (
	"context"

	"agentkit/internal/safe"
	"agentkit/metrics"
	"agentkit/pipeline"
)

// MetaMatchedRule is the metadata key Stage sets to the name of the rule that
// short-circuited the request.
const MetaMatchedRule = "preprocess.matched_rule"

// CounterShortCircuit is the counter name incremented on every short circuit,
// labelled with rule=<name>.
const CounterShortCircuit = "preprocess.short_circuit"

// Rule can fully resolve certain requests without an LLM call.
type Rule interface {
	// CanHandle reports whether this rule can fully resolve req without an
	// LLM call. It must not mutate req.
	CanHandle(req *pipeline.Request) bool

	// Handle resolves req and returns the final Response. Returning an error
	// or a nil Response means "I could not handle it after all"; Stage then
	// continues with the next rule.
	Handle(req *pipeline.Request) (*pipeline.Response, error)

	// Name identifies the rule in metrics and metadata.
	Name() string
}

// RuleFunc adapts a plain function into a Rule that always claims it can
// handle the request and reports "did not match" by returning a nil Response.
type RuleFunc struct {
	// RuleName is reported by Name.
	RuleName string
	// Match is optional; when nil, CanHandle always returns true.
	Match func(req *pipeline.Request) bool
	// Fn resolves the request.
	Fn func(req *pipeline.Request) (*pipeline.Response, error)
}

func (f RuleFunc) Name() string { return f.RuleName }

func (f RuleFunc) CanHandle(req *pipeline.Request) bool {
	if f.Match == nil {
		return true
	}
	return f.Match(req)
}

func (f RuleFunc) Handle(req *pipeline.Request) (*pipeline.Response, error) {
	if f.Fn == nil {
		return nil, nil
	}
	return f.Fn(req)
}

// Option configures a Stage.
type Option func(*Stage)

// WithMetrics attaches a metrics.Recorder. A nil recorder is safe.
func WithMetrics(r metrics.Recorder) Option {
	return func(s *Stage) { s.metrics = metrics.Or(r) }
}

// WithRules appends rules to the stage, after any passed to NewStage.
func WithRules(rules ...Rule) Option {
	return func(s *Stage) { s.rules = append(s.rules, rules...) }
}

// Stage is the pipeline.Stage implementation that runs the rules in order.
type Stage struct {
	rules   []Rule
	metrics metrics.Recorder
}

// NewStage builds a Stage from an ordered list of rules.
func NewStage(rules ...Rule) *Stage {
	return NewStageWith(rules, nil)
}

// NewStageWith builds a Stage from an ordered list of rules plus options.
// NewStage remains signature-compatible with docs/spec.md §2.4.1.
func NewStageWith(rules []Rule, opts ...Option) *Stage {
	s := &Stage{rules: append([]Rule(nil), rules...), metrics: metrics.Nop{}}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

func (s *Stage) Name() string { return "preprocess" }

// Rules returns the ordered rules the stage will run. The returned slice is a
// copy; mutating it does not affect the stage.
func (s *Stage) Rules() []Rule { return append([]Rule(nil), s.rules...) }

// Process runs each rule in order. The first rule that both claims the request
// and returns a non-nil Response short-circuits the pipeline. Any other
// outcome (no claim, an error, a nil Response) moves on to the next rule.
//
// Process never returns a non-nil error: the whole point of this stage is that
// it can only ever save an LLM call, never cost the caller their request.
func (s *Stage) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	if req == nil {
		return req, nil
	}
	for _, r := range s.rules {
		if r == nil {
			continue
		}
		if ctx != nil && ctx.Err() != nil {
			return req, nil
		}
		// CanHandle and Handle are caller-supplied. A panic in either is a
		// failure like any other and must degrade to "did not match", not
		// take down the turn (spec §2.5.1).
		if !safe.Bool(func() bool { return r.CanHandle(req) }) {
			continue
		}
		resp, err := safe.Value(func() (*pipeline.Response, error) { return r.Handle(req) })
		if err != nil || resp == nil {
			// Fail-open: treat as "did not match" and keep going — but say so,
			// or a permanently broken rule is indistinguishable from one that
			// simply never matches.
			reason := "rule_returned_nil"
			if err != nil {
				reason = "rule_failed"
			}
			metrics.Degrade(s.metrics, metrics.Degradation{
				Stage:  s.Name(),
				Reason: reason,
				Err:    err,
				Detail: safe.Name(r.Name, UnnamedRule),
			})
			continue
		}
		// Name() is caller code too. A panic here would discard a result the
		// rule already produced successfully — identification is never worth
		// failing a turn that had otherwise succeeded.
		name := safe.Name(r.Name, UnnamedRule)

		resp.ShortCircuited = true
		req.SetMeta(pipeline.MetaShortCircuit, resp)
		req.SetMeta(MetaMatchedRule, name)
		metrics.Inc(s.metrics, CounterShortCircuit, map[string]string{"rule": name})
		return req, nil
	}
	return req, nil
}

// UnnamedRule is reported when a Rule's Name method panics or returns "".
const UnnamedRule = "unnamed_rule"
