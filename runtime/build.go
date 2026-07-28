package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/JMjirapat/tokipe/budget"
	"github.com/JMjirapat/tokipe/compress"
	"github.com/JMjirapat/tokipe/config"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/preprocess"
	"github.com/JMjirapat/tokipe/router"
	"github.com/JMjirapat/tokipe/stores"
)

// Parse reads a Spec from JSON.
//
// Unknown fields are rejected. In a library that would be hostile; in a config
// file it is the whole point — "tool_cahce" silently ignored is a cache an
// operator believes is on, and the bill is the only thing that ever tells them
// otherwise.
func Parse(data []byte) (*Spec, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("runtime: parse spec: %w", err)
	}
	// Trailing content usually means two documents got concatenated, which is
	// worth saying out loud rather than silently honouring the first.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("runtime: parse spec: unexpected trailing content after the first JSON document")
	}
	return &s, nil
}

// ParseFile reads a Spec from a file on disk.
func ParseFile(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("runtime: read %s: %w", path, err)
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%w (in %s)", err, path)
	}
	return s, nil
}

// Options resolves a Spec into the functional options the config package
// consumes. Build is the usual entry point; this exists for a caller that wants
// to add Go-only options — a metrics recorder, a custom stage — on top of what
// the document declared.
//
// reg may be nil, in which case NewRegistry() is used.
func (s *Spec) Options(reg *Registry) ([]config.Option, error) {
	if s == nil {
		return nil, fmt.Errorf("runtime: nil spec")
	}
	if reg == nil {
		reg = NewRegistry()
	}

	var opts []config.Option
	add := func(o config.Option) { opts = append(opts, o) }

	if p := s.Stages.Preprocess; p != nil {
		rules, err := buildRules(p, reg)
		if err != nil {
			return nil, err
		}
		if len(rules) > 0 {
			add(config.WithPreprocess(rules...))
		}
	}

	if tc := s.Stages.ToolCache; tc != nil {
		return nil, errToolCacheNeedsExecutor
	}

	if r := s.Stages.RAG; r != nil {
		embedder, store, err := buildRAG(r, reg)
		if err != nil {
			return nil, err
		}
		topK := r.TopK
		if topK <= 0 {
			topK = defaultTopK
		}
		add(config.WithRAG(embedder, store, topK))
	}

	if d := s.Stages.Dedupe; d != nil {
		var dopts []compress.DedupeOption
		if d.Threshold != 0 {
			if d.Threshold < 0 || d.Threshold > 1 {
				return nil, fmt.Errorf("runtime: stages.dedupe.threshold must be between 0 and 1, got %v", d.Threshold)
			}
			dopts = append(dopts, compress.WithDedupeThreshold(d.Threshold))
		}
		add(config.WithChunkDedupe(dopts...))
	}

	if c := s.Stages.Compression; c != nil {
		compressors, err := buildCompressors(c)
		if err != nil {
			return nil, err
		}
		add(config.WithCompression(compressors...))
	}

	if h := s.Stages.HistoryBudget; h != nil {
		policy, counter, err := buildHistory(h, reg)
		if err != nil {
			return nil, err
		}
		add(config.WithHistoryBudget(policy, counter))
	}

	// Custom stages before alignment, matching config.WithStage's placement.
	for _, name := range s.Stages.Custom {
		stage, ok := reg.stages[name]
		if !ok {
			return nil, fmt.Errorf("runtime: unknown custom stage %q (registered: %s)", name, known(reg.stages))
		}
		add(config.WithStage(stage))
	}

	if s.Stages.CacheAlignment != nil {
		add(config.WithCacheAlignment())
	}

	if s.Router != nil {
		r, err := buildRouter(s.Router, reg)
		if err != nil {
			return nil, err
		}
		add(config.WithRouter(r))
	}

	return opts, nil
}

// defaultTopK is what an RAG section that omits top_k gets. Five is the figure
// the README's production example uses.
const defaultTopK = 5

// errToolCacheNeedsExecutor explains a limit that is structural rather than
// incidental. The tool cache resolves ToolCalls by *executing the misses*, and
// the executor is a Go function that runs the caller's tools. A document can
// name a cache backend; it cannot contain the code that runs a tool. Rather than
// half-enable the stage — a cache that can only ever miss — Build says so.
var errToolCacheNeedsExecutor = fmt.Errorf(
	"runtime: stages.tool_cache cannot be enabled from a document alone: the cache " +
		"needs a tool executor, which is Go code. Build the cache with " +
		"Registry.RegisterCache and pass config.WithToolCache to BuildWith")

