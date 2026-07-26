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
