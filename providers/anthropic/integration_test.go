package anthropic_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"agentkit/pipeline"
	"agentkit/providers/anthropic"
)

// TestPromptCachingAcrossTurns is the manual acceptance test for spec §3.2:
// two sequential calls sharing an identical static prefix must report
// CacheReadTokens > 0 on the second.
//
// It is skipped unless ANTHROPIC_API_KEY is set, so CI stays green without
// credentials. It is NOT optional at release-review time — see README.md in
// this directory for the procedure.
//
//	ANTHROPIC_API_KEY=sk-... go test -tags= -run TestPromptCachingAcrossTurns -v ./providers/anthropic/
func TestPromptCachingAcrossTurns(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; this is the documented manual integration test")
	}

	client, err := anthropic.New(anthropic.Config{
		APIKey:    key,
		MaxTokens: 64,
		Timeout:   60 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The prefix must exceed the provider's minimum cacheable length, which is
	// well above a handful of words — hence the padding.
	prefix := "You are a test fixture for agentkit's prompt-caching integration test.\n" +
		strings.Repeat("Answer concisely and never invent facts. Cite nothing. Stay terse.\n", 120)

	newRequest := func(q string) *pipeline.Request {
		return &pipeline.Request{
			Query: q,
			Messages: []pipeline.Message{
				{Role: "system", Content: prefix, Static: true},
				{Role: "user", Content: q},
			},
			CacheBreakpoints: []pipeline.CacheBreakpoint{
				{AfterMessageIndex: 0, Reason: "static_prefix"},
			},
		}
	}

	ctx := context.Background()

	first, err := client.Send(ctx, newRequest("Say the word: one."))
	if err != nil {
		t.Fatalf("first Send: %v", err)
	}
	t.Logf("turn 1 usage: input=%d cache_read=%d cache_creation=%d",
		first.Usage.InputTokens, first.Usage.CacheReadTokens, first.Usage.CacheCreationTokens)

	if first.Usage.CacheCreationTokens == 0 {
		t.Errorf("turn 1 wrote nothing to the cache (cache_creation_input_tokens == 0); " +
			"the prefix may be below the provider's minimum cacheable length")
	}

	second, err := client.Send(ctx, newRequest("Say the word: two."))
	if err != nil {
		t.Fatalf("second Send: %v", err)
	}
	t.Logf("turn 2 usage: input=%d cache_read=%d cache_creation=%d",
		second.Usage.InputTokens, second.Usage.CacheReadTokens, second.Usage.CacheCreationTokens)

	if second.Usage.CacheReadTokens == 0 {
		t.Fatal("turn 2 reported CacheReadTokens == 0; the identical static prefix was not reused")
	}
}
