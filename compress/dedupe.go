package compress

import (
	"context"
	"strings"
	"unicode"

	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
)

// DedupeStage removes retrieved chunks that duplicate one already kept.
//
// Retrieval routinely returns the same passage several times: a README quoted
// in a wiki page, a doc comment that also appears in generated API docs, the
// same paragraph indexed under two URLs. Every copy costs full price, and the
// second copy tells the model nothing the first did not.
//
// This is a Stage rather than a Compressor because duplication is a property of
// the *set* of chunks. A Compressor sees one chunk's content at a time and
// cannot know another chunk says the same thing.
//
// # How similarity is measured
//
// Jaccard similarity over word shingles: each chunk becomes a set of
// overlapping n-word sequences, and two chunks are duplicates when the sets
// overlap beyond a threshold. Shingles rather than bare words because word sets
// ignore order, and "cache the prefix, not the suffix" would match "cache the
// suffix, not the prefix" perfectly.
//
// This is lexical, not semantic. It catches copies and near-copies — the common
// case — and will not catch two passages that say the same thing in different
// words. Doing that needs embeddings, which would mean an Embedder dependency
// and a vector comparison per pair; that is a real option for a later phase,
// not a silent upgrade to this one.
type DedupeStage struct {
	threshold float64
	shingle   int
	minWords  int
	rec       metrics.Recorder
}

// Metric and metadata names emitted by the stage.
const (
	// MetricChunksDeduped counts chunks removed as duplicates.
	MetricChunksDeduped = "compress.chunks_deduped"

	// MetaDeduped is the Request.Metadata key holding how many chunks were
	// dropped this turn.
	MetaDeduped = "compress.deduped"
)

// Defaults chosen to be conservative: dropping a chunk the model needed is a
// worse failure than sending one it did not.
const (
	// DefaultDedupeThreshold is the Jaccard similarity above which two chunks
	// are treated as the same. 0.8 catches reformatted and lightly-edited
	// copies while leaving genuinely different passages alone.
	DefaultDedupeThreshold = 0.8

	// DefaultShingleSize is the number of words per shingle.
	DefaultShingleSize = 4

	// DefaultDedupeMinWords is the shortest chunk worth comparing. Short
	// chunks produce few shingles, and few shingles make Jaccard jumpy — two
	// unrelated one-liners can easily score 1.0.
	DefaultDedupeMinWords = 12
)

// DedupeOption configures a DedupeStage.
type DedupeOption func(*DedupeStage)

// WithDedupeThreshold sets the Jaccard similarity above which chunks are
// considered duplicates. Values outside (0,1] fall back to the default.
func WithDedupeThreshold(t float64) DedupeOption {
	return func(d *DedupeStage) {
		if t > 0 && t <= 1 {
			d.threshold = t
		}
	}
}

// WithShingleSize sets the words per shingle. Below 1 falls back to the default.
func WithShingleSize(n int) DedupeOption {
	return func(d *DedupeStage) {
		if n >= 1 {
			d.shingle = n
		}
	}
}

// WithDedupeMetrics attaches a metrics recorder.
func WithDedupeMetrics(r metrics.Recorder) DedupeOption {
	return func(d *DedupeStage) { d.rec = metrics.Or(r) }
}

// NewDedupeStage returns a stage that drops near-duplicate retrieved chunks.
func NewDedupeStage(opts ...DedupeOption) *DedupeStage {
	d := &DedupeStage{
		threshold: DefaultDedupeThreshold,
		shingle:   DefaultShingleSize,
		minWords:  DefaultDedupeMinWords,
		rec:       metrics.Nop{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

func (d *DedupeStage) Name() string { return "dedupe" }

// Process removes duplicate chunks, keeping the first occurrence.
//
// First rather than "best" on purpose: RAGStage returns chunks ordered by
// similarity to the query, so the first copy is already the most relevant one.
// Re-ranking here would silently override retrieval's own ordering.
//
// Fail-open: any doubt leaves the chunk in place. The stage never errors.
func (d *DedupeStage) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	if req == nil || len(req.RetrievedChunks) < 2 {
		return req, nil
	}
	if err := ctx.Err(); err != nil {
		return req, nil
	}

	kept := make([]pipeline.Chunk, 0, len(req.RetrievedChunks))
	keptSets := make([]map[string]struct{}, 0, len(req.RetrievedChunks))
	dropped := 0

	for _, chunk := range req.RetrievedChunks {
		set := shingles(chunk.Content, d.shingle, d.minWords)
		if set == nil {
			// Too short to compare safely — keep it. A chunk that cannot be
			// judged is not a chunk that can be discarded.
			kept = append(kept, chunk)
			keptSets = append(keptSets, nil)
			continue
		}

		duplicate := false
		for _, existing := range keptSets {
			if existing == nil {
				continue
			}
			if jaccard(set, existing) >= d.threshold {
				duplicate = true
				break
			}
		}
		if duplicate {
			dropped++
			metrics.Inc(d.rec, MetricChunksDeduped, map[string]string{"source": chunk.SourceURL})
			continue
		}
		kept = append(kept, chunk)
		keptSets = append(keptSets, set)
	}

	if dropped == 0 {
		return req, nil
	}
	req.RetrievedChunks = kept
	req.SetMeta(MetaDeduped, dropped)
	return req, nil
}

// shingles builds the set of overlapping n-word sequences in content, or nil
// when the content is too short for the comparison to mean anything.
//
// Words are lowercased and stripped of surrounding punctuation so that
// "cache," and "Cache" match — the difference between a quoted copy and the
// original is usually exactly that kind of noise.
func shingles(content string, n, minWords int) map[string]struct{} {
	words := normalizeWords(content)
	if len(words) < minWords || len(words) < n {
		return nil
	}

	set := make(map[string]struct{}, len(words))
	for i := 0; i+n <= len(words); i++ {
		set[strings.Join(words[i:i+n], " ")] = struct{}{}
	}
	return set
}

func normalizeWords(content string) []string {
	fields := strings.FieldsFunc(content, unicode.IsSpace)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimFunc(f, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if f != "" {
			out = append(out, strings.ToLower(f))
		}
	}
	return out
}

// jaccard is |A∩B| / |A∪B|.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// Iterate the smaller set: the intersection cannot be larger than it.
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}

	intersection := 0
	for k := range small {
		if _, ok := large[k]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

var _ pipeline.Stage = (*DedupeStage)(nil)
