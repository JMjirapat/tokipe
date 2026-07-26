package cli_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/providers/cli"
	"github.com/JMjirapat/tokipe/toolcache"
)

// The tests drive a fake CLI by re-executing this test binary with
// GO_CLI_HELPER set — the standard os/exec testing pattern. No fixture
// scripts, no shell, works identically on every platform Go supports.
const helperEnv = "GO_CLI_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())

	case "echo_stdin": // prove the prompt arrives on stdin, unmangled
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		fmt.Print(b.String())

	case "echo_args": // prove argv substitution happened
		fmt.Print(strings.Join(os.Args[1:], "|"))

	case "claude_json":
		fmt.Print(`{"type":"result","subtype":"success","is_error":false,` +
			`"result":"  pong  ","usage":{"input_tokens":2,"output_tokens":4,` +
			`"cache_read_input_tokens":4096,"cache_creation_input_tokens":512},` +
			`"modelUsage":{"claude-sonnet-5":{"inputTokens":2}}}`)

	case "claude_error":
		fmt.Print(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"ran out of turns"}`)

	case "codex_jsonl":
		// The real codex-cli 0.144.x stream, plus a line of log noise.
		fmt.Println(`{"type":"thread.started","thread_id":"019f9d29"}`)
		fmt.Println(`ERROR codex_models_manager::cache: failed to load models cache`)
		fmt.Println(`{"type":"turn.started"}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"first"}}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"final answer"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":7,"output_tokens":3}}`)

	case "codex_no_message":
		fmt.Println(`{"type":"thread.started","thread_id":"x"}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1}}`)

	case "fail":
		fmt.Fprintln(os.Stderr, "something broke")
		os.Exit(3)

	case "stream_lines":
		for _, l := range []string{"first line", "", "second line", "third line"} {
			fmt.Println(l)
		}

	case "stream_claude":
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}`)
		fmt.Println(`not json`)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":", world"}]}}`)
		fmt.Println(`{"type":"result","subtype":"success","result":"Hello, world",` +
			`"usage":{"input_tokens":11,"output_tokens":4,"cache_read_input_tokens":2048}}`)

	case "stream_codex":
		fmt.Println(`{"type":"thread.started","thread_id":"x"}`)
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"streamed"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":9,"cached_input_tokens":3,"output_tokens":2}}`)

	case "stream_then_fail":
		fmt.Println("partial output")
		fmt.Fprintln(os.Stderr, "then it broke")
		os.Exit(4)

	case "slow_stream":
		// One line, then a long pause. A consumer that abandons after the first
		// delta must not wait for this.
		fmt.Println("first line")
		time.Sleep(10 * time.Second)
		fmt.Println("second line")

	case "hang":
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

// helperConfig returns a Config that runs this test binary as the fake CLI.
func helperConfig(t *testing.T, mode string) cli.Config {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return cli.Config{
		Command: self,
		Env:     append(os.Environ(), helperEnv+"="+mode),
		Timeout: 30 * time.Second,
	}
}

func mustClient(t *testing.T, cfg cli.Config) *cli.Client {
	t.Helper()
	c, err := cli.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestPromptReachesStdinIntact(t *testing.T) {
	c := mustClient(t, helperConfig(t, "echo_stdin"))

	req := &pipeline.Request{
		Query: "what changed?",
		Messages: []pipeline.Message{
			{Role: "system", Content: "SYSTEM PROMPT", Static: true},
			{Role: "user", Content: "earlier turn"},
		},
		RetrievedChunks: []pipeline.Chunk{{SourceURL: "docs/a.md", Content: "chunk body"}},
	}

	resp, err := c.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	for _, want := range []string{"SYSTEM PROMPT", "earlier turn", "docs/a.md", "chunk body", "what changed?"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("prompt is missing %q:\n%s", want, resp.Content)
		}
	}
	// Static content must lead, so the prefix stays stable across turns.
	if !strings.HasPrefix(resp.Content, "SYSTEM PROMPT") {
		t.Errorf("static content must come first, got:\n%s", resp.Content)
	}
	// Dynamic content must trail the history.
	if strings.Index(resp.Content, "earlier turn") > strings.Index(resp.Content, "chunk body") {
		t.Error("retrieved chunks must come after conversation history")
	}
}

