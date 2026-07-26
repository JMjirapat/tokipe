// Package agentkit assembles LLM token-optimization stages into a pipeline.
//
// Quickstart:
//
//	kit := agentkit.New(client,
//	    config.WithPreprocess(myRules...),
//	    config.WithRAG(embedder, store, 5),
//	    config.WithDefaultCompression(),
//	    config.WithCacheAlignment(),
//	    config.WithRouter(router.NewHeuristicRouter(cheap, strong)),
//	)
//	resp, err := kit.Run(ctx, &pipeline.Request{Query: "…"})
//
// Every optimization is opt-in and every one fails open: if compression,
// retrieval, a tool executor, or a cache backend breaks — including by
// panicking — the turn still reaches the model, and Run returns no error for
// it.
//
// Run returns an error in exactly three cases, none of them an optimization
// failure:
//
//   - the model call itself failed;
//   - ctx was cancelled or its deadline passed;
//   - a caller-supplied Stage added with config.WithStage returned an error,
//     or a stage wrote a malformed short-circuit value into Metadata. Both
//     surface as *pipeline.StageError naming the stage responsible.
//
// One case is neither returned nor contained: a Stage you supplied via
// config.WithStage that *panics* propagates the panic to your caller. Run
// does not recover it. Your stage is your code running in your pipeline;
// recovering it would hide your bug rather than tolerate a third party's. If
// you want that panic contained, recover inside your own Process method.
//
// Everything agentkit itself calls into — preprocess rules, tool executors,
// compressors, embedders, vector stores, routers, and every Name method — is
// wrapped, and a panic there is treated exactly like the equivalent error.
//
// New enforces the stage ordering the spec requires (retrieval before
// compression, both before cache alignment, routing last). Callers who need a
// different order must compose pipeline.New directly and own that decision.
package agentkit

import (
	"github.com/JMjirapat/tokipe/config"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/providers"
)

// Re-exported so a caller can build a request and read a response without
// importing the subpackages directly.
type (
	Request  = pipeline.Request
	Response = pipeline.Response
	Message  = pipeline.Message
	Chunk    = pipeline.Chunk
	ToolCall = pipeline.ToolCall
	Usage    = pipeline.Usage
	Stage    = pipeline.Stage
)

// New builds a Pipeline from client and the enabled options. client is the
// default model endpoint; a router configured via config.WithRouter may
// override it per request.
//
// Passing no options yields a pipeline that simply forwards to client, which
// is a valid — and useful — baseline to measure the optimizations against.
func New(client providers.ModelClient, opts ...config.Option) *pipeline.Pipeline {
	cfg := config.New(opts...)
	return pipeline.NewWithRouter(client, cfg.Router, cfg.Stages()...)
}

// NewFromConfig is New for callers that already hold a resolved Config.
func NewFromConfig(client providers.ModelClient, cfg config.Config) *pipeline.Pipeline {
	return pipeline.NewWithRouter(client, cfg.Router, cfg.Stages()...)
}
