package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os/exec"
	"strings"

	"github.com/JMjirapat/tokipe/pipeline"
)

var _ pipeline.StreamingClient = (*Client)(nil)

// maxStreamLine bounds one line of a CLI's output. A CLI can print a very long
// line legitimately (a whole file, a stack trace), so this is generous — but
// unbounded growth on a subprocess's say-so is still a memory bug.
const maxStreamLine = 4 << 20

// StreamParser converts one line of a CLI's stdout into deltas.
//
// Returning an empty slice means "nothing user-visible on this line" — the
// common case for protocol chatter. The final call has done set, so a parser
// can flush accumulated usage as a trailing delta.
type StreamParser func(line string, done bool) []pipeline.Delta

// SendStream implements pipeline.StreamingClient by reading the subprocess's
// stdout as it is produced.
//
// It requires Config.StreamParse. Without one this adapter cannot know which
// lines are answer text and which are protocol noise, and guessing would show
// users a CLI's internal chatter. When it is unset, SendStream falls back to
// Send and yields the finished output as a single delta — correct, just not
// incremental, which is exactly the contract pipeline.StreamOne exists for.
func (c *Client) SendStream(ctx context.Context, req *pipeline.Request) (iter.Seq2[pipeline.Delta, error], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.cfg.StreamParse == nil {
		resp, err := c.Send(ctx, req)
		if err != nil {
			return nil, err
		}
		return pipeline.StreamOne(resp), nil
	}

	// Not deferred: the process must outlive this call, since its output is
	// read as the consumer ranges. Cleanup is owned by the sequence.
	streamCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)

	prompt := c.cfg.Render(req)
	cmd := exec.CommandContext(streamCtx, c.cfg.Command, c.argv(prompt)...)
	cmd.Dir = c.cfg.Dir
	cmd.Env = c.cfg.Env
	if c.cfg.PromptMode == PromptViaStdin {
		cmd.Stdin = strings.NewReader(prompt)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("cli %s: stdout pipe: %w", c.cfg.Command, err)
	}
	// stderr is collected rather than streamed: CLIs use it for progress and
	// log noise, none of which belongs in a model answer. It is kept so a
	// failure can explain itself.
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("cli %s: start: %w", c.cfg.Command, err)
	}

	return c.streamLines(cmd, stdout, &stderr, cancel), nil
}

func (c *Client) streamLines(
	cmd *exec.Cmd, stdout io.ReadCloser, stderr *strings.Builder, cancel context.CancelFunc,
) iter.Seq2[pipeline.Delta, error] {
	name := c.Name()

	return func(yield func(pipeline.Delta, error) bool) {
		// Wait must run exactly once: a second call returns "Wait was already
		// called" and would mask the real exit status. reap owns it, and the
		// defer only reaps if the body did not.
		var waited bool
		reap := func() error {
			if waited {
				return nil
			}
			waited = true
			// Drain first, or a child still writing blocks on a full pipe and
			// Wait never returns.
			_, _ = io.Copy(io.Discard, io.LimitReader(stdout, maxStreamLine))
			_ = stdout.Close()
			return cmd.Wait()
		}
		// cancel kills a subprocess the consumer walked away from, rather than
		// leaving it running until its own timeout.
		defer func() {
			_ = reap()
			cancel()
		}()

		emit := func(deltas []pipeline.Delta) bool {
			for _, d := range deltas {
				if d.ModelUsed == "" {
					d.ModelUsed = name
				}
				if !yield(d, nil) {
					return false
				}
			}
			return true
		}

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64<<10), maxStreamLine)

		for scanner.Scan() {
			if !emit(c.cfg.StreamParse(scanner.Text(), false)) {
				return // consumer stopped; the defer reaps and cancels
			}
		}
		if err := scanner.Err(); err != nil {
			_ = reap()
			yield(pipeline.Delta{ModelUsed: name}, fmt.Errorf("cli %s: read stdout: %w", c.cfg.Command, err))
			return
		}

		// Final flush lets a parser emit accumulated usage.
		if !emit(c.cfg.StreamParse("", true)) {
			return
		}

		// A CLI that printed a good answer and then exited non-zero is still a
		// failure worth reporting — but the text is already in the caller's
		// hands, which is why this is a yielded error and not a returned one.
		if err := reap(); err != nil {
			yield(pipeline.Delta{ModelUsed: name}, &Error{
				Command:  c.cfg.Command,
				ExitCode: cmd.ProcessState.ExitCode(),
				Stderr:   truncate(stderr.String(), 2000),
				Err:      err,
			})
		}
	}
}

