// Package stores defines the retrieval interfaces agentkit depends on.
// Concrete adapters (pgvector, mock) live in subdirectories; this file has
// zero third-party dependencies.
package stores

import (
	"context"

	"github.com/JMjirapat/tokipe/pipeline"
)

type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

type VectorStore interface {
	Search(ctx context.Context, vec []float32, topK int) ([]pipeline.Chunk, error)
}
