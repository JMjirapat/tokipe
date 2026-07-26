// Package providers holds LLM provider adapters. The ModelClient contract
// itself lives in package pipeline (to avoid an import cycle) and is aliased
// here so `providers.ModelClient` is the name callers use.
package providers

import "github.com/JMjirapat/tokipe/pipeline"

// ModelClient abstracts a single LLM provider/model endpoint. Implementations
// must NOT be assumed to be Anthropic-specific anywhere outside this package
// and its subdirectories.
type ModelClient = pipeline.ModelClient