// argv builds the argument vector, substituting the prompt when the CLI takes
// it as an argument. Shared with Send so the two paths cannot diverge on how a
// command is assembled.
func (c *Client) argv(prompt string) []string {
	if c.cfg.PromptMode != PromptAsArg {
		return c.cfg.Args
	}
	args := make([]string, len(c.cfg.Args))
	for i, a := range c.cfg.Args {
		args[i] = strings.ReplaceAll(a, PromptPlaceholder, prompt)
	}
	return args
}

// --- stream parsers ---------------------------------------------------------

// ClaudeStreamParser reads `claude -p --output-format stream-json`, which emits
// one JSON object per line. Text arrives inside assistant message events;
// usage arrives with the terminal result event.
//
// Pair it with ClaudeStreamPreset, which sets the matching --output-format.
func ClaudeStreamParser() StreamParser {
	var usage pipeline.Usage
	var haveUsage bool
	// sawText tracks whether any incremental text arrived. If none did, the
	// terminal result event is the only place the answer exists.
	var sawText bool

	return func(line string, done bool) []pipeline.Delta {
		if done {
			if !haveUsage {
				return nil
			}
			u := usage
			return []pipeline.Delta{{Usage: &u}}
		}

		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			return nil
		}

		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Result  string `json:"result"`
			Message *struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Delta *struct {
				Text string `json:"text"`
			} `json:"delta"`
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			return nil
		}

		if ev.Usage != nil {
			haveUsage = true
			if ev.Usage.InputTokens > 0 {
				usage.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.OutputTokens > 0 {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				usage.CacheReadTokens = ev.Usage.CacheReadInputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				usage.CacheCreationTokens = ev.Usage.CacheCreationInputTokens
			}
		}

		var out []pipeline.Delta
		switch {
		case ev.Delta != nil && ev.Delta.Text != "":
			out = append(out, pipeline.Delta{Text: ev.Delta.Text})
		case ev.Message != nil:
			for _, b := range ev.Message.Content {
				if b.Type == "text" && b.Text != "" {
					out = append(out, pipeline.Delta{Text: b.Text})
				}
			}
		case ev.Type == "result" && ev.Result != "" && ev.Subtype == "success":
			// Only reached when the CLI reported no incremental text at all;
			// the result event then holds the whole answer.
			if !sawText {
				out = append(out, pipeline.Delta{Text: ev.Result})
			}
		}
		if len(out) > 0 {
			sawText = true
		}
		return out
	}
}

// CodexStreamParser reads `codex exec --json`, whose agent_message items carry
// the text and whose turn.completed event carries the usage.
func CodexStreamParser() StreamParser {
	var usage pipeline.Usage
	var haveUsage bool

	return func(line string, done bool) []pipeline.Delta {
		if done {
			if !haveUsage {
				return nil
			}
			u := usage
			return []pipeline.Delta{{Usage: &u}}
		}

		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			return nil
		}
		var ev codexEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			return nil
		}

		if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 || ev.Usage.CachedInputTokens > 0 {
			haveUsage = true
			usage.InputTokens += ev.Usage.InputTokens
			usage.OutputTokens += ev.Usage.OutputTokens
			usage.CacheReadTokens += ev.Usage.CachedInputTokens
		}

		if text := strings.TrimSpace(ev.Item.Text); text != "" &&
			(ev.Item.Type == "agent_message" || ev.Item.Type == "") {
			return []pipeline.Delta{{Text: text}}
		}
		return nil
	}
}

// LineStreamParser treats every non-empty line as answer text, re-adding the
// newline the scanner stripped. Correct for a CLI that prints prose and nothing
// else — the shape OpenCode has.
func LineStreamParser() StreamParser {
	first := true
	return func(line string, done bool) []pipeline.Delta {
		if done || strings.TrimSpace(line) == "" {
			return nil
		}
		if first {
			first = false
			return []pipeline.Delta{{Text: line}}
		}
		return []pipeline.Delta{{Text: "\n" + line}}
	}
}
