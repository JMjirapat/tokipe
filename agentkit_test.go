package agentkit_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JMjirapat/tokipe"
	"github.com/JMjirapat/tokipe/cache"
	"github.com/JMjirapat/tokipe/config"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/preprocess"
	"github.com/JMjirapat/tokipe/providers/mock"
	"github.com/JMjirapat/tokipe/router"
	storemock "github.com/JMjirapat/tokipe/stores/mock"
	"github.com/JMjirapat/tokipe/toolcache"
)

// TestConcurrentRunsShareStatefulComponents is the Phase 4 stress test: many
// goroutines driving one Pipeline whose ToolCache, Aligner, vector store and
// metrics recorder are all shared. Run under -race, this is what proves the
// stateful components are safe to reuse across requests.
func TestConcurrentRunsShareStatefulComponents(t *testing.T) {
	const (
		goroutines = 24
		perG       = 20
	)

	var execs atomic.Int64
	exec := func(_ context.Context, call pipeline.ToolCall) (any, error) {
		execs.Add(1)
		return "result of " + call.Name, nil
	}

	store := storemock.NewStore()
	store.Add(storemock.DefaultDim,
		pipeline.Chunk{SourceURL: "a", Content: "  alpha   content  "},
		pipeline.Chunk{SourceURL: "b", Content: `{"k":"v","_debug":"drop"}`},
		pipeline.Chunk{SourceURL: "c", Content: "  gamma   content  "},
	)

	rec := metrics.NewInMemory()
	cheap := mock.New("cheap", "ok")
	strong := mock.New("strong", "ok")

	// One kit, one cache, one aligner, one store — shared by every goroutine.
	kit := agentkit.New(strong,
		config.WithMetrics(rec),
		config.WithPreprocess(evenRule{}),
		config.WithToolCache(toolcache.NewMemoryCache(), exec, time.Minute),
		config.WithRAG(storemock.NewEmbedder(storemock.DefaultDim), store, 2),
		config.WithDefaultCompression(),
		config.WithCacheAlignment(cache.DefaultSpecs()...),
		config.WithRouter(router.NewHeuristicRouter(
			router.Tier{Client: cheap, MaxComplexity: 0.4},
			router.Tier{Client: strong, MaxComplexity: 1.0},
		)),
	)

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perG)

	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range perG {
				n := g*perG + i
				resp, err := kit.Run(context.Background(), request(n))
				if err != nil {
					errs <- fmt.Errorf("goroutine %d iteration %d: %w", g, i, err)
					return
				}
				if resp == nil {
					errs <- fmt.Errorf("goroutine %d iteration %d: nil response", g, i)
					return
				}
				if n%2 == 0 && !resp.ShortCircuited {
					errs <- fmt.Errorf("goroutine %d iteration %d: even request should short-circuit", g, i)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	total := goroutines * perG
	if got := rec.Count(preprocess.CounterShortCircuit); got != total/2 {
		t.Errorf("short circuits = %d, want %d", got, total/2)
	}

	// Only two distinct tool calls exist across every goroutine. The cache
	// collapses repeats over time and singleflight collapses them across
	// concurrent callers, so the total must be exactly 2 — a stampede would
	// show up here as anything more.
	if n := execs.Load(); n != 2 {
		t.Errorf("tool executed %d times for 2 distinct calls, want exactly 2", n)
	}
	t.Logf("%d runs, %d tool executions, metrics=%v", total, execs.Load(), rec.Snapshot())
}

func request(n int) *pipeline.Request {
	req := &pipeline.Request{
		Query: fmt.Sprintf("request number %d", n),
		Messages: []pipeline.Message{
			{Role: "system", Content: "shared static system prompt", Static: true},
			{Role: "user", Content: fmt.Sprintf("turn %d", n)},
		},
		NeedsRetrieval: n%3 == 0,
	}
	if n%5 == 0 {
		req.ToolCalls = []pipeline.ToolCall{
			{Name: "search", Args: map[string]any{"q": "shared", "limit": 10}},
			{Name: "read", Args: map[string]any{"path": "main.go"}},
		}
	}
	if n%7 == 0 {
		req.Query += "\n```go\n" + strings.Repeat("func f() {}\n", 40) + "```"
	}
	return req
}

// evenRule short-circuits every even-numbered request, so half the load never
// reaches a model client at all.
type evenRule struct{}

func (evenRule) Name() string { return "even" }

func (evenRule) CanHandle(req *pipeline.Request) bool {
	var n int
	if _, err := fmt.Sscanf(req.Query, "request number %d", &n); err != nil {
		return false
	}
	return n%2 == 0
}

func (evenRule) Handle(*pipeline.Request) (*pipeline.Response, error) {
	return &pipeline.Response{Content: "even", ModelUsed: "none"}, nil
}

// TestPassThroughPipelineIsTheBaseline documents that a kit with no options is
// exactly equivalent to calling the client directly — the property the
// benchmark's baseline arm relies on.
func TestPassThroughPipelineIsTheBaseline(t *testing.T) {
	client := mock.New("m", "answer")
	kit := agentkit.New(client)

	req := &agentkit.Request{Query: "hello"}
	resp, err := kit.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Content != "answer" || client.Calls() != 1 {
		t.Fatalf("unexpected: resp=%+v calls=%d", resp, client.Calls())
	}
	if client.LastRequest() != req {
		t.Fatal("a pass-through pipeline must not modify the request")
	}
}

func TestNewFromConfigMatchesNew(t *testing.T) {
	client := mock.New("m", "ok")
	opts := []config.Option{config.WithDefaultCompression(), config.WithCacheAlignment()}

	fromConfig := agentkit.NewFromConfig(client, config.New(opts...))
	if _, err := fromConfig.Run(context.Background(), &agentkit.Request{Query: "hi"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if client.Calls() != 1 {
		t.Fatalf("want 1 call, got %d", client.Calls())
	}
}
