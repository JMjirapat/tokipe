package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"agentkit/pipeline"
)

// PlainTextParser treats the whole of stdout as the answer. Correct for CLIs
// that print prose and nothing else.
func PlainTextParser(stdout []byte) (*pipeline.Response, error) {
	return &pipeline.Response{Content: strings.TrimSpace(string(stdout))}, nil
}

// claudeResult is the subset of `claude -p --output-format json` this adapter
// reads. Verified against Claude Code 2.1.x; unknown fields are ignored, so
// added fields do not break parsing.
type claudeResult struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Usage   struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
}

// ClaudeJSONParser reads `claude -p --output-format json`.
//
// The reported Usage is genuine, but it measures Claude Code's own prompt —
// its system prompt and tool definitions dominate the token count. Do not read
// agentkit's cache-alignment effectiveness from these numbers.
func ClaudeJSONParser(stdout []byte) (*pipeline.Response, error) {
	var r claudeResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &r); err != nil {
		return nil, fmt.Errorf("decoding claude json: %w", err)
	}
	if r.IsError {
		return nil, fmt.Errorf("claude reported an error (subtype %q): %s", r.Subtype, r.Result)
	}

	resp := &pipeline.Response{
		Content: strings.TrimSpace(r.Result),
		Usage: pipeline.Usage{
			InputTokens:         r.Usage.InputTokens,
			OutputTokens:        r.Usage.OutputTokens,
			CacheReadTokens:     r.Usage.CacheReadInputTokens,
			CacheCreationTokens: r.Usage.CacheCreationInputTokens,
		},
	}
	// modelUsage is keyed by model id; with one key it names the model used.
	if len(r.ModelUsage) == 1 {
		for model := range r.ModelUsage {
			resp.ModelUsed = model
		}
	}
	return resp, nil
}

