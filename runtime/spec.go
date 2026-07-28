// Package runtime builds a tokipe pipeline from a declarative document instead
// of from Go code.
//
// The library's normal surface is functional options: a caller writes
// config.WithRAG(...) and compiles it in. That is the right shape for a Go
// program and the wrong shape for the thing this package exists to serve — an
// agent runtime that other processes talk to, whose operator is not going to
// recompile a binary to raise a TTL.
//
// So the same pipeline is expressible as JSON:
//
//	{
//	  "provider": {"type": "openai", "base_url": "http://localhost:11434/v1"},
//	  "stages": {
//	    "tool_cache": {"backend": "memory", "ttl": "30m"},
//	    "compression": {"preset": "default"},
//	    "cache_alignment": {}
//	  }
//	}
//
// JSON rather than YAML because the core module is standard-library-only and
// that guarantee is worth more than the nicer syntax. See PLAN.md.
//
// # What is deliberately not expressible
//
// Preprocess rules beyond exact-match, custom stages, and hand-written
// embedders are Go functions. A document cannot contain a function, and the
// alternative — a config-file expression language — would be a worse programming
// language than the one already available. Those stay in code, and Registry is
// how a program supplies them to a document-built pipeline.
//
// # Configuration errors do not fail open
//
// The pipeline's own rule is that a broken optimization must never cost a turn.
// This package inverts it: a document that does not parse, names a component
// that is not registered, or sets a field that does not exist is an error
// returned from Build, and nothing starts. Fail-open is right for a dependency
// that breaks at 3am and wrong for a typo an operator can fix in ten seconds —
// silently running a pipeline that is not the one someone wrote down is how a
// disabled cache goes unnoticed for a month.
package runtime

import (
	"encoding/json"
	"fmt"
	"time"
)

// Spec is a whole pipeline, declared.
//
// The zero value is a valid pass-through: no stages enabled, whatever provider
// is named. That mirrors tokipe.New(client) with no options, which the README
// recommends as the baseline to measure everything else against.
type Spec struct {
	// Provider is the default model endpoint. Required unless Router supplies
	// tiers, in which case it is the fallback for a router that declines.
	Provider *ProviderSpec `json:"provider,omitempty"`

	// Stages enables individual optimizations. Absent sections are disabled.
	Stages StagesSpec `json:"stages"`

	// Router, when set, selects among tiered providers per request.
	Router *RouterSpec `json:"router,omitempty"`
}

// ProviderSpec names a model endpoint and how to reach it.
//
// There is no APIKey field, and that is not an oversight: a key in a config file
// is a key in a git repository. Keys are named, not carried — APIKeyEnv holds
// the name of the environment variable to read at build time.
type ProviderSpec struct {
	// Type selects a registered provider factory: "mock", "anthropic",
	// "openai", or "cli" out of the box.
	Type string `json:"type"`

	// Model is the provider's model identifier. Each factory supplies its own
	// default when this is empty.
	Model string `json:"model,omitempty"`

	// BaseURL points an OpenAI-compatible or Anthropic client at a specific
	// server. Empty means the provider's default endpoint.
	BaseURL string `json:"base_url,omitempty"`

	// APIKeyEnv names the environment variable holding the key. The value is
	// read at build time and never stored in the Spec.
	APIKeyEnv string `json:"api_key_env,omitempty"`

	// WorkDir is the directory a CLI-backed provider runs in.
	WorkDir string `json:"work_dir,omitempty"`

	// Timeout bounds a single request.
	Timeout Duration `json:"timeout,omitempty"`

	// Options carries factory-specific settings that do not deserve a field on
	// every provider. Registered factories document their own keys.
	Options map[string]string `json:"options,omitempty"`
}

// StagesSpec is the set of optimizations a document can enable. A nil section
// means the stage is off; an empty object ({}) means on with defaults.
type StagesSpec struct {
	Preprocess     *PreprocessSpec  `json:"preprocess,omitempty"`
	ToolCache      *ToolCacheSpec   `json:"tool_cache,omitempty"`
	RAG            *RAGSpec         `json:"rag,omitempty"`
	Dedupe         *DedupeSpec      `json:"dedupe,omitempty"`
	Compression    *CompressionSpec `json:"compression,omitempty"`
	HistoryBudget  *HistorySpec     `json:"history_budget,omitempty"`
	CacheAlignment *AlignSpec       `json:"cache_alignment,omitempty"`

	// Custom names stages the program registered with Registry.RegisterStage,
	// enabled in the order listed. They run after every built-in stage and
	// before cache alignment, exactly as config.WithStage places them.
	Custom []string `json:"custom,omitempty"`
}

// PreprocessSpec declares exact-match rules: the subset of preprocessing that
// survives being written down.
//
// A rule here answers a request whose Query equals Match, with Respond, and
// never calls the model. That covers the cases a document can express honestly —
// slash commands, canned help, fixed greetings. Anything needing a predicate
// belongs in Go, through Registry.Rules.
type PreprocessSpec struct {
	Rules []ExactRuleSpec `json:"rules,omitempty"`

	// Use names rules the program registered with Registry.RegisterRule. They
	// are tried before the declared Rules, since a Go rule exists precisely
	// because its condition is more specific than exact equality.
	Use []string `json:"use,omitempty"`
}

