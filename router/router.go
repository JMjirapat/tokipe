// Package router selects which ModelClient serves the final LLM call, based on
// a cheap, deterministic estimate of how hard the request is.
//
// The Router contract itself is declared in package pipeline (pipeline imports
// nothing, and Pipeline.Run needs the interface) and is aliased here so
// `router.Router` and `router.RouteDecision` are the names callers use, exactly
// as docs/spec.md §2.4.7 describes.
//
// A Router is deliberately NOT a pipeline.Stage: it runs after every
// context-shaping stage so it can see the final prompt size.
package router

import "github.com/JMjirapat/tokipe/pipeline"

// Router selects the ModelClient used for the final Send.
//
// Alias of pipeline.Router — see the package comment for why the interface is
// declared there.
type Router = pipeline.Router

// RouteDecision is the result of a Route call. A zero Client means "no opinion";
// Pipeline.Run falls back to its default client, which is the fail-open path.
//
// Alias of pipeline.RouteDecision.
type RouteDecision = pipeline.RouteDecision

// Tier binds a ModelClient to the upper bound of the complexity range it is
// willing to serve: this tier handles requests scoring <= MaxComplexity.
type Tier struct {
	Client        pipeline.ModelClient
	MaxComplexity float64
}

// UnnamedClient is reported when a ModelClient's Name method panics or
// returns "".
const UnnamedClient = "unnamed_client"