// codexEvent is the subset of `codex exec --json` events this adapter reads.
// Verified against codex-cli 0.144.x, whose stream looks like:
//
//	{"type":"thread.started","thread_id":"…"}
//	{"type":"turn.started"}
//	{"type":"item.completed","item":{"type":"agent_message","text":"pong"}}
//	{"type":"turn.completed","usage":{"input_tokens":15184,"cached_input_tokens":10496,"output_tokens":5}}
//
// The top-level text fields are fallbacks for other/older shapes.
type codexEvent struct {
	Type string `json:"type"`

	Item struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`

	Usage struct {
		InputTokens       int `json:"input_tokens"`
		OutputTokens      int `json:"output_tokens"`
		CachedInputTokens int `json:"cached_input_tokens"`
	} `json:"usage"`

	// Fallbacks.
	Message string `json:"message"`
	Text    string `json:"text"`
	Content string `json:"content"`
}

// CodexJSONLParser reads `codex exec --json`, one JSON event per line. It keeps
// the last agent message and takes token counts from the turn.completed events.
//
// It is deliberately permissive: unrecognised events are skipped rather than
// treated as errors, and output that is not JSONL at all falls back to being
// read as plain text. Codex's schema moves faster than Anthropic's, and a
// parser that hard-fails on an unfamiliar event would break on every upgrade.
func CodexJSONLParser(stdout []byte) (*pipeline.Response, error) {
	var (
		last  string
		usage pipeline.Usage
		seen  bool
	)

	for _, line := range bytes.Split(stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue // log noise on the same stream; not our business
		}
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		seen = true

		// The real shape: text lives on the nested item.
		if ev.Item.Text != "" && (ev.Item.Type == "agent_message" || ev.Item.Type == "") {
			if s := strings.TrimSpace(ev.Item.Text); s != "" {
				last = s
			}
		}
		// Fallbacks for shapes that put text at the top level.
		for _, candidate := range []string{ev.Message, ev.Text, ev.Content} {
			if s := strings.TrimSpace(candidate); s != "" {
				last = s
			}
		}

		usage.InputTokens += ev.Usage.InputTokens
		usage.OutputTokens += ev.Usage.OutputTokens
		usage.CacheReadTokens += ev.Usage.CachedInputTokens
	}

	if !seen {
		// Not JSONL at all — treat it as plain output rather than erroring.
		return PlainTextParser(stdout)
	}
	if last == "" {
		return nil, fmt.Errorf("codex emitted %d bytes of events but no agent message", len(stdout))
	}
	return &pipeline.Response{Content: last, Usage: usage}, nil
}

// ClaudePreset runs Claude Code non-interactively and reads its JSON output.
// Authenticated by whatever the `claude` CLI is already logged into — a Pro or
// Max subscription is enough; no API key is involved.
//
// dir is the working directory the agent sees. Pass "" for the current one.
func ClaudePreset(dir string) Config {
	return Config{
		Command:    "claude",
		Args:       []string{"-p", "--output-format", "json"},
		PromptMode: PromptViaStdin,
		Parse:      ClaudeJSONParser,
		Dir:        dir,
		ClientName: "claude-cli",
	}
}

// CodexPreset runs Codex non-interactively and reads its JSONL event stream.
//
// Codex refuses to run outside a trusted directory, failing with "Not inside a
// trusted directory and --skip-git-repo-check was not specified". Pass a dir
// inside a git repository, or append "--skip-git-repo-check" to Args yourself.
// The preset does not add that flag: it is Codex's own safety guard, and
// disabling it should be the caller's explicit choice, not a hidden default.
func CodexPreset(dir string) Config {
	return Config{
		Command:    "codex",
		Args:       []string{"exec", "--json", "-"},
		PromptMode: PromptViaStdin,
		Parse:      CodexJSONLParser,
		Dir:        dir,
		ClientName: "codex-cli",
	}
}

// OpenCodePreset runs OpenCode non-interactively. It prints prose, so there is
// no usage data to report — Response.Usage stays zero.
//
// model is the "provider/model" string OpenCode expects; pass "" for its
// configured default.
func OpenCodePreset(dir, model string) Config {
	args := []string{"run"}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, PromptPlaceholder)

	// The model goes in the name so two tiers backed by different opencode
	// models are distinguishable in routing decisions, logs, and metrics.
	name := "opencode-cli"
	if model != "" {
		name = "opencode:" + model
	}

	return Config{
		Command:    "opencode",
		Args:       args,
		PromptMode: PromptAsArg,
		Parse:      PlainTextParser,
		Dir:        dir,
		ClientName: name,
	}
}

// --- streaming presets ------------------------------------------------------

// ClaudeStreamPreset is ClaudePreset with incremental output enabled.
//
// It requests `--output-format stream-json`, which emits one JSON object per
// line as the answer is produced, and `--verbose`, which Claude Code requires
// alongside stream-json in print mode. Parse is still ClaudeJSONParser so a
// plain Send on the same client keeps working.
func ClaudeStreamPreset(dir string) Config {
	cfg := ClaudePreset(dir)
	cfg.Args = []string{"-p", "--output-format", "stream-json", "--verbose"}
	cfg.StreamParse = ClaudeStreamParser()
	cfg.ClientName = "claude-cli-stream"
	return cfg
}

// CodexStreamPreset is CodexPreset with incremental output enabled. Codex's
// `exec --json` already streams events, so only the parser changes.
//
// The same directory restriction applies as for CodexPreset: Codex refuses to
// run outside a trusted directory.
func CodexStreamPreset(dir string) Config {
	cfg := CodexPreset(dir)
	cfg.StreamParse = CodexStreamParser()
	cfg.ClientName = "codex-cli-stream"
	return cfg
}

// OpenCodeStreamPreset is OpenCodePreset reading stdout line by line.
//
// OpenCode prints prose with no envelope, so every line is answer text. That
// makes the increments coarser than the other two — a line at a time rather
// than a token at a time — which is the honest limit of what its output shape
// allows, not something this adapter can improve on.
func OpenCodeStreamPreset(dir, model string) Config {
	cfg := OpenCodePreset(dir, model)
	cfg.StreamParse = LineStreamParser()
	if model != "" {
		cfg.ClientName = "opencode-stream:" + model
	} else {
		cfg.ClientName = "opencode-cli-stream"
	}
	return cfg
}
