package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JMjirapat/tokipe/pipeline"
)

const okResponse = `{
  "model": "gpt-4o-mini-2024-07-18",
  "choices": [{"message": {"role": "assistant", "content": "hello world"}, "finish_reason": "stop"}],
  "usage": {"prompt_tokens": 100, "completion_tokens": 5, "prompt_tokens_details": {"cached_tokens": 64}}
}`

func serve(t *testing.T, h http.HandlerFunc) (*Client, *[]byte, *http.Header) {
	t.Helper()
	var body []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
		h(w, r)
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{APIKey: "test-key", BaseURL: srv.URL, Model: "gpt-4o-mini", Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, &body, &hdr
}

func TestSendParsesContentAndUsage(t *testing.T) {
	c, _, hdr := serve(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, okResponse) })

	resp, err := c.Send(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Content != "hello world" {
		t.Errorf("Content = %q", resp.Content)
	}
	// The server names the exact model; prefer it over the configured alias.
	if resp.ModelUsed != "gpt-4o-mini-2024-07-18" {
		t.Errorf("ModelUsed = %q", resp.ModelUsed)
	}
	want := pipeline.Usage{InputTokens: 100, OutputTokens: 5, CacheReadTokens: 64}
	if resp.Usage != want {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, want)
	}
	if got := hdr.Get("authorization"); got != "Bearer test-key" {
		t.Errorf("authorization = %q", got)
	}
}

// Local compatible servers (Ollama, llama.cpp) take no key. Requiring one would
// make this adapter useless for exactly the case it was added to serve.
func TestNoAPIKeyIsAllowedAndSendsNoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "" {
			t.Errorf("an authorization header was sent without a key: %q", r.Header.Get("authorization"))
		}
		fmt.Fprint(w, okResponse)
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New must not require an API key: %v", err)
	}
	if _, err := c.Send(context.Background(), &pipeline.Request{Query: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestExtraHeadersAreSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("HTTP-Referer") != "https://example.com" {
			t.Errorf("extra header missing: %v", r.Header)
		}
		fmt.Fprint(w, okResponse)
	}))
	t.Cleanup(srv.Close)

	c, _ := New(Config{BaseURL: srv.URL, ExtraHeaders: map[string]string{"HTTP-Referer": "https://example.com"}})
	if _, err := c.Send(context.Background(), &pipeline.Request{Query: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// The same layout rule as the Anthropic adapter: system, history, retrieved
// context, then the newest turn exactly once.
func TestWireOrderPutsContextBeforeTheNewestTurn(t *testing.T) {
	c, body, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, okResponse) })

	_, err := c.Send(context.Background(), &pipeline.Request{
		Query: "current question",
		Messages: []pipeline.Message{
			{Role: "system", Content: "stable", Static: true},
			{Role: "user", Content: "earlier turn"},
			{Role: "user", Content: "current question"},
		},
		RetrievedChunks: []pipeline.Chunk{{Content: "retrieved evidence", SourceURL: "docs/a"}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var sent apiRequest
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var order []string
	for _, m := range sent.Messages {
		order = append(order, m.Content)
	}
	joined := strings.Join(order, "|")

	if sent.Messages[0].Role != "system" {
		t.Errorf("system must lead: %v", order)
	}
	evidence := indexOf(order, func(s string) bool { return strings.Contains(s, "retrieved evidence") })
	question := indexOf(order, func(s string) bool { return s == "current question" })
	if evidence < 0 || question < 0 {
		t.Fatalf("missing content: %v", order)
	}
	if evidence > question {
		t.Errorf("evidence follows the question: %v", order)
	}
	if strings.Count(joined, "current question") != 1 {
		t.Errorf("the question appears more than once: %v", order)
	}
}

func TestEmptyQueryStillOrdersCorrectly(t *testing.T) {
	c, body, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, okResponse) })

	_, err := c.Send(context.Background(), &pipeline.Request{
		Messages: []pipeline.Message{
			{Role: "system", Content: "stable"},
			{Role: "user", Content: "current question"},
		},
		RetrievedChunks: []pipeline.Chunk{{Content: "retrieved evidence"}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var sent apiRequest
	_ = json.Unmarshal(*body, &sent)
	var order []string
	for _, m := range sent.Messages {
		order = append(order, m.Content)
	}
	evidence := indexOf(order, func(s string) bool { return strings.Contains(s, "retrieved evidence") })
	question := indexOf(order, func(s string) bool { return s == "current question" })
	if evidence > question {
		t.Errorf("with an empty Query, evidence still follows the question: %v", order)
	}
}

func TestUnknownRolesBecomeUser(t *testing.T) {
	c, body, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, okResponse) })
	_, _ = c.Send(context.Background(), &pipeline.Request{
		Messages: []pipeline.Message{{Role: "tool", Content: "a tool result"}},
	})

	var sent apiRequest
	_ = json.Unmarshal(*body, &sent)
	for _, m := range sent.Messages {
		switch m.Role {
		case "user", "assistant", "system":
		default:
			t.Errorf("unexpected role %q reached the wire", m.Role)
		}
	}
}

