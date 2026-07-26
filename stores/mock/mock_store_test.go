package mock

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/JMjirapat/tokipe/pipeline"
)

func TestEmbedderDeterministicAndUnitLength(t *testing.T) {
	e := NewEmbedder(32)
	ctx := context.Background()

	a, err := e.Embed(ctx, "the quick brown fox")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	b, err := e.Embed(ctx, "the quick brown fox")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(a) != 32 {
		t.Fatalf("dim = %d, want 32", len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("embedding not deterministic at %d: %v vs %v", i, a[i], b[i])
		}
	}
	if got := Cosine(a, a); math.Abs(got-1) > 1e-6 {
		t.Fatalf("self-similarity = %v, want 1", got)
	}
	if e.Calls() != 2 {
		t.Fatalf("Calls = %d, want 2", e.Calls())
	}
	if inputs := e.Inputs(); len(inputs) != 2 || inputs[0] != "the quick brown fox" {
		t.Fatalf("Inputs = %v", inputs)
	}
	e.Reset()
	if e.Calls() != 0 || len(e.Inputs()) != 0 {
		t.Fatal("Reset did not clear recorded calls")
	}
}

func TestEmbedderEmptyTextIsNonZero(t *testing.T) {
	e := NewEmbedder(0)
	v, err := e.Embed(context.Background(), "   !!!  ")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != DefaultDim {
		t.Fatalf("dim = %d, want %d", len(v), DefaultDim)
	}
	if Cosine(v, v) == 0 {
		t.Fatal("empty-ish text produced a zero vector")
	}
}

func TestEmbedderErrAndOverride(t *testing.T) {
	wantErr := errors.New("boom")
	e := NewFailingEmbedder(wantErr)
	if _, err := e.Embed(context.Background(), "x"); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}

	e2 := NewEmbedder(4)
	e2.EmbedFunc = func(context.Context, string) ([]float32, error) {
		return []float32{1, 0, 0, 0}, nil
	}
	v, err := e2.Embed(context.Background(), "anything")
	if err != nil || len(v) != 4 || v[0] != 1 {
		t.Fatalf("EmbedFunc override not honoured: %v %v", v, err)
	}
}

func TestEmbedderRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := NewEmbedder(8)
	if _, err := e.Embed(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestStoreSearchRanksAndLimits(t *testing.T) {
	const dim = 64
	s := NewStore()
	s.Add(dim,
		pipeline.Chunk{Content: "postgres vector similarity search", SourceURL: "a"},
		pipeline.Chunk{Content: "banana bread recipe", SourceURL: "b"},
		pipeline.Chunk{Content: "vector similarity search tuning", SourceURL: "c"},
	)
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}

	q := Vector("vector similarity search", dim)
	got, err := s.Search(context.Background(), q, 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (topK)", len(got))
	}
	if got[0].Similarity < got[1].Similarity {
		t.Fatalf("results not sorted desc: %v", got)
	}
	if got[0].Similarity <= 0 {
		t.Fatalf("top similarity = %v, want > 0", got[0].Similarity)
	}
	for _, c := range got {
		if c.SourceURL == "b" {
			t.Fatalf("unrelated chunk ranked in top 2: %v", got)
		}
	}
	if s.Calls() != 1 {
		t.Fatalf("Calls = %d, want 1", s.Calls())
	}
	s.Reset()
	if s.Calls() != 0 {
		t.Fatal("Reset did not clear call count")
	}
}

func TestStoreSearchEdgeCases(t *testing.T) {
	s := NewStore()
	s.Add(8, pipeline.Chunk{Content: "one"})

	if got, err := s.Search(context.Background(), Vector("one", 8), 0); err != nil || got != nil {
		t.Fatalf("topK=0 => %v, %v; want nil, nil", got, err)
	}

	wantErr := errors.New("db down")
	fs := NewFailingStore(wantErr)
	if _, err := fs.Search(context.Background(), nil, 3); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Search(ctx, nil, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	os := NewStore()
	os.SearchFunc = func(context.Context, []float32, int) ([]pipeline.Chunk, error) {
		return []pipeline.Chunk{{Content: "override"}}, nil
	}
	got, err := os.Search(context.Background(), nil, 1)
	if err != nil || len(got) != 1 || got[0].Content != "override" {
		t.Fatalf("SearchFunc override not honoured: %v %v", got, err)
	}
}

func TestCosineMismatchedAndZero(t *testing.T) {
	if got := Cosine([]float32{1, 0, 0}, []float32{1, 0}); math.Abs(got-1) > 1e-6 {
		t.Fatalf("prefix cosine = %v, want 1", got)
	}
	if got := Cosine([]float32{0, 0}, []float32{1, 1}); got != 0 {
		t.Fatalf("zero-magnitude cosine = %v, want 0", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	const dim = 16
	e := NewEmbedder(dim)
	s := NewStore()
	s.Add(dim, pipeline.Chunk{Content: "alpha"}, pipeline.Chunk{Content: "beta"})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := e.Embed(context.Background(), "alpha")
			if err != nil {
				t.Errorf("Embed: %v", err)
				return
			}
			if _, err := s.Search(context.Background(), v, 1); err != nil {
				t.Errorf("Search: %v", err)
			}
			if i%5 == 0 {
				s.Add(dim, pipeline.Chunk{Content: "extra"})
			}
		}(i)
	}
	wg.Wait()

	if e.Calls() != 50 {
		t.Fatalf("embedder Calls = %d, want 50", e.Calls())
	}
	if s.Calls() != 50 {
		t.Fatalf("store Calls = %d, want 50", s.Calls())
	}
}
