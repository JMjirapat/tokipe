package cli_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"agentkit/pipeline"
	"agentkit/providers/cli"
	"agentkit/toolcache"
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
		fmt.Println(`{"type":"item.started","text":""}`)
		fmt.Println(`not json at all`)
		fmt.Println(`{"type":"item.completed","text":"first"}`)
		fmt.Println(`{"type":"item.completed","text":"final answer","usage":{"input_tokens":10,"output_tokens":3,"cached_input_tokens":7}}`)

	case "fail":
		fmt.Fprintln(os.Stderr, "something broke")
		os.Exit(3)

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
