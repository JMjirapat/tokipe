package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JMjirapat/tokipe/pipeline"
)

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{APIKey: "test-key-not-real", BaseURL: srv.URL, Model: "claude-opus-5"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func decodeBody(t *testing.T, r *http.Request) apiRequest {
	t.Helper()
	var req apiRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return req
}

const okResponse = `{
  "id": "msg_1",
  "model": "claude-opus-5",
  "stop_reason": "end_turn",
  "content": [{"type":"text","text":"hello "},{"type":"thinking","text":"ignored"},{"type":"text","text":"world"}],
  "usage": {
    "input_tokens": 100,
    "output_tokens": 20,
    "cache_read_input_tokens": 4096,
    "cache_creation_input_tokens": 512
  }
}`

func TestNameAndConfigDefaults(t *testing.T) {
	c, err := New(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.cfg.Model != DefaultModel || c.cfg.MaxTokens != DefaultMaxTokens ||
		c.cfg.BaseURL != DefaultBaseURL || c.cfg.Timeout != DefaultTimeout {
		t.Fatalf("defaults not applied: %+v", c.cfg)
	}
	if c.Name() != "anthropic/"+DefaultModel {
		t.Fatalf("Name = %q", c.Name())
	}
	if !strings.HasSuffix(c.url, MessagesPath) {
		t.Fatalf("url = %q", c.url)
	}

	named, _ := New(Config{APIKey: "k", ClientName: "primary"})
	if named.Name() != "primary" {
		t.Fatalf("ClientName override ignored: %q", named.Name())
	}

	if _, err := New(Config{}); err == nil {
		t.Fatal("New without APIKey succeeded")
	}
}

func TestSendHeadersAndBody(t *testing.T) {
	var (
		gotHeaders http.Header
		gotBody    apiRequest
		gotPath    string
	)
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotPath = r.URL.Path
		gotBody = decodeBody(t, r)
		_, _ = io.WriteString(w, okResponse)
	})

	req := &pipeline.Request{
		Query: "what changed?",
		Messages: []pipeline.Message{
			{Role: "system", Content: "You are a helpful assistant.", Static: true},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		},
	}
	resp, err := c.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotPath != MessagesPath {
		t.Fatalf("path = %q, want %q", gotPath, MessagesPath)
	}
	if got := gotHeaders.Get("anthropic-version"); got != APIVersion {
		t.Fatalf("anthropic-version = %q, want %q", got, APIVersion)
	}
	if got := gotHeaders.Get("x-api-key"); got == "" {
		t.Fatal("x-api-key header missing")
	}
	if got := gotHeaders.Get("content-type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}

	if gotBody.Model != "claude-opus-5" || gotBody.MaxTokens != DefaultMaxTokens {
		t.Fatalf("model/max_tokens = %q/%d", gotBody.Model, gotBody.MaxTokens)
	}
	if len(gotBody.System) != 1 || gotBody.System[0].Text != "You are a helpful assistant." {
		t.Fatalf("system not hoisted: %+v", gotBody.System)
	}
	// user, assistant, then the appended query.
	if len(gotBody.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(gotBody.Messages), gotBody.Messages)
	}
	if gotBody.Messages[0].Role != "user" || gotBody.Messages[1].Role != "assistant" {
		t.Fatalf("roles = %v/%v", gotBody.Messages[0].Role, gotBody.Messages[1].Role)
	}
	last := gotBody.Messages[2]
	if last.Role != "user" || last.Content[0].Text != "what changed?" {
		t.Fatalf("query not appended last: %+v", last)
	}

	if resp.Content != "hello world" {
		t.Fatalf("Content = %q, want %q", resp.Content, "hello world")
	}
	if resp.ModelUsed != "claude-opus-5" {
		t.Fatalf("ModelUsed = %q", resp.ModelUsed)
	}
	want := pipeline.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 4096, CacheCreationTokens: 512}
	if resp.Usage != want {
		t.Fatalf("Usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestQueryNotDuplicatedWhenAlreadyLast(t *testing.T) {
	var got apiRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = decodeBody(t, r)
		_, _ = io.WriteString(w, okResponse)
	})
	req := &pipeline.Request{
		Query:    "same text",
		Messages: []pipeline.Message{{Role: "user", Content: "same text"}},
	}
	if _, err := c.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (no duplicate query): %+v", len(got.Messages), got.Messages)
	}
}

func TestEmptyRequestStillSendsOneMessage(t *testing.T) {
	var got apiRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = decodeBody(t, r)
		_, _ = io.WriteString(w, okResponse)
	})
	if _, err := c.Send(context.Background(), &pipeline.Request{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(got.Messages))
	}
}

