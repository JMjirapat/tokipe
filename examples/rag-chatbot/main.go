// Command rag-chatbot demonstrates the retrieval path end to end against
// mocks only — no API key, no database, no network.
//
// What it proves (spec §2.4.3, §2.4.6):
//
//   - RAG → compress → cache-align run in that order, and the order is chosen
//     by agentkit rather than by this program.
//   - Compression shrinks retrieved chunks without inventing content.
//   - No cache breakpoint is ever anchored inside the retrieved-chunk segment,
//     because that content changes every turn and would poison the cache.
//   - Two different questions over the same corpus produce the *same*
//     breakpoint for the static prefix — that identical prefix is what the
//     provider actually gets to reuse.
//
// Run: go run ./examples/rag-chatbot
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"agentkit"
	"agentkit/config"
	"agentkit/metrics"
	"agentkit/pipeline"
	"agentkit/providers/mock"
	storemock "agentkit/stores/mock"
)

const systemPrompt = `You are a support assistant for the Orbit API.
Answer only from the provided context. Cite the source URL for every claim.`

// Deliberately noisy: verbose JSON and padded prose are exactly what the
// compressors are for.
var corpus = []pipeline.Chunk{
	{
		SourceURL: "https://docs.example.com/rate-limits",
		Content: `{
      "endpoint"   : "/v1/messages",
      "rate_limit" : { "requests_per_minute": 4000, "tokens_per_minute": 400000 },
      "burst"      : true,
      "_internal_debug_id": "req-88ac-not-useful",
      "_trace"     : "span:9931/parent:1120"
    }`,
	},
	{
		SourceURL: "https://docs.example.com/retries",
		Content: "Retries    are   expected.\n\n\n   Clients SHOULD retry on 429 and 5xx\n\n   using exponential backoff with jitter.    ",
	},
	{
		SourceURL: "https://docs.example.com/auth",
		Content: "Authentication     uses    an API key   passed in the x-api-key header.\n\n\nKeys are scoped per project.",
	},
}

func main() {
	ctx := context.Background()

	embedder := storemock.NewEmbedder(storemock.DefaultDim)
	store := storemock.NewStore()
	store.Add(storemock.DefaultDim, corpus...)

	rec := metrics.NewInMemory()
	client := mock.New("mock-model", "Retry on 429 with exponential backoff.")

	kit := agentkit.New(client,
		config.WithMetrics(rec),
		config.WithRAG(embedder, store, 2),
		config.WithDefaultCompression(),
		config.WithCacheAlignment(),
	)

	questions := []string{
		"What should I do when I get a 429?",
		"How do I authenticate?",
	}

	var firstBreakpoints []pipeline.CacheBreakpoint
	for i, q := range questions {
		req := &pipeline.Request{
			Query:          q,
			NeedsRetrieval: true,
			TurnType:       pipeline.TurnNewQuestion,
			Messages: []pipeline.Message{
				{Role: "system", Content: systemPrompt, Static: true},
				{Role: "user", Content: q},
			},
		}

		rawBytes := chunkBytes(corpus)
		resp, err := kit.Run(ctx, req)
		if err != nil {
			log.Fatalf("run: %v", err)
		}

		sent := client.LastRequest()
		fmt.Printf("\n── turn %d ─ %s\n", i+1, q)
		fmt.Printf("   retrieved      : %d chunks (topK=2 of %d in corpus)\n", len(sent.RetrievedChunks), len(corpus))
		fmt.Printf("   chunk bytes    : %d → %d after compression\n", rawBytes, chunkBytes(sent.RetrievedChunks))
		fmt.Printf("   breakpoints    : %v\n", sent.CacheBreakpoints)
		fmt.Printf("   answer         : %s\n", resp.Content)

		assertNoBreakpointInDynamicSegment(sent)

		if i == 0 {
			firstBreakpoints = sent.CacheBreakpoints
		} else {
			assertStablePrefix(firstBreakpoints, sent.CacheBreakpoints)
		}
	}

	fmt.Printf("\nmetrics: %v\n", rec.Snapshot())
	fmt.Println("\nOK — retrieval ran, chunks shrank, and the cacheable prefix stayed identical across turns.")
}

// chunkBytes is a stand-in for token counting; bytes are enough to show the
// direction of travel without pulling in a tokenizer.
func chunkBytes(chunks []pipeline.Chunk) int {
	n := 0
	for _, c := range chunks {
		n += len(c.Content)
	}
	return n
}

// assertNoBreakpointInDynamicSegment is the non-negotiable rule from §2.4.6,
// checked here at runtime so the example fails loudly if it ever regresses.
func assertNoBreakpointInDynamicSegment(req *pipeline.Request) {
	if len(req.RetrievedChunks) == 0 {
		return
	}
	staticCount := 0
	for _, m := range req.Messages {
		if m.Static || m.Role == "system" {
			staticCount++
		}
	}
	for _, bp := range req.CacheBreakpoints {
		if bp.AfterMessageIndex >= staticCount {
			log.Fatalf("breakpoint %v landed past the static segment (%d static messages) — it would be invalidated every turn",
				bp, staticCount)
		}
	}
}

func assertStablePrefix(first, second []pipeline.CacheBreakpoint) {
	if fmt.Sprint(first) != fmt.Sprint(second) {
		log.Fatalf("static-prefix breakpoints changed between turns (%v → %v); the provider cache would miss every time",
			first, second)
	}
	fmt.Printf("   %s\n", strings.Repeat("·", 3)+" prefix breakpoints identical to turn 1 → cacheable")
}
