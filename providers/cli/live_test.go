package cli_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"agentkit"
	"agentkit/config"
	"agentkit/pipeline"
	"agentkit/providers/cli"
)

// TestLiveClaudeCLI drives the real `claude` binary. It is skipped unless
// AGENTKIT_CLI_LIVE=1, because it consumes the subscription's quota and takes
// seconds rather than milliseconds.
//
//	AGENTKIT_CLI_LIVE=1 go test -run TestLiveClaudeCLI -v ./providers/cli/
//
// Unlike the Anthropic API integration test, this one needs no API key — the
// CLI is already authenticated by whatever plan the user is logged into. That
// is the whole point of this package.
func TestLiveClaudeCLI(t *testing.T) {
	if os.Getenv("AGENTKIT_CLI_LIVE") != "1" {
		t.Skip("set AGENTKIT_CLI_LIVE=1 to run against the real claude CLI")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH")
	}

	cfg := cli.ClaudePreset(t.TempDir())
	cfg.Timeout = 3 * time.Minute
	client, err := cli.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	kit := agentkit.New(client, config.WithCacheAlignment())

	resp, err := kit.Run(context.Background(), &pipeline.Request{
		Query: "Reply with exactly one word: pong",
		Messages: []pipeline.Message{
			{Role: "system", Content: "You are a terse test fixture. Obey literally.", Static: true},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	t.Logf("content=%q model=%q usage=%+v", resp.Content, resp.ModelUsed, resp.Usage)

	if !strings.Contains(strings.ToLower(resp.Content), "pong") {
		t.Errorf("content = %q, expected it to contain 'pong'", resp.Content)
	}
	if resp.ModelUsed == "" {
		t.Error("ModelUsed should be populated from the CLI's modelUsage field")
	}
	if resp.Usage.OutputTokens == 0 {
		t.Error("Usage.OutputTokens should be populated from the CLI's usage field")
	}
}
