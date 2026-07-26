// Package compress implements the CompressStage (docs/spec.md §2.4.4): it
// walks Request.RetrievedChunks and rewrites each chunk's content with the
// first registered Compressor that claims it.
//
// Every compressor in this package is *lossless with respect to meaning*: it
// may delete whitespace or caller-nominated low-value fields, but it never
// invents content and never rewrites meaning-bearing characters. Semantic
// summarisation is explicitly out of scope for v1.
//
// ORDERING: this stage must run BEFORE cache.Aligner. It mutates chunk bytes,
// and the aligner's breakpoints are only valid against the final bytes.
//
// PACKAGE-NAMING DEVIATION: the spec writes `json.Compressor` /
// `text.Compressor` / `code.Compressor`, which would imply three subpackages —
// and a subpackage literally named `json` would shadow encoding/json for every
// caller that dot-less imports both. They are therefore JSONCompressor,
// TextCompressor and CodeCompressor in this single package, one per file
// exactly as the §2.2 module layout specifies (compress/json.go, text.go,
// code.go).
package compress

import (
	"context"

	"github.com/JMjirapat/tokipe/internal/safe"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
)

// MetricCompressed is the counter name incremented once per chunk actually
// rewritten by a compressor.
const MetricCompressed = "compress_chunks"

// Compressor turns one piece of content into a smaller, equivalent piece.
type Compressor interface {
	// Compress returns the compressed form of content. Returning an error is
	// not fatal: the Stage leaves the original content untouched.
	Compress(content string) (string, error)

	// CanHandle reports whether this compressor understands content. It must
	// be cheap and must not panic on arbitrary bytes.
	CanHandle(content string) bool
}

// Stage is the content router: compressors are checked in registration order
// and the first whose CanHandle returns true wins.
type Stage struct {
	compressors []Compressor
	rec         metrics.Recorder
}

// NewStage builds a Stage from an ordered compressor list. Order matters:
// place specific compressors (JSON, code) before catch-alls (text).
func NewStage(compressors ...Compressor) *Stage {
	return NewStageWithMetrics(nil, compressors...)
}

// NewStageWithMetrics is NewStage plus an optional metrics.Recorder (nil is
// fine — it becomes a no-op).
func NewStageWithMetrics(rec metrics.Recorder, compressors ...Compressor) *Stage {
	return &Stage{
		compressors: append([]Compressor(nil), compressors...),
		rec:         metrics.Or(rec),
	}
}

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return "compress" }

// Process compresses every retrieved chunk it can.
//
// Fail-open (docs/spec.md §2.5 requirement 1): Process never returns a non-nil
// error. A compressor that errors, panics-free-but-fails, or returns content
// larger than the original leaves that chunk exactly as it was; other chunks
// are still processed.
func (s *Stage) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	if req == nil {
		return req, nil
	}
	for i := range req.RetrievedChunks {
		if ctx != nil && ctx.Err() != nil {
			// Fail-open: stop early, keep whatever was already compressed.
			return req, nil
		}
		original := req.RetrievedChunks[i].Content
		for _, c := range s.compressors {
			// Compressors are caller-supplied; a panic must leave the chunk
			// uncompressed rather than fail the turn (spec §2.5.1).
			if c == nil {
				continue
			}
			canHandle, cErr := safe.Value(func() (bool, error) { return c.CanHandle(original), nil })
			if cErr != nil {
				metrics.Degrade(s.rec, metrics.Degradation{
					Stage: s.Name(), Reason: "compressor_canhandle_panicked", Err: cErr,
				})
				continue
			}
			if !canHandle {
				continue
			}
			compressed, err := safe.Value(func() (string, error) { return c.Compress(original) })
			if err != nil {
				// Fail-open: leave the chunk uncompressed, and report it —
				// silently sending full-size chunks is the failure mode this
				// stage exists to prevent.
				metrics.Degrade(s.rec, metrics.Degradation{
					Stage:  s.Name(),
					Reason: "compress_failed",
					Err:    err,
				})
				break
			}
			if len(compressed) >= len(original) {
				// No win; never make a prompt bigger in the name of
				// compression.
				break
			}
			req.RetrievedChunks[i].Content = compressed
			s.rec.Counter(MetricCompressed).Inc(map[string]string{"stage": s.Name()})
			break
		}
	}
	return req, nil
}

var _ pipeline.Stage = (*Stage)(nil)