// Cache breakpoints are advisory here. They must be silently ignored rather
// than mistranslated into a field this API does not have.
func TestCacheBreakpointsAreNotTransmitted(t *testing.T) {
	c, body, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, okResponse) })
	_, _ = c.Send(context.Background(), &pipeline.Request{
		Query:            "q",
		Messages:         []pipeline.Message{{Role: "system", Content: "s", Static: true}},
		CacheBreakpoints: []pipeline.CacheBreakpoint{{AfterMessageIndex: 0, Reason: "static_prefix"}},
	})
	if strings.Contains(string(*body), "cache_control") || strings.Contains(string(*body), "breakpoint") {
		t.Errorf("breakpoints leaked onto the wire:\n%s", *body)
	}
}

func TestNonOKStatusIsTyped(t *testing.T) {
	c, _, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`)
	})

	_, err := c.Send(context.Background(), &pipeline.Request{Query: "q"})
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *openai.Error", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Type != "rate_limit_exceeded" {
		t.Errorf("err = %+v", apiErr)
	}
	if !apiErr.Retryable() {
		t.Error("429 should be retryable")
	}
}

func TestEmptyChoicesIsAnError(t *testing.T) {
	c, _, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"model":"m","choices":[]}`)
	})
	if _, err := c.Send(context.Background(), &pipeline.Request{Query: "q"}); err == nil {
		t.Error("a response with no choices must be an error, not empty content")
	}
}

func TestMalformedBodyIsAnError(t *testing.T) {
	c, _, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "not json") })
	if _, err := c.Send(context.Background(), &pipeline.Request{Query: "q"}); err == nil {
		t.Error("want an error for a malformed body")
	}
}

func TestAPIKeyNeverLeaksIntoNameOrError(t *testing.T) {
	const key = "sk-notarealkey-000"
	c, _, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	})
	c.cfg.APIKey = key

	if strings.Contains(c.Name(), key) {
		t.Error("Name leaked the API key")
	}
	_, err := c.Send(context.Background(), &pipeline.Request{Query: "q"})
	if err != nil && strings.Contains(err.Error(), key) {
		t.Errorf("the error leaked the API key: %v", err)
	}
}

func TestCanceledContextFailsBeforeSending(t *testing.T) {
	var calls int
	c, _, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, okResponse)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Send(ctx, &pipeline.Request{Query: "q"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("made %d requests on a canceled context", calls)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.cfg.BaseURL != DefaultBaseURL || c.cfg.Model != DefaultModel {
		t.Errorf("defaults not applied: %+v", c.cfg)
	}
	if !strings.HasSuffix(c.url, ChatPath) {
		t.Errorf("url = %q", c.url)
	}
	if c.Name() != "openai/"+DefaultModel {
		t.Errorf("Name = %q", c.Name())
	}
}

func TestImplementsModelClient(t *testing.T) {
	c, _ := New(Config{})
	var _ pipeline.ModelClient = c
	_ = pipeline.New(c)
}

func indexOf(ss []string, pred func(string) bool) int {
	for i, s := range ss {
		if pred(s) {
			return i
		}
	}
	return -1
}
