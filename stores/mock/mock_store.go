// Package mock provides in-memory, network-free test doubles for the
// stores.Embedder and stores.VectorStore interfaces.
//
// Both doubles record call counts so tests can assert negative facts such as
// "the RAG stage never ran retrieval when NeedsRetrieval was false".
// Both are safe for concurrent use.
package mock

import (
	"context"
	"hash/fnv"
	"math"
	"sort"
	"sync"

	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/stores"
)

// DefaultDim is the vector dimensionality used by Embedder when none is given.
const DefaultDim = 64

// Embedder is a deterministic, offline stores.Embedder. It hashes the input
// text into a fixed-dimension unit vector: the same text always yields the same
// vector, and similar texts (sharing words) yield vectors with higher cosine
// similarity. It performs no I/O.
type Embedder struct {
	// Dim is the vector dimensionality. Zero means DefaultDim.
	Dim int

	// Err, if set, is returned from every Embed call (fail-open testing).
	Err error

	// EmbedFunc, if set, overrides the default hashing behaviour entirely.
	EmbedFunc func(ctx context.Context, text string) ([]float32, error)

	mu     sync.Mutex
	calls  int
	inputs []string
}

var _ stores.Embedder = (*Embedder)(nil)

// NewEmbedder returns an Embedder with the given dimensionality. dim <= 0 uses
// DefaultDim.
func NewEmbedder(dim int) *Embedder {
	if dim <= 0 {
		dim = DefaultDim
	}
	return &Embedder{Dim: dim}
}

// NewFailingEmbedder returns an Embedder whose Embed always fails with err.
func NewFailingEmbedder(err error) *Embedder {
	e := NewEmbedder(0)
	e.Err = err
	return e
}

// Embed implements stores.Embedder.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	e.calls++
	e.inputs = append(e.inputs, text)
	dim := e.Dim
	e.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.EmbedFunc != nil {
		return e.EmbedFunc(ctx, text)
	}
	if e.Err != nil {
		return nil, e.Err
	}
	if dim <= 0 {
		dim = DefaultDim
	}
	return Vector(text, dim), nil
}

// Calls reports how many times Embed was invoked.
func (e *Embedder) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// Inputs returns a copy of every text passed to Embed, in call order.
func (e *Embedder) Inputs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.inputs...)
}

// Reset zeroes the recorded call counts and inputs.
func (e *Embedder) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = 0
	e.inputs = nil
}

// Vector deterministically hashes text into a unit vector of length dim.
// Exported so tests can build the exact vector a given text will embed to.
func Vector(text string, dim int) []float32 {
	if dim <= 0 {
		dim = DefaultDim
	}
	vec := make([]float32, dim)
	for _, tok := range tokenize(text) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(tok))
		sum := h.Sum64()
		idx := int(sum % uint64(dim))
		// Sign bit spreads tokens over both halves of each axis.
		if sum&(1<<63) != 0 {
			vec[idx] -= 1
		} else {
			vec[idx] += 1
		}
	}
	normalize(vec)
	return vec
}

// tokenize splits on any non-alphanumeric rune and lowercases ASCII letters.
func tokenize(text string) []string {
	var (
		out []string
		cur []rune
	)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range text {
		switch {
		case r >= 'A' && r <= 'Z':
			cur = append(cur, r+('a'-'A'))
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	if len(out) == 0 {
		// Guarantee a non-zero vector even for empty/punctuation-only input.
		out = append(out, "\x00empty")
	}
	return out
}

func normalize(vec []float32) {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return
	}
	norm := math.Sqrt(sum)
	for i, v := range vec {
		vec[i] = float32(float64(v) / norm)
	}
}

// Document is a chunk plus its vector, as held by Store.
type Document struct {
	Chunk  pipeline.Chunk
	Vector []float32
}

// Store is an in-memory stores.VectorStore doing a linear-scan cosine
// similarity search. It is safe for concurrent use.
type Store struct {
	// Err, if set, is returned from every Search call (fail-open testing).
	Err error

	// SearchFunc, if set, overrides the default scan entirely.
	SearchFunc func(ctx context.Context, vec []float32, topK int) ([]pipeline.Chunk, error)

	mu    sync.RWMutex
	docs  []Document
	calls int
}

var _ stores.VectorStore = (*Store)(nil)

// NewStore returns an empty Store.
func NewStore() *Store { return &Store{} }

// NewFailingStore returns a Store whose Search always fails with err.
func NewFailingStore(err error) *Store {
	s := NewStore()
	s.Err = err
	return s
}

// Add indexes a chunk under the vector produced by embedding its content with
// Vector at dimensionality dim. It is the convenient way to seed a Store that
// will be queried through this package's Embedder.
func (s *Store) Add(dim int, chunks ...pipeline.Chunk) {
	docs := make([]Document, 0, len(chunks))
	for _, c := range chunks {
		docs = append(docs, Document{Chunk: c, Vector: Vector(c.Content, dim)})
	}
	s.AddDocuments(docs...)
}

// AddDocuments indexes pre-vectorized documents.
func (s *Store) AddDocuments(docs ...Document) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range docs {
		d.Vector = append([]float32(nil), d.Vector...)
		s.docs = append(s.docs, d)
	}
}

// Len reports how many documents are indexed.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.docs)
}

// Search implements stores.VectorStore. It returns the topK most cosine-similar
// chunks in descending similarity order, with Chunk.Similarity populated.
func (s *Store) Search(ctx context.Context, vec []float32, topK int) ([]pipeline.Chunk, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.SearchFunc != nil {
		return s.SearchFunc(ctx, vec, topK)
	}
	if s.Err != nil {
		return nil, s.Err
	}
	if topK <= 0 {
		return nil, nil
	}

	s.mu.RLock()
	scored := make([]pipeline.Chunk, 0, len(s.docs))
	for _, d := range s.docs {
		c := d.Chunk
		c.Similarity = Cosine(vec, d.Vector)
		scored = append(scored, c)
	}
	s.mu.RUnlock()

	// Stable order: similarity desc, then content asc for determinism.
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Similarity != scored[j].Similarity {
			return scored[i].Similarity > scored[j].Similarity
		}
		return scored[i].Content < scored[j].Content
	})
	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored, nil
}

// Calls reports how many times Search was invoked.
func (s *Store) Calls() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.calls
}

// Reset zeroes the recorded call count (documents are kept).
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = 0
}

// Cosine returns the cosine similarity of a and b. Mismatched lengths are
// compared over the shorter prefix; a zero-magnitude vector yields 0.
func Cosine(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
