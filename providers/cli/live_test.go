package cli_test

import (
	"context"
	"iter"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/JMjirapat/tokipe"
	"github.com/JMjirapat/tokipe/config"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/providers/cli"
)

// TestLiveCLIs drives the real binaries. Skipped unless AGENTKIT_CLI_LIVE=1,
// because each subtest spends subscription quota and takes seconds.
//
//	AGENTKIT_CLI_LIVE=1 go test -run TestLiveCLIs -v ./providers/cli/
//
// Unlike the Anthropic API integration test, this needs no API key — each CLI
// is already authenticated by whatever plan it is logged into. That is the
// whole point of this package.
//
// Verified 2026-07-26 against claude 2.1.217, codex-cli 0.144.4,
// opencode 1.17.20.
func TestLiveCLIs(t *testing.T) {
	if os.Getenv("AGENTKIT_CLI_LIVE") != "1" {
		t.Skip("set AGENTKIT_CLI_LIVE=1 to run against the real CLIs")
	}

	cases := []struct {
		name string
		cfg  cli.Config

		// wantUsage is false for CLIs that report no token counts.
		wantUsage bool
		// wantModel is false for CLIs that do not name the model they used.
		wantModel bool
		// dir must be a git repo for codex, which refuses untrusted directories.
		useRepoDir bool
	}{
		{name: "claude", cfg: cli.ClaudePreset(""), wantUsage: true, wantModel: true},
		{name: "codex", cfg: cli.CodexPreset(""), wantUsage: true, useRepoDir: true},
		{name: "opencode", cfg: cli.OpenCodePreset("", "")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath(tc.cfg.Command); err != nil {
				t.Skipf("%s not on PATH", tc.cfg.Command)
			}

			cfg := tc.cfg
			cfg.Timeout = 3 * time.Minute
			if tc.useRepoDir {
				// Run where the repo is; codex rejects untrusted directories.
				wd, err := os.Getwd()
				if err != nil {
					t.Fatalf("Getwd: %v", err)
				}
				cfg.Dir = wd
			} else {
				cfg.Dir = t.TempDir()
			}

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
				t.Error("ModelUsed should always be set, falling back to the client name")
			}
			if tc.wantModel && resp.ModelUsed == client.Name() {
				t.Errorf("ModelUsed = %q, expected the CLI's own model id", resp.ModelUsed)
			}
			if tc.wantUsage && resp.Usage.OutputTokens == 0 {
				t.Error("Usage.OutputTokens should be populated from the CLI's usage report")
			}
			if !tc.wantUsage && resp.Usage != (pipeline.Usage{}) {
				t.Errorf("expected no usage data from %s, got %+v", tc.name, resp.Usage)
			}
		})
	}
}

// TestLiveCLIStreaming drives the real binaries through SendStream. Skipped
// unless AGENTKIT_CLI_LIVE=1; each subtest spends subscription quota.
//
//	AGENTKIT_CLI_LIVE=1 go test -run TestLiveCLIStreaming -v ./providers/cli/
//
// Verified 2026-07-26 against claude 2.1.217, codex-cli 0.144.4,
// opencode 1.17.20.
//
// Note on granularity, learned here rather than assumed: Claude Code's
// stream-json emits an assistant message as a single block containing the whole
// text, so a delta is one message, not one token. OpenCode streams a line at a
// time. Neither is token-level, and no adapter can make them so — the limit is
// in the CLIs' output, and callers sizing a UI around per-token updates need
// the API backend instead.
func TestLiveCLIStreaming(t *testing.T) {
	if os.Getenv("AGENTKIT_CLI_LIVE") != "1" {
		t.Skip("set AGENTKIT_CLI_LIVE=1 to stream from the real CLIs")
	}

	cases := []struct {
		name       string
		cfg        cli.Config
		wantUsage  bool
		useRepoDir bool
	}{
		{name: "claude", cfg: cli.ClaudeStreamPreset(""), wantUsage: true},
		{name: "codex", cfg: cli.CodexStreamPreset(""), wantUsage: true, useRepoDir: true},
		{name: "opencode", cfg: cli.OpenCodeStreamPreset("", "")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath(tc.cfg.Command); err != nil {
				t.Skipf("%s not on PATH", tc.cfg.Command)
			}

			cfg := tc.cfg
			cfg.Timeout = 3 * time.Minute
			if tc.useRepoDir {
				wd, err := os.Getwd()
				if err != nil {
					t.Fatalf("Getwd: %v", err)
				}
				cfg.Dir = wd
			} else {
				cfg.Dir = t.TempDir()
			}

			client, err := cli.New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			seq, err := pipeline.New(client).RunStream(context.Background(), &pipeline.Request{
				Query: "Count from 1 to 5, one number per line. No other text.",
				Messages: []pipeline.Message{
					{Role: "system", Content: "You are a terse test fixture. Obey literally.", Static: true},
				},
			})
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}

			deltas := 0
			resp, err := pipeline.Collect(countDeltas(seq, &deltas))
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			t.Logf("deltas=%d content=%q usage=%+v", deltas, resp.Content, resp.Usage)

			if !strings.Contains(resp.Content, "1") || !strings.Contains(resp.Content, "5") {
				t.Errorf("content = %q, expected it to count", resp.Content)
			}
			if deltas == 0 {
				t.Error("no deltas were produced")
			}
			if tc.wantUsage && resp.Usage.OutputTokens == 0 {
				t.Error("expected usage from the terminal event")
			}
		})
	}
}

func countDeltas(seq iter.Seq2[pipeline.Delta, error], n *int) iter.Seq2[pipeline.Delta, error] {
	return func(yield func(pipeline.Delta, error) bool) {
		for d, err := range seq {
			if d.Text != "" {
				*n++
			}
			if !yield(d, err) {
				return
			}
		}
	}
}