func TestCacheBreakpointsBecomeCacheControl(t *testing.T) {
	var got apiRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = decodeBody(t, r)
		_, _ = io.WriteString(w, okResponse)
	})

	req := &pipeline.Request{
		Query: "next",
		Messages: []pipeline.Message{
			{Role: "system", Content: "sys", Static: true},
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: "a1"},
		},
		CacheBreakpoints: []pipeline.CacheBreakpoint{
			{AfterMessageIndex: 0, Reason: "static"},
			{AfterMessageIndex: 2, Reason: "history"},
		},
	}
	if _, err := c.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got.System[0].CacheControl == nil || got.System[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("system block missing cache_control: %+v", got.System[0])
	}
	// Messages index 2 == assistant "a1" == conversation slot 1.
	a1 := got.Messages[1]
	if a1.Content[0].CacheControl == nil {
		t.Fatalf("assistant block missing cache_control: %+v", a1)
	}
	// The appended query must never be cached (dynamic).
	q := got.Messages[len(got.Messages)-1]
	if q.Content[0].CacheControl != nil {
		t.Fatalf("newest turn must not carry cache_control: %+v", q)
	}
}

func TestBreakpointTruncationKeepsDeepestFour(t *testing.T) {
	bps := []pipeline.CacheBreakpoint{
		{AfterMessageIndex: 0},
		{AfterMessageIndex: 1},
		{AfterMessageIndex: 2},
		{AfterMessageIndex: 3},
		{AfterMessageIndex: 4},
		{AfterMessageIndex: 5},
	}
	got := selectBreakpoints(bps, MaxCacheBreakpts)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	for i, want := range []int{2, 3, 4, 5} {
		if got[i].AfterMessageIndex != want {
			t.Fatalf("kept %v, want the deepest four", got)
		}
	}

	// Duplicates on the same index collapse.
	dup := selectBreakpoints([]pipeline.CacheBreakpoint{{AfterMessageIndex: 1}, {AfterMessageIndex: 1}}, 4)
	if len(dup) != 1 {
		t.Fatalf("duplicates not collapsed: %v", dup)
	}

	if selectBreakpoints(nil, 4) != nil || selectBreakpoints(bps, 0) != nil {
		t.Fatal("empty inputs should yield nil")
	}
}

func TestAtMostFourCacheControlBlocksOnTheWire(t *testing.T) {
	var got apiRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = decodeBody(t, r)
		_, _ = io.WriteString(w, okResponse)
	})

	req := &pipeline.Request{Query: "q"}
	for i := 0; i < 8; i++ {
		req.Messages = append(req.Messages, pipeline.Message{Role: "user", Content: "m"})
		req.CacheBreakpoints = append(req.CacheBreakpoints, pipeline.CacheBreakpoint{AfterMessageIndex: i})
	}
	if _, err := c.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}

	count := 0
	for _, b := range got.System {
		if b.CacheControl != nil {
			count++
		}
	}
	for _, m := range got.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				count++
			}
		}
	}
	if count != MaxCacheBreakpts {
		t.Fatalf("cache_control blocks = %d, want %d", count, MaxCacheBreakpts)
	}
}

func TestOutOfRangeBreakpointIgnored(t *testing.T) {
	var got apiRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = decodeBody(t, r)
		_, _ = io.WriteString(w, okResponse)
	})
	req := &pipeline.Request{
		Query:            "q",
		Messages:         []pipeline.Message{{Role: "user", Content: "u"}},
		CacheBreakpoints: []pipeline.CacheBreakpoint{{AfterMessageIndex: 99}, {AfterMessageIndex: -1}},
	}
	if _, err := c.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, m := range got.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				t.Fatalf("out-of-range breakpoint applied: %+v", m)
			}
		}
	}
}

func TestRetrievedChunksAppendedAsContext(t *testing.T) {
	var got apiRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = decodeBody(t, r)
		_, _ = io.WriteString(w, okResponse)
	})
	req := &pipeline.Request{
		Query:    "explain",
		Messages: []pipeline.Message{{Role: "user", Content: "earlier"}},
		RetrievedChunks: []pipeline.Chunk{
			{Content: "chunk A", SourceURL: "https://a"},
			{Content: "chunk B"},
		},
	}
	if _, err := c.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// earlier, chunks, query
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(got.Messages), got.Messages)
	}
	chunks := got.Messages[1]
	if chunks.Role != "user" || len(chunks.Content) != 3 {
		t.Fatalf("chunk message shape = %+v", chunks)
	}
	if !strings.Contains(chunks.Content[1].Text, "https://a") ||
		!strings.Contains(chunks.Content[1].Text, "chunk A") {
		t.Fatalf("chunk not formatted with source: %q", chunks.Content[1].Text)
	}
	if chunks.Content[2].Text != "chunk B" {
		t.Fatalf("sourceless chunk = %q", chunks.Content[2].Text)
	}
	for _, b := range chunks.Content {
		if b.CacheControl != nil {
			t.Fatal("retrieved chunks are dynamic and must never carry cache_control")
		}
	}
}