func buildRules(p *PreprocessSpec, reg *Registry) ([]preprocess.Rule, error) {
	rules := make([]preprocess.Rule, 0, len(p.Use)+len(p.Rules))
	for _, name := range p.Use {
		rule, ok := reg.rules[name]
		if !ok {
			return nil, fmt.Errorf("runtime: unknown preprocess rule %q (registered: %s)", name, known(reg.rules))
		}
		rules = append(rules, rule)
	}
	for i, spec := range p.Rules {
		if spec.Match == "" {
			return nil, fmt.Errorf("runtime: stages.preprocess.rules[%d]: match is required", i)
		}
		if spec.Respond == "" {
			return nil, fmt.Errorf("runtime: stages.preprocess.rules[%d] (%q): respond is required", i, spec.Match)
		}
		rules = append(rules, exactRule(spec))
	}
	return rules, nil
}

// exactRule turns a declared rule into a preprocess.Rule.
//
// The match is exact by construction. That is a narrow contract, and narrow is
// the point: a rule that claims a request it should not have answers the user
// wrong with no model in the loop to catch it, and a config file is exactly
// where a too-clever pattern would be written by someone who cannot see the
// consequences from there.
func exactRule(spec ExactRuleSpec) preprocess.Rule {
	name := spec.Name
	if name == "" {
		name = "exact:" + spec.Match
	}
	want, respond, fold := spec.Match, spec.Respond, spec.CaseInsensitive

	return preprocess.RuleFunc{
		RuleName: name,
		Match: func(req *pipeline.Request) bool {
			if fold {
				return strings.EqualFold(req.Query, want)
			}
			return req.Query == want
		},
		Fn: func(*pipeline.Request) (*pipeline.Response, error) {
			return &pipeline.Response{Content: respond, ShortCircuited: true}, nil
		},
	}
}

func buildRAG(r *RAGSpec, reg *Registry) (stores.Embedder, stores.VectorStore, error) {
	if r.Embedder == "" || r.Store == "" {
		return nil, nil, fmt.Errorf(
			"runtime: stages.rag needs both embedder and store (registered embedders: %s; stores: %s)",
			known(reg.embedders), known(reg.stores))
	}
	ef, ok := reg.embedders[r.Embedder]
	if !ok {
		return nil, nil, fmt.Errorf("runtime: unknown embedder %q (registered: %s)", r.Embedder, known(reg.embedders))
	}
	sf, ok := reg.stores[r.Store]
	if !ok {
		return nil, nil, fmt.Errorf("runtime: unknown store %q (registered: %s)", r.Store, known(reg.stores))
	}
	e, err := ef(r.Options)
	if err != nil {
		return nil, nil, fmt.Errorf("runtime: embedder %q: %w", r.Embedder, err)
	}
	st, err := sf(r.Options)
	if err != nil {
		return nil, nil, fmt.Errorf("runtime: store %q: %w", r.Store, err)
	}
	return e, st, nil
}

func buildCompressors(c *CompressionSpec) ([]compress.Compressor, error) {
	names := c.Compressors
	switch {
	case c.Preset != "" && len(names) > 0:
		return nil, fmt.Errorf("runtime: stages.compression sets both preset and compressors; pick one")
	case c.Preset == "default":
		names = []string{"json", "text"}
	case c.Preset != "":
		return nil, fmt.Errorf("runtime: unknown compression preset %q (known: default)", c.Preset)
	case len(names) == 0:
		names = []string{"json", "text"}
	}

	out := make([]compress.Compressor, 0, len(names))
	for i, n := range names {
		switch n {
		case "json":
			out = append(out, compress.NewJSONCompressor())
		case "code":
			var opts []compress.CodeOption
			if c.ElideBodies {
				opts = append(opts, compress.WithBodyElision())
			}
			out = append(out, compress.NewCodeCompressor(opts...))
		case "text":
			// The text compressor handles anything, so a compressor after it is
			// unreachable. Silently building a chain where half the entries can
			// never run would be the config-file equivalent of dead code.
			if i != len(names)-1 {
				return nil, fmt.Errorf(
					"runtime: stages.compression lists %q at position %d of %d: it is a catch-all "+
						"and must be last, or the compressors after it can never run", n, i+1, len(names))
			}
			out = append(out, compress.NewTextCompressor())
		default:
			return nil, fmt.Errorf("runtime: unknown compressor %q (known: json, code, text)", n)
		}
	}
	return out, nil
}