// ExactRuleSpec is one declarative rule.
type ExactRuleSpec struct {
	// Name identifies the rule in metrics. Defaults to "exact:"+Match.
	Name string `json:"name,omitempty"`

	// Match is compared against Request.Query for exact equality.
	Match string `json:"match"`

	// CaseInsensitive compares Match and Query without regard to case.
	CaseInsensitive bool `json:"case_insensitive,omitempty"`

	// Respond is the answer returned when Match hits.
	Respond string `json:"respond"`
}

// ToolCacheSpec configures tool-result reuse.
type ToolCacheSpec struct {
	// Backend selects a registered cache factory. "memory" out of the box;
	// "redis" once the nested module registers itself.
	Backend string `json:"backend,omitempty"`

	// TTL bounds how long a cached result stays valid. Zero means no expiry.
	TTL Duration `json:"ttl,omitempty"`

	// Options carries backend-specific settings, e.g. a Redis address.
	Options map[string]string `json:"options,omitempty"`
}

// RAGSpec configures retrieval. Both an embedder and a store must resolve, so
// a document alone cannot enable RAG — the program must have registered them.
type RAGSpec struct {
	Embedder string `json:"embedder"`
	Store    string `json:"store"`
	TopK     int    `json:"top_k,omitempty"`

	Options map[string]string `json:"options,omitempty"`
}

// DedupeSpec configures duplicate-chunk removal.
type DedupeSpec struct {
	// Threshold below 1.0 opts into lossy near-duplicate matching. It is
	// spelled out here rather than defaulted low because discarding evidence
	// that merely resembles other evidence should be a decision someone made
	// on purpose. Zero means the safe default (exact matches only).
	Threshold float64 `json:"threshold,omitempty"`
}

// CompressionSpec selects compressors by name, in the order given.
type CompressionSpec struct {
	// Preset "default" is JSON + prose, matching config.WithDefaultCompression.
	// Leave empty and use Compressors for an explicit chain.
	Preset string `json:"preset,omitempty"`

	// Compressors names the chain explicitly: "json", "code", "text". A
	// catch-all ("text") must come last or Build rejects the document, because
	// a catch-all in front of anything else silently disables it.
	Compressors []string `json:"compressors,omitempty"`

	// ElideBodies makes the code compressor drop function bodies. Lossy, and
	// off unless asked for.
	ElideBodies bool `json:"elide_bodies,omitempty"`
}

// HistorySpec sets the per-turn-type token budgets.
type HistorySpec struct {
	// Preset "default" uses budget.DefaultPolicy(). Any explicit budget below
	// overrides the corresponding preset value.
	Preset string `json:"preset,omitempty"`

	RoutineStep   int `json:"routine_step,omitempty"`
	NewQuestion   int `json:"new_question,omitempty"`
	ErrorRecovery int `json:"error_recovery,omitempty"`

	// Counter names a registered token counter. Empty uses the free character
	// estimator, which is approximate and systematically wrong on code and CJK
	// — fine for trimming to save cost, not for trimming against a hard limit.
	Counter string `json:"counter,omitempty"`
}

// AlignSpec configures prompt-cache breakpoints. It has no fields today: v1
// recognises exactly one anchor, the end of the static prefix, and inventing
// others would place breakpoints tokipe cannot prove are cache-safe. The type
// exists so that `"cache_alignment": {}` reads as deliberate and so options can
// arrive later without changing the document's shape.
type AlignSpec struct{}

// RouterSpec declares tiered model selection.
type RouterSpec struct {
	// Tiers are matched in order; a request routes to the first tier whose
	// MaxComplexity it does not exceed.
	Tiers []TierSpec `json:"tiers"`
}

// TierSpec binds a provider to the complexity ceiling it serves.
type TierSpec struct {
	Provider ProviderSpec `json:"provider"`

	// MaxComplexity is the inclusive upper bound this tier accepts, 0..1.
	//
	// Worth knowing before tuning: the heuristic weights length at 0.5, code at
	// 0.3 and chunk count at 0.2, so a short code-heavy prompt lands near 0.33.
	// A boundary at 0.35 sends it to the cheap tier.
	MaxComplexity float64 `json:"max_complexity"`
}

// Duration is a time.Duration that reads as "30m" in JSON rather than as
// 1800000000000. A config file written in nanoseconds is a config file nobody
// can review.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("runtime: %q is not a duration (want e.g. \"30m\", \"1h30m\"): %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}

	// A bare number is almost always someone meaning seconds and getting
	// nanoseconds. Refuse it rather than silently configuring a 30-nanosecond
	// TTL that looks like it works and caches nothing.
	var n float64
	if err := json.Unmarshal(b, &n); err == nil {
		return fmt.Errorf("runtime: duration must be a string like \"30s\", not the number %v", n)
	}
	return fmt.Errorf("runtime: cannot read %s as a duration", b)
}
