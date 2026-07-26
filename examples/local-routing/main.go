// Command local-routing demonstrates cost-aware model selection: a mixed
// workload split across a cheap "local" model and an expensive "cloud" model
// purely by HeuristicRouter, with no per-request routing code below.
//
// The point to notice is what this file does *not* contain: no if-statements
// about which model to use. The business logic builds a Request; the router
// decides. Swapping the policy means changing the tier table or the weights,
// not this loop (spec §1.5 use case 3, §3.2).
//
// Run: go run ./examples/local-routing
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/JMjirapat/tokipe"
	"github.com/JMjirapat/tokipe/config"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/providers/mock"
	"github.com/JMjirapat/tokipe/router"
)

type job struct {
	label string
	req   *pipeline.Request
}

func main() {
	ctx := context.Background()

	local := mock.New("local-8b", "handled locally")
	cloud := mock.New("cloud-frontier", "handled in the cloud")
	rec := metrics.NewInMemory()

	// Tiers are (client, max complexity). Anything scoring above every tier
	// falls through to the last one — the expensive model is the backstop,
	// never the default.
	heuristic := router.NewHeuristicRouterWith(
		[]router.Tier{
			{Client: local, MaxComplexity: 0.35},
			{Client: cloud, MaxComplexity: 1.0},
		},
		router.WithMetrics(rec),
	)

	kit := agentkit.New(cloud, config.WithMetrics(rec), config.WithRouter(heuristic))

	for _, j := range workload() {
		if _, err := kit.Run(ctx, j.req); err != nil {
			log.Fatalf("run %s: %v", j.label, err)
		}
		fmt.Printf("%-34s → %s\n", j.label, j.req.Metadata["router.client"])
	}

	total := local.Calls() + cloud.Calls()
	fmt.Printf("\nlocal: %d/%d calls (%.0f%%)   cloud: %d/%d\n",
		local.Calls(), total, 100*float64(local.Calls())/float64(total), cloud.Calls(), total)
	fmt.Printf("metrics: %v\n", rec.Snapshot())

	if local.Calls() == 0 || cloud.Calls() == 0 {
		log.Fatal("expected the workload to split across both tiers")
	}

	retuned(ctx)
	fmt.Println("\nOK — traffic split across two tiers with no manual routing in this file.")
}

// retuned shows the knob that matters in practice. Under the default weights
// (length 0.5, code 0.3, chunks 0.2) the chunk-heavy synthesis job still scores
// low enough for the local model — 14 chunks saturate the chunk signal, but
// that signal only carries a fifth of the score.
//
// If your workload's hard cases are retrieval-heavy rather than long or
// code-dense, that default is wrong for you. Changing it is a weights change,
// not a code change: the request-building logic above is untouched.
func retuned(ctx context.Context) {
	local := mock.New("local-8b", "handled locally")
	cloud := mock.New("cloud-frontier", "handled in the cloud")

	chunkHeavy := router.NewHeuristicRouterWith(
		[]router.Tier{
			{Client: local, MaxComplexity: 0.35},
			{Client: cloud, MaxComplexity: 1.0},
		},
		router.WithWeights(router.Weights{Length: 0.3, Code: 0.2, Chunks: 0.5}),
		router.WithChunkSaturation(8),
	)
	kit := agentkit.New(cloud, config.WithRouter(chunkHeavy))

	fmt.Println("\nsame workload, chunk-weighted policy (Chunks 0.2 → 0.5):")
	for _, j := range workload() {
		if _, err := kit.Run(ctx, j.req); err != nil {
			log.Fatalf("run %s: %v", j.label, err)
		}
		fmt.Printf("%-34s → %s\n", j.label, j.req.Metadata["router.client"])
	}
}

func workload() []job {
	longCode := "```go\n" + strings.Repeat("func f(x int) (int, error) { return x*2 + (x&3), nil }\n", 60) + "```"

	return []job{
		{"short classification", &pipeline.Request{Query: "Is this sentiment positive? 'great'"}},
		{"short lookup", &pipeline.Request{Query: "What is the capital of France?"}},
		{"one-line reformat", &pipeline.Request{Query: "Convert 2024-01-02 to DD/MM/YYYY"}},
		{"refactor a large code block", &pipeline.Request{
			Query:    "Refactor this for readability and explain the tradeoffs:\n" + longCode,
			Messages: []pipeline.Message{{Role: "user", Content: longCode}},
		}},
		{"synthesis over many chunks", &pipeline.Request{
			Query:           "Reconcile these sources and explain the discrepancies in detail.",
			RetrievedChunks: manyChunks(14),
		}},
	}
}

func manyChunks(n int) []pipeline.Chunk {
	out := make([]pipeline.Chunk, n)
	for i := range out {
		out[i] = pipeline.Chunk{
			Content:   fmt.Sprintf("source %d: a paragraph of retrieved evidence about the subject under discussion.", i),
			SourceURL: fmt.Sprintf("https://docs.example.com/%d", i),
		}
	}
	return out
}
