// Package rag implements the retrieval-augmented-generation stage: it embeds
// the current query, searches a vector store, and attaches the results to
// Request.RetrievedChunks. See docs/spec.md §2.4.3.
package rag

import (
	"context"

	"github.com/JMjirapat/tokipe/internal/safe"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/stores"
)

// Metric names emitted by this stage.
const (
	MetricRetrievals = "rag_retrievals"
	MetricFailOpen   = "rag_fail_open"
	MetricSkipped    = "rag_skipped"
)

// Option configures a Stage.
type Option func(*Stage)

// WithMetrics attaches a metrics.Recorder. A nil recorder is safe.
func WithMetrics(r metrics.Recorder) Option {
	return func(s *Stage) { s.metrics = metrics.Or(r) }
}

// Stage retrieves context chunks for a Request and stores them in
// Request.RetrievedChunks.
//
// ####################################################################
// # PLACEMENT REQUIREMENT — LOAD-BEARING, DO NOT REORDER             #
// #                                                                  #
// # This stage MUST run BEFORE compress.Stage and BEFORE             #
// # cache.Aligner in any pipeline that uses it (docs/spec.md         #
// # §2.4.3, §2.4.6).                                                 #
// #                                                                  #
// # Why: RetrievedChunks are *dynamic* — they change on every turn.  #
// # compress.Stage must see them in order to shrink them, and        #
// # cache.Aligner must see them in order to keep every provider-side #
// # cache breakpoint OUT of that dynamic segment. Running rag after  #
// # the aligner appends fresh, unseen content behind an already      #
// # placed breakpoint, which poisons the prompt cache on every       #
// # single turn and silently destroys the cache-hit rate this whole  #
// # library exists to deliver.                                       #
// #                                                                  #
// # Correct order: ... -> rag -> compress -> lazyload -> cache_align #
// ####################################################################
type Stage struct {
	embedder stores.Embedder
	store    stores.VectorStore
	topK     int
	metrics  metrics.Recorder
}

var _ pipeline.Stage = (*Stage)(nil)

// NewStage builds a Stage. Signature is exactly as docs/spec.md §2.4.3.
func NewStage(embedder stores.Embedder, store stores.VectorStore, topK int) *Stage {
	return NewStageWith(embedder, store, topK)
}

// NewStageWith is NewStage plus functional options (metrics).
func NewStageWith(embedder stores.Embedder, store stores.VectorStore, topK int, opts ...Option) *Stage {
	s := &Stage{
		embedder: embedder,
		store:    store,
		topK:     topK,
		metrics:  metrics.Nop{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return "rag" }

// TopK reports the configured retrieval depth.
func (s *Stage) TopK() int { return s.topK }

// Process embeds req.Query and populates req.RetrievedChunks.
//
// FAIL-OPEN CONTRACT (docs/spec.md §2.5): Process never returns a non-nil
// error for a retrieval failure. If the embedder or the store fails — or the
// dependencies are simply not configured — RetrievedChunks is left empty and
// the Request continues down the pipeline unchanged. A degraded answer beats a
// failed request. The one exception is context cancellation, which is the
// caller's own signal to stop and is propagated verbatim.
func (s *Stage) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	if req == nil {
		return req, nil
	}
	if !req.NeedsRetrieval {
		// Zero embedder/store calls on this path — asserted by the tests.
		metrics.Inc(s.metrics, MetricSkipped, map[string]string{"reason": "not_needed"})
		return req, nil
	}
	if err := ctx.Err(); err != nil {
		return req, err
	}
	if s.embedder == nil || s.store == nil || s.topK <= 0 {
		s.failOpen(req, "not_configured")
		s.degrade("not_configured", nil)
		return req, nil
	}

	// Embedder and VectorStore are caller-supplied; a panic in either must
	// degrade to "no retrieval" like any other failure (spec §2.5.1).
	vec, err := safe.Value(func() ([]float32, error) { return s.embedder.Embed(ctx, req.Query) })
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return req, ctxErr
		}
		s.failOpen(req, "embed_error")
		s.degrade("embed_failed", err)
		return req, nil
	}

	chunks, err := safe.Value(func() ([]pipeline.Chunk, error) { return s.store.Search(ctx, vec, s.topK) })
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return req, ctxErr
		}
		s.failOpen(req, "search_error")
		s.degrade("search_failed", err)
		return req, nil
	}

	if len(chunks) > s.topK {
		chunks = chunks[:s.topK]
	}
	req.RetrievedChunks = chunks
	req.SetMeta("rag.chunks", len(chunks))
	metrics.Inc(s.metrics, MetricRetrievals, map[string]string{"result": "ok"})
	return req, nil
}

// degrade reports why retrieval was skipped. failOpen already counted it; this
// adds the reason and the error, so an operator can tell a misconfiguration
// from an outage.
func (s *Stage) degrade(reason string, err error) {
	metrics.Degrade(s.metrics, metrics.Degradation{Stage: s.Name(), Reason: reason, Err: err})
}

func (s *Stage) failOpen(req *pipeline.Request, reason string) {
	req.SetMeta("rag.fail_open", reason)
	metrics.Inc(s.metrics, MetricFailOpen, map[string]string{"reason": reason})
	metrics.Inc(s.metrics, MetricRetrievals, map[string]string{"result": "fail_open"})
}
