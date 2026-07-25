// Package cli adapts any command-line coding agent into a
// pipeline.ModelClient by running it as a subprocess.
//
// It exists because API credit and a subscription are different products. If
// you have a Claude Pro, Codex, or OpenCode plan but no API key, the CLI those
// plans authenticate is a perfectly good backend for agentkit — no key, no
// separate billing.
//
// The adapter is provider-agnostic by construction: it knows how to run a
// command and how to turn its stdout into a Response, and nothing else.
// Presets for the three common CLIs are in presets.go; a CLI with any other
// output shape needs only a Parser.
//
// # What you give up versus providers/anthropic
//
// A CLI builds its own prompt scaffold — system prompt, tool definitions,
// session state — and gives you no way to inject cache_control markers. So:
//
//   - Request.CacheBreakpoints cannot be transmitted. They are ignored, not
//     silently mistranslated.
//   - Reported Usage is real, but it describes the CLI's prompt, not yours.
//     Do not use it to measure agentkit's cache alignment.
//
// Everything else in the pipeline still pays off: preprocess rules still skip
// calls entirely, the tool cache still stops re-execution, compression still
// shrinks what you send, and routing still picks between backends.
//
// # Security
//
// The command is executed directly via os/exec with an explicit argument
// vector. There is no shell, so nothing in a Request can be interpreted as a
// shell metacharacter. Prompts default to being written to the subprocess's
// stdin rather than passed as an argument, which keeps them out of the process
// table and clear of ARG_MAX.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"agentkit/pipeline"
)

// PromptPlaceholder is replaced by the rendered prompt in Config.Args when
// PromptMode is PromptAsArg.
const PromptPlaceholder = "{{prompt}}"

// PromptMode selects how the prompt reaches the subprocess.
type PromptMode int

const (
	// PromptViaStdin writes the prompt to the subprocess's stdin. Default:
	// no argument-length limit, and the prompt never appears in `ps`.
	PromptViaStdin PromptMode = iota

	// PromptAsArg substitutes the prompt for PromptPlaceholder in Args.
	PromptAsArg
)

// Parser turns a command's stdout into a Response. Usage may be left zero when
// the CLI does not report it.
type Parser func(stdout []byte) (*pipeline.Response, error)

// Renderer flattens a Request into the single prompt string a CLI accepts.
type Renderer func(req *pipeline.Request) string

// DefaultTimeout bounds a single Send when Config.Timeout is zero. CLI agents
// are much slower than a raw API call — they may run tools, and they think for
// longer — so this is generous by API standards.
const DefaultTimeout = 5 * time.Minute

// Config configures a Client.
type Config struct {
	// Command is the executable to run. Required. Resolved via PATH.
	Command string

	// Args are passed verbatim. When PromptMode is PromptAsArg, exactly one
	// element should be PromptPlaceholder.
	Args []string

	// PromptMode selects stdin (default) or argument delivery.
	PromptMode PromptMode

	// Parse converts stdout into a Response. Defaults to PlainTextParser.
	Parse Parser

	// Render flattens the Request into a prompt. Defaults to RenderPrompt.
	Render Renderer

	// Dir is the subprocess working directory. Empty means the current one.
	//
	// This matters more than it looks: coding-agent CLIs read the directory
	// they are started in. Set it deliberately.
	Dir string

	// Env, if non-nil, replaces the subprocess environment entirely.
	Env []string

	// Timeout bounds a single Send. Defaults to DefaultTimeout.
	Timeout time.Duration

	// ClientName overrides the value returned by Name().
	ClientName string
}

// Client runs a CLI agent as a pipeline.ModelClient.
type Client struct {
	cfg Config
}

// Error is returned when the command fails. Stderr is truncated to keep it
// loggable; it is included because a CLI's diagnostics are usually the only
// clue about what went wrong.
type Error struct {
	Command  string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *Error) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("cli %s: exit %d: %v: %s", e.Command, e.ExitCode, e.Err, e.Stderr)
	}
	return fmt.Sprintf("cli %s: exit %d: %v", e.Command, e.ExitCode, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// ErrNoCommand is returned by New when Config.Command is empty.
var ErrNoCommand = errors.New("cli: Config.Command is required")

// New validates cfg and returns a Client. It does not run anything yet, but it
// does resolve Command against PATH so a typo fails at construction rather
// than on the first request.
func New(cfg Config) (*Client, error) {
	if cfg.Command == "" {
		return nil, ErrNoCommand
	}
	if _, err := exec.LookPath(cfg.Command); err != nil {
		return nil, fmt.Errorf("cli: %q not found on PATH: %w", cfg.Command, err)
	}
	if cfg.Parse == nil {
		cfg.Parse = PlainTextParser
	}
	if cfg.Render == nil {
		cfg.Render = RenderPrompt
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Client{cfg: cfg}, nil
}

func (c *Client) Name() string {
	if c.cfg.ClientName != "" {
		return c.cfg.ClientName
	}
	return "cli:" + c.cfg.Command
}

// Send renders req into a prompt, runs the command, and parses its stdout.
func (c *Client) Send(ctx context.Context, req *pipeline.Request) (*pipeline.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	prompt := c.cfg.Render(req)

	args := c.cfg.Args
	if c.cfg.PromptMode == PromptAsArg {
		args = make([]string, len(c.cfg.Args))
		for i, a := range c.cfg.Args {
			args[i] = strings.ReplaceAll(a, PromptPlaceholder, prompt)
		}
	}

	// exec.CommandContext, not a shell: the prompt is data, never syntax.
	cmd := exec.CommandContext(ctx, c.cfg.Command, args...)
	cmd.Dir = c.cfg.Dir
	cmd.Env = c.cfg.Env
	if c.cfg.PromptMode == PromptViaStdin {
		cmd.Stdin = strings.NewReader(prompt)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &Error{
			Command:  c.cfg.Command,
			ExitCode: cmd.ProcessState.ExitCode(),
			Stderr:   truncate(stderr.String(), 2000),
			Err:      err,
		}
	}

	resp, err := c.cfg.Parse(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("cli %s: parsing output: %w", c.cfg.Command, err)
	}
	if resp.ModelUsed == "" {
		resp.ModelUsed = c.Name()
	}
	return resp, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// RenderPrompt flattens a Request into plain text: system content first, then
// history, then retrieved context, then the current query.
//
// The ordering deliberately mirrors cache.Aligner's — static content first,
// dynamic last. A CLI gives us no cache_control markers, but most providers
// cache automatically on a matched prefix, so a stable prefix still helps.
func RenderPrompt(req *pipeline.Request) string {
	var b strings.Builder

	for _, m := range req.Messages {
		if m.Static || m.Role == "system" {
			b.WriteString(m.Content)
			b.WriteString("\n\n")
		}
	}
	for _, m := range req.Messages {
		if m.Static || m.Role == "system" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}
	if len(req.RetrievedChunks) > 0 {
		b.WriteString("\n<context>\n")
		for _, ch := range req.RetrievedChunks {
			if ch.SourceURL != "" {
				fmt.Fprintf(&b, "[%s]\n", ch.SourceURL)
			}
			b.WriteString(ch.Content)
			b.WriteString("\n")
		}
		b.WriteString("</context>\n")
	}
	if req.Query != "" {
		b.WriteString("\n")
		b.WriteString(req.Query)
	}
	return strings.TrimSpace(b.String())
}
