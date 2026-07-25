// Package config is the functional-options surface for assembling a pipeline.
//
// Its real job is to make the load-bearing stage ordering from the spec
// impossible to get wrong. Callers declare *what* they want enabled; this
// package decides the order:
//
//	preprocess → toolcache → rag → compress → lazyload → cache-align
//
// RAG must precede compress, and both must precede cache alignment, because a
// cache breakpoint placed after per-turn content poisons the provider-side
// cache (spec §2.4.3, §2.4.6). Routing is not a stage at all: it runs last,
// after the prompt has its final shape, so it can see the real size.
package config

import (
	"time"

	"agentkit/cache"
	"agentkit/compress"
	"agentkit/metrics"
	"agentkit/pipeline"
	"agentkit/preprocess"
	"agentkit/rag"
	"agentkit/stores"
	"agentkit/toolcache"
)

// Config is the resolved set of enabled optimizations. Build it with Options
// rather than by hand; the zero value is a valid pass-through pipeline.
type Config struct {
	Metrics metrics.Recorder

	PreprocessRules []preprocess.Rule

	ToolCache    toolcache.Cache
	ToolExecutor toolcache.Executor
	ToolTTL      time.Duration

	Embedder stores.Embedder
	Store    stores.VectorStore
	TopK     int

	Compressors []compress.Compressor

	AlignerSpecs []cache.BreakpointSpec
	AlignEnabled bool

	ExtraStages []pipeline.Stage

	Router pipeline.Router
}

// Option configures a Config.
type Option func(*Config)

// New resolves opts into a Config.
func New(opts ...Option) Config {
	c := Config{Metrics: metrics.Nop{}}
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	c.Metrics = metrics.Or(c.Metrics)
	return c
}

// WithMetrics attaches one recorder to every stage that reports counters.
func WithMetrics(r metrics.Recorder) Option {
	return func(c *Config) { c.Metrics = metrics.Or(r) }
}

// WithPreprocess enables deterministic short-circuiting. Rules are tried in
// the order given; the first that can handle the request wins.
func WithPreprocess(rules ...preprocess.Rule) Option {
	return func(c *Config) { c.PreprocessRules = append(c.PreprocessRules, rules...) }
}

// WithToolCache resolves Request.ToolCalls through cache, executing only the
// misses via exec. A zero ttl caches without expiry.
func WithToolCache(store toolcache.Cache, exec toolcache.Executor, ttl time.Duration) Option {
	return func(c *Config) {
		c.ToolCache, c.ToolExecutor, c.ToolTTL = store, exec, ttl
	}
}

// WithRAG enables retrieval for requests that set NeedsRetrieval.
func WithRAG(embedder stores.Embedder, store stores.VectorStore, topK int) Option {
	return func(c *Config) {
		c.Embedder, c.Store, c.TopK = embedder, store, topK
	}
}

// WithCompression compresses retrieved chunks. Compressors are tried in order;
// the first whose CanHandle returns true wins.
func WithCompression(compressors ...compress.Compressor) Option {
	return func(c *Config) { c.Compressors = append(c.Compressors, compressors...) }
}

// WithDefaultCompression enables the v1 compressors: JSON first (it is the
// stricter detector), then conservative prose whitespace collapsing.
func WithDefaultCompression() Option {
	return WithCompression(compress.NewJSONCompressor(), compress.NewTextCompressor())
}

// WithCacheAlignment enables prompt-cache breakpoint placement. Passing no
// specs uses cache.DefaultSpecs.
func WithCacheAlignment(specs ...cache.BreakpointSpec) Option {
	return func(c *Config) {
		c.AlignEnabled = true
		if len(specs) > 0 {
			c.AlignerSpecs = specs
		}
	}
}

// WithRouter selects the model client per request, after every stage has run.
func WithRouter(r pipeline.Router) Option {
	return func(c *Config) { c.Router = r }
}

// WithStage appends a caller-supplied stage. It runs after every built-in
// stage, so it can never displace the ordering the built-ins depend on — in
// particular it cannot land between RAG and cache alignment.
//
// A stage that must run earlier should be composed by hand with
// pipeline.New instead of through this package.
func WithStage(stages ...pipeline.Stage) Option {
	return func(c *Config) { c.ExtraStages = append(c.ExtraStages, stages...) }
}

// Stages materializes the enabled optimizations in their required order.
//
// Do not reorder this function's body without re-reading spec §2.4.3 and
// §2.4.6. The sequence is a correctness constraint, not a preference.
func (c Config) Stages() []pipeline.Stage {
	rec := metrics.Or(c.Metrics)
	var stages []pipeline.Stage

	// 1. Answer without an LLM if a rule can.
	if len(c.PreprocessRules) > 0 {
		stages = append(stages, preprocess.NewStageWith(c.PreprocessRules, preprocess.WithMetrics(rec)))
	}

	// 2. Reuse tool results instead of re-executing and re-sending them.
	if c.ToolCache != nil && c.ToolExecutor != nil {
		stages = append(stages, toolcache.NewStage(c.ToolCache, c.ToolExecutor,
			toolcache.WithTTL(c.ToolTTL), toolcache.WithStageMetrics(rec)))
	}

	// 3. Retrieval — MUST precede compression and alignment.
	if c.Embedder != nil && c.Store != nil {
		stages = append(stages, rag.NewStageWith(c.Embedder, c.Store, c.TopK, rag.WithMetrics(rec)))
	}

	// 4. Shrink what retrieval produced — MUST precede alignment.
	if len(c.Compressors) > 0 {
		stages = append(stages, compress.NewStageWithMetrics(rec, c.Compressors...))
	}

	// 5. Caller extensions, still ahead of alignment so they cannot invalidate
	//    breakpoints computed over content they went on to change.
	stages = append(stages, c.ExtraStages...)

	// 6. Alignment LAST: it needs the final content layout.
	if c.AlignEnabled {
		specs := c.AlignerSpecs
		if len(specs) == 0 {
			specs = cache.DefaultSpecs()
		}
		stages = append(stages, cache.NewAlignerWith(specs, cache.WithMetrics(rec)))
	}

	return stages
}
