package rag

import (
	"context"
	"errors"
	"testing"

	"agentkit/metrics"
	"agentkit/pipeline"
	storemock "agentkit/stores/mock"
)

const dim = 64

func seededStore() *storemock.Store {
	s := storemock.NewStore()
	s.Add(dim,
		pipeline.Chunk{Content: "vector similarity search in postgres", SourceURL: "a"},
		pipeline.Chunk{Content: "vector similarity tuning guide", SourceURL: "b"},
		pipeline.Chunk{Content: "banana bread recipe", SourceURL: "c"},
		pipeline.Chunk{Content: "how to fold a paper crane", SourceURL: "d"},
	)
	return s
}

func TestStageImplementsContract(t *testing.T) {
	s := NewStage(storemock.NewEmbedder(dim), storemock.NewStore(), 3)
	if s.Name() != "rag" {
		t.Fatalf("Name = %q, want %q", s.Name(), "rag")
	}
	if s.TopK() != 3 {
		t.Fatalf("TopK = %d, want 3", s.TopK())
	}
	var _ pipeline.Stage = s
}

func TestNeedsRetrievalFalseSkipsEntirely(t *testing.T) {
	emb := storemock.NewEmbedder(dim)
	st := seededStore()
	rec := metrics.NewInMemory()
	s := NewStageWith(emb, st, 3, WithMetrics(rec))

	req := &pipeline.Request{Query: "vector similarity search", NeedsRetrieval: false}
	got, err := s.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(got.RetrievedChunks) != 0 {
		t.Fatalf("RetrievedChunks = %v, want empty", got.RetrievedChunks)
	}
	if emb.Calls() != 0 {
		t.Fatalf("embedder called %d times, want 0", emb.Calls())
	}
	if st.Calls() != 0 {
		t.Fatalf("store called %d times, want 0", st.Calls())
	}
	if rec.Count(MetricRetrievals) != 0 {
		t.Fatalf("retrieval counter = %d, want 0", rec.Count(MetricRetrievals))
	}
	if rec.Count(MetricSkipped) != 1 {
		t.Fatalf("skip counter = %d, want 1", rec.Count(MetricSkipped))
	}
}

func TestHappyPathPopulatesChunksAndRespectsTopK(t *testing.T) {
	emb := storemock.NewEmbedder(dim)
	st := seededStore()
	rec := metrics.NewInMemory()
	s := NewStageWith(emb, st, 2, WithMetrics(rec))

	req := &pipeline.Request{Query: "vector similarity search", NeedsRetrieval: true}
	got, err := s.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(got.RetrievedChunks) != 2 {
		t.Fatalf("len(RetrievedChunks) = %d, want 2", len(got.RetrievedChunks))
	}
	if got.RetrievedChunks[0].Similarity < got.RetrievedChunks[1].Similarity {
		t.Fatalf("chunks not sorted by similarity: %v", got.RetrievedChunks)
	}
	for _, c := range got.RetrievedChunks {
		if c.SourceURL == "c" || c.SourceURL == "d" {
			t.Fatalf("irrelevant chunk retrieved: %+v", c)
		}
	}
	if emb.Calls() != 1 || st.Calls() != 1 {
		t.Fatalf("calls = embed %d / search %d, want 1 / 1", emb.Calls(), st.Calls())
	}
	if inputs := emb.Inputs(); len(inputs) != 1 || inputs[0] != req.Query {
		t.Fatalf("embedder saw %v, want [%q]", inputs, req.Query)
	}
	if got.Metadata["rag.chunks"] != 2 {
		t.Fatalf("metadata rag.chunks = %v, want 2", got.Metadata["rag.chunks"])
	}
	if rec.Count(MetricRetrievals) != 1 || rec.Count(MetricFailOpen) != 0 {
		t.Fatalf("counters: retrievals=%d fail_open=%d", rec.Count(MetricRetrievals), rec.Count(MetricFailOpen))
	}
}

func TestStageTruncatesOverEagerStore(t *testing.T) {
	st := storemock.NewStore()
	st.SearchFunc = func(context.Context, []float32, int) ([]pipeline.Chunk, error) {
		return []pipeline.Chunk{{Content: "1"}, {Content: "2"}, {Content: "3"}}, nil
	}
	s := NewStage(storemock.NewEmbedder(dim), st, 2)
	got, err := s.Process(context.Background(), &pipeline.Request{Query: "q", NeedsRetrieval: true})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(got.RetrievedChunks) != 2 {
		t.Fatalf("len = %d, want 2 (topK enforced by the stage)", len(got.RetrievedChunks))
	}
}