func buildHistory(h *HistorySpec, reg *Registry) (budget.Policy, budget.TokenCounter, error) {
	var policy budget.Policy
	switch h.Preset {
	case "default":
		policy = budget.DefaultPolicy()
	case "":
		// no preset; explicit budgets below must supply everything
	default:
		return policy, nil, fmt.Errorf("runtime: unknown history_budget preset %q (known: default)", h.Preset)
	}

	if h.RoutineStep != 0 {
		policy.RoutineStepBudget = h.RoutineStep
	}
	if h.NewQuestion != 0 {
		policy.NewQuestionBudget = h.NewQuestion
	}
	if h.ErrorRecovery != 0 {
		policy.ErrorRecoveryBudget = h.ErrorRecovery
	}

	// config.WithHistoryBudget only enables the stage for a non-zero policy, so
	// an all-zero one is a section that does nothing. Say so instead of starting
	// a pipeline whose declared budget is silently absent.
	if policy == (budget.Policy{}) {
		return policy, nil, fmt.Errorf(
			"runtime: stages.history_budget sets no budgets; use \"preset\": \"default\" or set at least one of " +
				"routine_step, new_question, error_recovery")
	}
	if policy.RoutineStepBudget < 0 || policy.NewQuestionBudget < 0 || policy.ErrorRecoveryBudget < 0 {
		return policy, nil, fmt.Errorf("runtime: stages.history_budget budgets must not be negative")
	}

	if h.Counter == "" {
		return policy, nil, nil // nil counter = character estimator, per config.WithHistoryBudget
	}
	counter, ok := reg.counters[h.Counter]
	if !ok {
		return policy, nil, fmt.Errorf("runtime: unknown token counter %q (registered: %s)", h.Counter, known(reg.counters))
	}
	return policy, counter, nil
}

func buildRouter(rs *RouterSpec, reg *Registry) (pipeline.Router, error) {
	if len(rs.Tiers) == 0 {
		return nil, fmt.Errorf("runtime: router declares no tiers")
	}
	tiers := make([]router.Tier, 0, len(rs.Tiers))
	for i, t := range rs.Tiers {
		client, err := reg.provider(t.Provider)
		if err != nil {
			return nil, fmt.Errorf("runtime: router.tiers[%d]: %w", i, err)
		}
		if t.MaxComplexity <= 0 || t.MaxComplexity > 1 {
			return nil, fmt.Errorf(
				"runtime: router.tiers[%d].max_complexity is %v; it must be greater than 0 and at most 1",
				i, t.MaxComplexity)
		}
		tiers = append(tiers, router.Tier{Client: client, MaxComplexity: t.MaxComplexity})
	}

	// An ascending order is what makes "first tier that accepts it" mean "the
	// cheapest tier that accepts it". Out of order, the expensive tier can sit
	// in front and take everything, and the document would look correct.
	for i := 1; i < len(tiers); i++ {
		if tiers[i].MaxComplexity <= tiers[i-1].MaxComplexity {
			return nil, fmt.Errorf(
				"runtime: router.tiers must be ordered by ascending max_complexity; tier %d (%v) does not exceed tier %d (%v)",
				i, tiers[i].MaxComplexity, i-1, tiers[i-1].MaxComplexity)
		}
	}
	// The last tier is the backstop: anything the cheaper tiers declined lands
	// here, so it has to accept the whole range.
	if last := tiers[len(tiers)-1]; last.MaxComplexity != 1 {
		return nil, fmt.Errorf(
			"runtime: the last router tier must have max_complexity 1 so it can serve requests every other tier declined, got %v",
			last.MaxComplexity)
	}

	return router.NewHeuristicRouter(tiers...), nil
}

// Build assembles the pipeline a Spec describes.
//
// reg may be nil for the built-in registry. extra are options applied after the
// document's, which is how Go-only capabilities join a declared pipeline — a
// metrics recorder, a tool executor, a custom stage:
//
//	kit, err := runtime.Build(spec, reg,
//	    config.WithMetrics(rec),
//	    config.WithToolCache(cache, myExecutor, time.Minute),
//	)
func Build(s *Spec, reg *Registry, extra ...config.Option) (*pipeline.Pipeline, error) {
	if s == nil {
		return nil, fmt.Errorf("runtime: nil spec")
	}
	if reg == nil {
		reg = NewRegistry()
	}
	if s.Provider == nil {
		return nil, fmt.Errorf("runtime: provider is required (it is the default endpoint, and the fallback when a router declines)")
	}

	client, err := reg.provider(*s.Provider)
	if err != nil {
		return nil, err
	}

	opts, err := s.Options(reg)
	if err != nil {
		return nil, err
	}
	opts = append(opts, extra...)

	cfg := config.New(opts...)
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.Nop{}
	}
	return pipeline.NewWithRouter(client, cfg.Router, cfg.Stages()...), nil
}

// BuildFile is ParseFile followed by Build.
func BuildFile(path string, reg *Registry, extra ...config.Option) (*pipeline.Pipeline, error) {
	s, err := ParseFile(path)
	if err != nil {
		return nil, err
	}
	return Build(s, reg, extra...)
}