func TestUnknownRolesNormalizeToUser(t *testing.T) {
	var got apiRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = decodeBody(t, r)
		_, _ = io.WriteString(w, okResponse)
	})
	req := &pipeline.Request{Messages: []pipeline.Message{
		{Role: "TOOL", Content: "x"},
		{Role: "SYSTEM", Content: "s"},
		{Role: "Assistant", Content: "a"},
	}}
	if _, err := c.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(got.System) != 1 {
		t.Fatalf("case-insensitive system hoisting failed: %+v", got.System)
	}
	if got.Messages[0].Role != "user" || got.Messages[1].Role != "assistant" {
		t.Fatalf("roles = %+v", got.Messages)
	}
}

func TestTemperaturePassthrough(t *testing.T) {
	var got apiRequest
	temp := 0.25
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = decodeBody(t, r)
		_, _ = io.WriteString(w, okResponse)
	}))
	defer srv.Close()

	c, err := New(Config{APIKey: "k", BaseURL: srv.URL, Temperature: &temp})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Send(context.Background(), &pipeline.Request{Query: "q"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.Temperature == nil || *got.Temperature != temp {
		t.Fatalf("Temperature = %v, want %v", got.Temperature, temp)
	}
}

func TestErrorStatusIsTyped(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	})
	_, err := c.Send(context.Background(), &pipeline.Request{Query: "q"})
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *anthropic.Error", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d", apiErr.StatusCode)
	}
	if apiErr.Type != "rate_limit_error" || apiErr.Message != "slow down" {
		t.Fatalf("parsed error = %+v", apiErr)
	}
	if !apiErr.Retryable() {
		t.Fatal("429 should be Retryable")
	}
	if !strings.Contains(apiErr.Error(), "429") {
		t.Fatalf("Error() = %q, want the status in it", apiErr.Error())
	}

	c2, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "not json")
	})
	_, err = c2.Send(context.Background(), &pipeline.Request{Query: "q"})
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("err = %v", err)
	}
	if apiErr.Retryable() {
		t.Fatal("401 should not be Retryable")
	}
	if apiErr.Body != "not json" {
		t.Fatalf("Body = %q", apiErr.Body)
	}
}

func TestAPIKeyNeverLeaksIntoNameOrError(t *testing.T) {
	const key = "sk-ant-notarealkey-000"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
	}))
	defer srv.Close()

	c, _ := New(Config{APIKey: key, BaseURL: srv.URL})
	_, err := c.Send(context.Background(), &pipeline.Request{Query: "q"})
	if strings.Contains(err.Error(), key) || strings.Contains(c.Name(), key) {
		t.Fatal("API key leaked into an error or Name()")
	}
}

func TestMalformedSuccessBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{not json")
	})
	if _, err := c.Send(context.Background(), &pipeline.Request{Query: "q"}); err == nil {
		t.Fatal("malformed body accepted")
	}
}

func TestResponseModelFallback(t *testing.T) {
	resp, err := parseResponse([]byte(`{"content":[{"type":"text","text":"hi"}]}`), "fallback-name")
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if resp.ModelUsed != "fallback-name" {
		t.Fatalf("ModelUsed = %q", resp.ModelUsed)
	}
	if resp.Content != "hi" {
		t.Fatalf("Content = %q", resp.Content)
	}
}

// blockingHandler stalls until release is called. It must never wait on
// r.Context() alone: httptest.Server.Close waits for in-flight handlers, and
// the server does not reliably observe a client-side cancellation before the
// test tears it down — that deadlocks Close (it hung for the full test
// timeout). The returned release func is what guarantees the handler returns,
// so callers must arrange for it to run BEFORE the server is closed.
func blockingHandler() (http.HandlerFunc, func()) {
	release := make(chan struct{})
	h := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	return h, func() { close(release) }
}

func TestContextCancellation(t *testing.T) {
	h, release := blockingHandler()
	// Deferred calls run before t.Cleanup(srv.Close) registered by newTestClient.
	defer release()

	c, _ := newTestClient(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Send(ctx, &pipeline.Request{Query: "q"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestTimeoutIsApplied(t *testing.T) {
	h, release := blockingHandler()
	srv := httptest.NewServer(h)
	defer srv.Close() // LIFO: runs after release below
	defer release()

	c, err := New(Config{APIKey: "k", BaseURL: srv.URL, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	if _, err := c.Send(context.Background(), &pipeline.Request{Query: "q"}); err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout not honoured: took %v", elapsed)
	}
}

func TestImplementsModelClient(t *testing.T) {
	c, _ := New(Config{APIKey: "k"})
	var _ pipeline.ModelClient = c
	p := pipeline.New(c)
	_ = p // compile-time check that it slots into a Pipeline
}