// A prompt is data. If it were ever handed to a shell, this test would show it.
func TestShellMetacharactersAreNotInterpreted(t *testing.T) {
	c := mustClient(t, helperConfig(t, "echo_stdin"))

	nasty := "$(touch /tmp/agentkit-pwned); `id`; rm -rf / & echo done | tee x > y"
	resp, err := c.Send(context.Background(), &pipeline.Request{Query: nasty})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Content != nasty {
		t.Fatalf("prompt was altered in transit:\n got: %q\nwant: %q", resp.Content, nasty)
	}
	if _, err := os.Stat("/tmp/agentkit-pwned"); err == nil {
		os.Remove("/tmp/agentkit-pwned")
		t.Fatal("command substitution executed — the prompt reached a shell")
	}
}

func TestPromptAsArgSubstitutesPlaceholder(t *testing.T) {
	cfg := helperConfig(t, "echo_args")
	cfg.PromptMode = cli.PromptAsArg
	cfg.Args = []string{"run", "--model", "x/y", cli.PromptPlaceholder}

	resp, err := mustClient(t, cfg).Send(context.Background(), &pipeline.Request{Query: "hello"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Content != "run|--model|x/y|hello" {
		t.Fatalf("argv = %q", resp.Content)
	}
}

func TestClaudeJSONParserPopulatesUsage(t *testing.T) {
	cfg := helperConfig(t, "claude_json")
	cfg.Parse = cli.ClaudeJSONParser

	resp, err := mustClient(t, cfg).Send(context.Background(), &pipeline.Request{Query: "ping"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Content != "pong" {
		t.Errorf("Content = %q, want trimmed %q", resp.Content, "pong")
	}
	if resp.ModelUsed != "claude-sonnet-5" {
		t.Errorf("ModelUsed = %q", resp.ModelUsed)
	}
	want := pipeline.Usage{InputTokens: 2, OutputTokens: 4, CacheReadTokens: 4096, CacheCreationTokens: 512}
	if resp.Usage != want {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestClaudeErrorResultIsAnError(t *testing.T) {
	cfg := helperConfig(t, "claude_error")
	cfg.Parse = cli.ClaudeJSONParser

	_, err := mustClient(t, cfg).Send(context.Background(), &pipeline.Request{Query: "ping"})
	if err == nil {
		t.Fatal("is_error:true must surface as an error")
	}
	if !strings.Contains(err.Error(), "error_max_turns") {
		t.Errorf("error should name the subtype, got: %v", err)
	}
}

func TestCodexJSONLParserKeepsLastMessageAndSumsUsage(t *testing.T) {
	cfg := helperConfig(t, "codex_jsonl")
	cfg.Parse = cli.CodexJSONLParser

	resp, err := mustClient(t, cfg).Send(context.Background(), &pipeline.Request{Query: "ping"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Content != "final answer" {
		t.Errorf("Content = %q, want the last message", resp.Content)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.CacheReadTokens != 7 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
}

func TestCodexParserFallsBackToPlainText(t *testing.T) {
	resp, err := cli.CodexJSONLParser([]byte("just prose, no events\n"))
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	if resp.Content != "just prose, no events" {
		t.Errorf("Content = %q", resp.Content)
	}
}

func TestNonZeroExitCarriesStderrAndCode(t *testing.T) {
	_, err := mustClient(t, helperConfig(t, "fail")).Send(context.Background(), &pipeline.Request{Query: "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	var cliErr *cli.Error
	if !errors.As(err, &cliErr) {
		t.Fatalf("want *cli.Error, got %T: %v", err, err)
	}
	if cliErr.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", cliErr.ExitCode)
	}
	if !strings.Contains(cliErr.Stderr, "something broke") {
		t.Errorf("Stderr = %q", cliErr.Stderr)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Error("cli.Error must unwrap to the underlying *exec.ExitError")
	}
}

func TestTimeoutReturnsContextError(t *testing.T) {
	cfg := helperConfig(t, "hang")
	cfg.Timeout = 100 * time.Millisecond

	start := time.Now()
	_, err := mustClient(t, cfg).Send(context.Background(), &pipeline.Request{Query: "x"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout not honoured: took %v", elapsed)
	}
}

func TestCallerCancellationBeatsTheTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := mustClient(t, helperConfig(t, "hang")).Send(ctx, &pipeline.Request{Query: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNewValidatesCommand(t *testing.T) {
	if _, err := cli.New(cli.Config{}); !errors.Is(err, cli.ErrNoCommand) {
		t.Errorf("empty Command: err = %v, want ErrNoCommand", err)
	}
	if _, err := cli.New(cli.Config{Command: "definitely-not-a-real-binary-xyzzy"}); err == nil {
		t.Error("a command missing from PATH should fail at construction")
	}
}

func TestNameDefaultsToCommand(t *testing.T) {
	c := mustClient(t, cli.Config{Command: "go"})
	if c.Name() != "cli:go" {
		t.Errorf("Name = %q", c.Name())
	}
	named := mustClient(t, cli.Config{Command: "go", ClientName: "custom"})
	if named.Name() != "custom" {
		t.Errorf("Name = %q", named.Name())
	}
}

func TestImplementsModelClient(t *testing.T) {
	var _ pipeline.ModelClient = mustClient(t, cli.Config{Command: "go"})
}

func TestRenderPromptOnEmptyRequest(t *testing.T) {
	if got := cli.RenderPrompt(&pipeline.Request{}); got != "" {
		t.Errorf("empty request should render empty, got %q", got)
	}
}

func TestRenderPromptIncludesCachedToolResults(t *testing.T) {
	req := &pipeline.Request{Query: "summarize"}
	req.SetMeta(toolcache.MetaResults, map[string]any{
		"hash-b": "second result",
		"hash-a": "first result",
	})

	got := cli.RenderPrompt(req)
	for _, want := range []string{"<tool_results>", "first result", "second result", "</tool_results>"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
	// Results must precede the query, like every other piece of context.
	if strings.Index(got, "first result") > strings.Index(got, "summarize") {
		t.Error("tool results must come before the query")
	}
}

// Map iteration order must not leak into the prompt, or an otherwise identical
// turn renders differently each run and defeats the backend's own prefix cache.
func TestRenderPromptIsDeterministicAcrossMapOrder(t *testing.T) {
	results := map[string]any{}
	for i := range 20 {
		results[fmt.Sprintf("hash-%02d", i)] = fmt.Sprintf("result %d", i)
	}

	req := &pipeline.Request{Query: "q"}
	req.SetMeta(toolcache.MetaResults, results)

	first := cli.RenderPrompt(req)
	for range 50 {
		if got := cli.RenderPrompt(req); got != first {
			t.Fatal("RenderPrompt is not deterministic across map iteration order")
		}
	}
}

func TestRenderPromptIgnoresMalformedToolResults(t *testing.T) {
	req := &pipeline.Request{Query: "q"}
	req.SetMeta(toolcache.MetaResults, "not a map")

	if got := cli.RenderPrompt(req); got != "q" {
		t.Errorf("a malformed results value must be skipped, got %q", got)
	}
}

func TestCodexParserErrorsWhenNoAgentMessage(t *testing.T) {
	cfg := helperConfig(t, "codex_no_message")
	cfg.Parse = cli.CodexJSONLParser

	_, err := mustClient(t, cfg).Send(context.Background(), &pipeline.Request{Query: "ping"})
	if err == nil {
		t.Fatal("a stream with events but no agent message must be an error, not empty content")
	}
	if !strings.Contains(err.Error(), "no agent message") {
		t.Errorf("error should say what was missing, got: %v", err)
	}
}

func TestPresetsAreWiredToTheRightParsers(t *testing.T) {
	cases := []struct {
		name    string
		cfg     cli.Config
		command string
		wantArg string
	}{
		{"claude", cli.ClaudePreset(""), "claude", "--output-format"},
		{"codex", cli.CodexPreset(""), "codex", "--json"},
		{"opencode", cli.OpenCodePreset("", "anthropic/claude-sonnet-4-6"), "opencode", "--model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cfg.Command != tc.command {
				t.Errorf("Command = %q, want %q", tc.cfg.Command, tc.command)
			}
			if !slices.Contains(tc.cfg.Args, tc.wantArg) {
				t.Errorf("Args %v missing %q", tc.cfg.Args, tc.wantArg)
			}
			if tc.cfg.Parse == nil {
				t.Error("preset must set a Parser")
			}
		})
	}

	// opencode takes the prompt as an argument, the other two via stdin.
	if cli.OpenCodePreset("", "").PromptMode != cli.PromptAsArg {
		t.Error("opencode preset should pass the prompt as an argument")
	}
	if cli.ClaudePreset("").PromptMode != cli.PromptViaStdin {
		t.Error("claude preset should pass the prompt via stdin")
	}
}

// Two tiers backed by different opencode models must be distinguishable, or
// routing decisions, logs, and metrics all collapse into one indistinct name.
func TestOpenCodePresetNamesIncludeTheModel(t *testing.T) {
	cheap := cli.OpenCodePreset("", "opencode-go/deepseek-v4-flash")
	strong := cli.OpenCodePreset("", "opencode-go/qwen3.7-max")

	if cheap.ClientName == strong.ClientName {
		t.Fatalf("both tiers report %q; they must differ", cheap.ClientName)
	}
	if !strings.Contains(cheap.ClientName, "deepseek-v4-flash") {
		t.Errorf("ClientName = %q, expected it to name the model", cheap.ClientName)
	}
	if got := cli.OpenCodePreset("", "").ClientName; got != "opencode-cli" {
		t.Errorf("with no model, ClientName = %q, want the plain default", got)
	}
	// The model must also actually reach the command line, not just the name.
	if !slices.Contains(cheap.Args, "opencode-go/deepseek-v4-flash") {
		t.Errorf("Args %v missing the model", cheap.Args)
	}
}

// --- streaming (Phase 5) ----------------------------------------------------

func drainStream(t *testing.T, c *cli.Client, req *pipeline.Request) ([]string, *pipeline.Response, error) {
	t.Helper()
	seq, err := c.SendStream(context.Background(), req)
	if err != nil {
		return nil, nil, err
	}

	var texts []string
	resp := &pipeline.Response{}
	var b strings.Builder
	var streamErr error
	for d, err := range seq {
		if d.Text != "" {
			texts = append(texts, d.Text)
			b.WriteString(d.Text)
		}
		if d.Usage != nil {
			resp.Usage = *d.Usage
		}
		if d.ModelUsed != "" {
			resp.ModelUsed = d.ModelUsed
		}
		if err != nil {
			streamErr = err
			break
		}
	}
	resp.Content = b.String()
	return texts, resp, streamErr
}

func TestLineStreamParserYieldsOneDeltaPerLine(t *testing.T) {
	cfg := helperConfig(t, "stream_lines")
	cfg.StreamParse = cli.LineStreamParser()

	texts, resp, err := drainStream(t, mustClient(t, cfg), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	// Blank lines carry nothing and must not become deltas.
	if len(texts) != 3 {
		t.Fatalf("got %d deltas (%q), want 3 non-empty lines", len(texts), texts)
	}
	if resp.Content != "first line\nsecond line\nthird line" {
		t.Errorf("Content = %q, want newlines restored between lines", resp.Content)
	}
}

func TestClaudeStreamParserYieldsTextAndFinalUsage(t *testing.T) {
	cfg := helperConfig(t, "stream_claude")
	cfg.StreamParse = cli.ClaudeStreamParser()

	texts, resp, err := drainStream(t, mustClient(t, cfg), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if strings.Join(texts, "|") != "Hello|, world" {
		t.Errorf("deltas = %q, want the two incremental texts", texts)
	}
	if resp.Content != "Hello, world" {
		t.Errorf("Content = %q", resp.Content)
	}
	// The result event repeats the whole answer; it must not be appended again.
	if strings.Count(resp.Content, "Hello") != 1 {
		t.Errorf("the terminal result duplicated the text: %q", resp.Content)
	}
	want := pipeline.Usage{InputTokens: 11, OutputTokens: 4, CacheReadTokens: 2048}
	if resp.Usage != want {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestCodexStreamParserYieldsTextAndUsage(t *testing.T) {
	cfg := helperConfig(t, "stream_codex")
	cfg.StreamParse = cli.CodexStreamParser()

	_, resp, err := drainStream(t, mustClient(t, cfg), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if resp.Content != "streamed" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.Usage.InputTokens != 9 || resp.Usage.CacheReadTokens != 3 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
}

// Without a StreamParse, SendStream must still work — buffered, one delta.
// That is what lets a caller use RunStream against any CLI.
func TestSendStreamWithoutParserFallsBackToOneDelta(t *testing.T) {
	cfg := helperConfig(t, "stream_lines") // no StreamParse set
	texts, resp, err := drainStream(t, mustClient(t, cfg), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(texts) != 1 {
		t.Fatalf("got %d deltas, want a single buffered one", len(texts))
	}
	if !strings.Contains(resp.Content, "third line") {
		t.Errorf("Content = %q, want the whole output", resp.Content)
	}
}

// Text already delivered must survive a non-zero exit.
func TestStreamExitFailureIsYieldedAfterPartialText(t *testing.T) {
	cfg := helperConfig(t, "stream_then_fail")
	cfg.StreamParse = cli.LineStreamParser()

	_, resp, err := drainStream(t, mustClient(t, cfg), &pipeline.Request{Query: "q"})
	if err == nil {
		t.Fatal("a non-zero exit must surface")
	}
	var cliErr *cli.Error
	if !errors.As(err, &cliErr) {
		t.Fatalf("err = %v (%T), want *cli.Error", err, err)
	}
	if cliErr.ExitCode != 4 {
		t.Errorf("ExitCode = %d, want 4", cliErr.ExitCode)
	}
	if !strings.Contains(cliErr.Stderr, "then it broke") {
		t.Errorf("Stderr = %q", cliErr.Stderr)
	}
	if resp.Content != "partial output" {
		t.Errorf("Content = %q, want the text printed before the failure", resp.Content)
	}
}

func TestStreamCanceledContextFailsUpFront(t *testing.T) {
	cfg := helperConfig(t, "stream_lines")
	cfg.StreamParse = cli.LineStreamParser()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mustClient(t, cfg).SendStream(ctx, &pipeline.Request{Query: "q"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Walking away mid-stream must reap the subprocess, not leave it running.
func TestAbandonedStreamReapsTheSubprocess(t *testing.T) {
	cfg := helperConfig(t, "stream_lines")
	cfg.StreamParse = cli.LineStreamParser()
	c := mustClient(t, cfg)

	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	for range seq {
		break
	}

	// A second full stream on the same client proves nothing was wedged.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = drainStream(t, c, &pipeline.Request{Query: "again"})
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the client is wedged after an abandoned stream")
	}
}

func TestStreamPresetsAreWired(t *testing.T) {
	cases := map[string]cli.Config{
		"claude":   cli.ClaudeStreamPreset(""),
		"codex":    cli.CodexStreamPreset(""),
		"opencode": cli.OpenCodeStreamPreset("", "p/m"),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if cfg.StreamParse == nil {
				t.Error("a stream preset must set StreamParse")
			}
			if cfg.Parse == nil {
				t.Error("Parse must remain set so plain Send still works")
			}
		})
	}
	if !slices.Contains(cli.ClaudeStreamPreset("").Args, "stream-json") {
		t.Error("the claude stream preset must request stream-json")
	}
}

func TestClientSatisfiesStreamingClient(t *testing.T) {
	var _ pipeline.StreamingClient = mustClient(t, cli.Config{Command: "go"})
}

// QA-REPORT-2 M3: the deferred cleanup reaped before cancelling, and reaping
// waits for the child. A consumer that broke out of the range loop therefore
// blocked until the CLI exited on its own or the timeout fired — holding a
// request goroutine and a subprocess long after the client had disconnected.
func TestAbandoningAStreamDoesNotWaitForTheChild(t *testing.T) {
	cfg := helperConfig(t, "slow_stream")
	cfg.StreamParse = cli.LineStreamParser()
	cfg.Timeout = 30 * time.Second // long, so a regression blocks rather than times out

	c := mustClient(t, cfg)
	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "q"})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		for range seq {
			break // walk away after the first delta
		}
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		// The child sleeps 10s. Anything near that means we waited for it.
		if took > 3*time.Second {
			t.Errorf("abandoning blocked for %v waiting on the child", took)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("abandoning a stream blocked; the subprocess is not being cancelled first")
	}
}
