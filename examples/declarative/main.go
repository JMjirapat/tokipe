// Command declarative builds a pipeline from a JSON document instead of from Go
// code, then runs both entry points against it: Run, which sends; and Prepare,
// which shapes the request and hands it back for the caller to send itself.
//
// Those two are the shapes an external agent runtime needs. An orchestrator that
// owns its own model call — its own SDK, its own credentials — uses Prepare and
// sends what it gets back. One that would rather delegate the whole turn uses
// Run. Both get identical stage behaviour, because both are the same code path.
//
// Run: go run ./examples/declarative
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/runtime"
)

// The document an operator would edit. It uses the mock provider so this
// example needs no key and no network; changing "mock" to "openai" with a
// base_url is the whole difference between this and a local Ollama.
const document = `{
  "provider": {
    "type": "mock",
    "model": "demo-model",
    "options": {"content": "the model answered"}
  },
  "stages": {
    "preprocess": {
      "rules": [
        {"match": "/help", "respond": "commands: /help, /status"},
        {"match": "ping",  "respond": "pong", "case_insensitive": true}
      ]
    },
    "dedupe": {},
    "compression": {"preset": "default"},
    "history_budget": {"preset": "default"},
    "cache_alignment": {}
  },
  "router": {
    "tiers": [
      {"provider": {"type": "mock", "model": "cheap-8b",  "options": {"content": "handled cheaply"}},  "max_complexity": 0.35},
      {"provider": {"type": "mock", "model": "strong-4o", "options": {"content": "handled by the big one"}}, "max_complexity": 1.0}
    ]
  }
}`

func main() {
	spec, err := runtime.Parse([]byte(document))
	if err != nil {
		log.Fatalf("parse: %v", err)
	}

	// A nil registry means the built-ins. A program that wants pgvector, Redis
	// or its own Go-written rules registers them on a Registry and passes that.
	kit, err := runtime.Build(spec, nil)
	if err != nil {
		log.Fatalf("build: %v", err)
	}

	ctx := context.Background()

	fmt.Println("── Run: tokipe performs the model call ──")
	for _, q := range []string{"/help", "PING", "explain the deployment failure"} {
		resp, err := kit.Run(ctx, &pipeline.Request{Query: q})
		if err != nil {
			log.Fatalf("run %q: %v", q, err)
		}
		fmt.Printf("  %-32q → %-26q short_circuit=%v\n", q, resp.Content, resp.ShortCircuited)
	}

	fmt.Println("\n── Prepare: the caller performs the model call ──")

	// A realistic turn: a stable system prompt the provider can cache, some
	// history, and a question.
	req := &pipeline.Request{
		Query: "why did the deployment fail?",
		Messages: []pipeline.Message{
			{Role: "system", Content: "You are a terse incident assistant.", Static: true},
			{Role: "user", Content: "deploy 41 went out at 14:02"},
			{Role: "assistant", Content: "acknowledged"},
		},
		TurnType: pipeline.TurnErrorRecovery,
	}

	prep, err := kit.Prepare(ctx, req)
	if err != nil {
		log.Fatalf("prepare: %v", err)
	}
	if prep.ShortCircuited() {
		fmt.Printf("  answered without a model: %q\n", prep.Response.Content)
		return
	}

	fmt.Printf("  routed to        : %s\n", prep.Client.Name())
	fmt.Printf("  messages         : %d\n", len(prep.Request.Messages))
	fmt.Printf("  cache breakpoints: %d", len(prep.Request.CacheBreakpoints))
	for _, bp := range prep.Request.CacheBreakpoints {
		fmt.Printf(" (after message %d, %s)", bp.AfterMessageIndex, bp.Reason)
	}
	fmt.Println()

	// This is what crosses the process boundary to the agent that asked for it.
	// Externalize drops the internal short-circuit key and elides anything that
	// cannot be encoded, so the result always marshals.
	out, err := json.MarshalIndent(pipeline.Externalize(prep.Request), "  ", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	fmt.Println("\n  the shaped request, as the caller would receive it:")
	fmt.Println("  " + strings.ReplaceAll(string(out), "\n", "\n  "))

	// The caller sends it with its own client. Here we use the one the router
	// picked, which is what tokipe would have done.
	resp, err := prep.Client.Send(ctx, prep.Request)
	if err != nil {
		log.Fatalf("send: %v", err)
	}
	fmt.Printf("\n  sent by the caller → %q (model %s)\n", resp.Content, resp.ModelUsed)

	if len(os.Args) > 1 && os.Args[1] == "-json" {
		_ = json.NewEncoder(os.Stdout).Encode(spec)
	}
}
