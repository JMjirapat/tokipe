package anthropic_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/providers/anthropic"
)

// TestPromptCachingAcrossTurns is the manual acceptance test for spec §3.2:
// two sequential calls sharing an identical static prefix must report
// CacheReadTokens > 0 on the second.
//
// It is skipped unless ANTHROPIC_API_KEY is set, so CI stays green without
// credentials. It is NOT optional at release-review time — see README.md in
// this directory for the procedure.
//
//	ANTHROPIC_API_KEY=sk-... go test -run TestPromptCachingAcrossTurns -v ./providers/anthropic/
//
// Two optional overrides let the same test run against something other than
// api.anthropic.com — a gateway, a regional endpoint, or a local recording
// proxy:
//
//	ANTHROPIC_BASE_URL   endpoint root, without /v1/messages (default https://api.anthropic.com)
//	ANTHROPIC_MODEL      model id (default the client's DefaultModel)
//
// The model matters more than it looks: the minimum cacheable prefix is
// model-dependent, so a smaller model may need a longer prefix than the one
// built below before it will cache anything at all.
func TestPromptCachingAcrossTurns(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; this is the documented manual integration test")
	}

	cfg := anthropic.Config{
		APIKey:    key,
		MaxTokens: 64,
		Timeout:   60 * time.Second,
	}
	// Empty means "use the default" in Config, so an unset variable needs no
	// special case here.
	cfg.BaseURL = os.Getenv("ANTHROPIC_BASE_URL")
	cfg.Model = os.Getenv("ANTHROPIC_MODEL")

	endpoint := cfg.BaseURL
	if endpoint == "" {
		endpoint = anthropic.DefaultBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = anthropic.DefaultModel
	}
	// Logged first: if an assertion below fails, the endpoint and model are the
	// first two things anyone will want to know, and a custom gateway is a far
	// more likely cause than a defect in the client.
	t.Logf("endpoint=%s model=%s", endpoint, model)

	client, err := anthropic.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The prefix must exceed the provider's minimum cacheable length, which is
	// well above a handful of words — hence the padding.
	prefix := "You are a test fixture for tokipe's prompt-caching integration test.\n" +
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
		t.Errorf("turn 1 wrote nothing to the cache (cache_creation_input_tokens == 0) "+
			"at %s with model %s; either the prefix is below that model's minimum "+
			"cacheable length — lengthen it, do not delete this assertion — or the "+
			"endpoint does not pass cache usage back through",
			endpoint, model)
	}

	second, err := client.Send(ctx, newRequest("Say the word: two."))
	if err != nil {
		t.Fatalf("second Send: %v", err)
	}
	t.Logf("turn 2 usage: input=%d cache_read=%d cache_creation=%d",
		second.Usage.InputTokens, second.Usage.CacheReadTokens, second.Usage.CacheCreationTokens)

	if second.Usage.CacheReadTokens == 0 {
		t.Fatalf("turn 2 reported CacheReadTokens == 0 at %s with model %s; "+
			"the identical static prefix was not reused", endpoint, model)
	}
}