func TestFailOpen(t *testing.T) {
	boom := errors.New("boom")

	tests := []struct {
		name       string
		embedder   *storemock.Embedder
		store      *storemock.Store
		topK       int
		wantReason string
		wantSearch int
	}{
		{
			name:       "embedder error",
			embedder:   storemock.NewFailingEmbedder(boom),
			store:      seededStore(),
			topK:       3,
			wantReason: "embed_error",
			wantSearch: 0,
		},
		{
			name:       "store error",
			embedder:   storemock.NewEmbedder(dim),
			store:      storemock.NewFailingStore(boom),
			topK:       3,
			wantReason: "search_error",
			wantSearch: 1,
		},
		{
			name:       "topK not configured",
			embedder:   storemock.NewEmbedder(dim),
			store:      seededStore(),
			topK:       0,
			wantReason: "not_configured",
			wantSearch: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := metrics.NewInMemory()
			s := NewStageWith(tt.embedder, tt.store, tt.topK, WithMetrics(rec))
			req := &pipeline.Request{Query: "anything", NeedsRetrieval: true}

			got, err := s.Process(context.Background(), req)
			if err != nil {
				t.Fatalf("Process returned %v, want nil (fail-open)", err)
			}
			if len(got.RetrievedChunks) != 0 {
				t.Fatalf("RetrievedChunks = %v, want empty", got.RetrievedChunks)
			}
			if got.Metadata["rag.fail_open"] != tt.wantReason {
				t.Fatalf("fail_open reason = %v, want %q", got.Metadata["rag.fail_open"], tt.wantReason)
			}
			if tt.store.Calls() != tt.wantSearch {
				t.Fatalf("store calls = %d, want %d", tt.store.Calls(), tt.wantSearch)
			}
			if rec.Count(MetricFailOpen) != 1 {
				t.Fatalf("fail-open counter = %d, want 1", rec.Count(MetricFailOpen))
			}
		})
	}
}

func TestFailOpenWithNilDependencies(t *testing.T) {
	s := NewStage(nil, nil, 3)
	req := &pipeline.Request{Query: "q", NeedsRetrieval: true}
	got, err := s.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process returned %v, want nil (fail-open)", err)
	}
	if len(got.RetrievedChunks) != 0 {
		t.Fatalf("RetrievedChunks = %v, want empty", got.RetrievedChunks)
	}
}

func TestNilRequestIsSafe(t *testing.T) {
	s := NewStage(storemock.NewEmbedder(dim), seededStore(), 3)
	got, err := s.Process(context.Background(), nil)
	if got != nil || err != nil {
		t.Fatalf("Process(nil) = %v, %v; want nil, nil", got, err)
	}
}

func TestContextCancellationPropagates(t *testing.T) {
	emb := storemock.NewEmbedder(dim)
	st := seededStore()
	s := NewStage(emb, st, 3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := &pipeline.Request{Query: "q", NeedsRetrieval: true}
	_, err := s.Process(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if emb.Calls() != 0 || st.Calls() != 0 {
		t.Fatalf("cancelled ctx still did I/O: embed %d search %d", emb.Calls(), st.Calls())
	}
}

// A store that cancels the context mid-flight must surface the cancellation
// rather than silently failing open.
func TestCancellationDuringSearchIsNotSwallowed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := storemock.NewStore()
	st.SearchFunc = func(context.Context, []float32, int) ([]pipeline.Chunk, error) {
		cancel()
		return nil, errors.New("aborted")
	}
	s := NewStage(storemock.NewEmbedder(dim), st, 3)
	_, err := s.Process(ctx, &pipeline.Request{Query: "q", NeedsRetrieval: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// The stage must never break a pipeline: run it inside a real Pipeline with a
// failing store and assert Run still succeeds.
func TestStageIsFailOpenInsidePipeline(t *testing.T) {
	client := &recordingClient{}
	s := NewStage(storemock.NewEmbedder(dim), storemock.NewFailingStore(errors.New("db down")), 3)
	p := pipeline.New(client, s)

	resp, err := p.Run(context.Background(), &pipeline.Request{Query: "q", NeedsRetrieval: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("Content = %q, want %q", resp.Content, "ok")
	}
}

type recordingClient struct{}

func (recordingClient) Name() string { return "test" }
func (recordingClient) Send(context.Context, *pipeline.Request) (*pipeline.Response, error) {
	return &pipeline.Response{Content: "ok", ModelUsed: "test"}, nil
}
