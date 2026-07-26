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

// sseServer flushes each frame so the client genuinely reads incrementally.
func sseServer(t *testing.T, frames ...string) (*Client, *[]byte) {
	t.Helper()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprint(w, f)
			if ok {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{APIKey: "k", BaseURL: srv.URL, Model: "gpt-4o-mini", Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, &captured
}

func frame(payload string) string { return "data: " + payload + "\n\n" }

func happyFrames() []string {
	return []string{
		frame(`{"model":"gpt-4o-mini-2024-07-18","choices":[{"delta":{"content":"Hello"}}]}`),
		frame(`{"model":"gpt-4o-mini-2024-07-18","choices":[{"delta":{"content":", "}}]}`),
		frame(`{"model":"gpt-4o-mini-2024-07-18","choices":[{"delta":{"content":"world"}}]}`),
		frame(`{"model":"gpt-4o-mini-2024-07-18","choices":[{"delta":{},"finish_reason":"stop"}]}`),
		frame(`{"model":"gpt-4o-mini-2024-07-18","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":32}}}`),
		"data: [DONE]\n\n",
	}
}

func TestSendStreamYieldsDeltasAndFinalUsage(t *testing.T) {
	c, captured := sseServer(t, happyFrames()...)

	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}

	var texts []string
	var usage *pipeline.Usage
	var model string
	for d, err := range seq {
		if err != nil {
			t.Fatalf("mid-stream error: %v", err)
		}
		if d.Text != "" {
			texts = append(texts, d.Text)
		}
		if d.Usage != nil {
			usage = d.Usage
		}
		if d.ModelUsed != "" {
			model = d.ModelUsed
		}
	}

	if got := strings.Join(texts, "|"); got != "Hello|, |world" {
		t.Errorf("deltas = %q, want three text increments only", got)
	}
	if usage == nil {
		t.Fatal("no usage delta was emitted")
	}
	want := pipeline.Usage{InputTokens: 50, OutputTokens: 3, CacheReadTokens: 32}
	if *usage != want {
		t.Errorf("usage = %+v, want %+v", *usage, want)
	}
	if model != "gpt-4o-mini-2024-07-18" {
		t.Errorf("ModelUsed = %q, want the model the server reported", model)
	}

	// stream_options asks for usage; without it streaming would be a blind spot
	// for accounting on servers that honour it.
	var sent apiRequest
	if err := json.Unmarshal(*captured, &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !sent.Stream {
		t.Error(`the streaming request must set "stream": true`)
	}
	if sent.StreamOptions == nil || !sent.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage should be requested")
	}
}

// Many compatible servers ignore stream_options. Absent usage must stay absent
// rather than being reported as zeros.
func TestNoUsageFrameMeansNoUsageDelta(t *testing.T) {
	c, _ := sseServer(t,
		frame(`{"choices":[{"delta":{"content":"ok"}}]}`),
		"data: [DONE]\n\n",
	)

	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	for d, err := range seq {
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if d.Usage != nil {
			t.Errorf("a usage delta was invented where the server reported none: %+v", *d.Usage)
		}
	}
}

func TestNonStreamingRequestOmitsStreamFields(t *testing.T) {
	c, captured := sseServer(t, frame(`{"choices":[{"delta":{"content":"x"}}]}`))
	// Send, not SendStream.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = &body
		fmt.Fprint(w, okResponse)
	}))
	t.Cleanup(srv.Close)
	c.url = srv.URL + ChatPath

	if _, err := c.Send(context.Background(), &pipeline.Request{Query: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(string(*captured), `"stream"`) {
		t.Errorf("a non-streaming body must not mention stream:\n%s", *captured)
	}
}

func TestStreamErrorFrameIsYieldedAfterPartialText(t *testing.T) {
	c, _ := sseServer(t,
		frame(`{"choices":[{"delta":{"content":"partial"}}]}`),
		frame(`{"error":{"type":"server_error","message":"upstream exploded"}}`),
	)

	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("SendStream should not fail up front: %v", err)
	}
	resp, err := pipeline.Collect(seq)
	if err == nil {
		t.Fatal("the error frame must surface")
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Errorf("err = %v", err)
	}
	if resp.Content != "partial" {
		t.Errorf("Content = %q, want the text delivered before the error", resp.Content)
	}
}

func TestStreamToleratesJunkAndUnknownFrames(t *testing.T) {
	c, _ := sseServer(t,
		": an SSE comment\n\n",
		"event: ping\n\n",
		frame(`not json at all`),
		frame(`{"choices":[{"delta":{}}]}`),
		frame(`{"some_future_field":{"nested":true},"choices":[]}`),
		frame(`{"choices":[{"delta":{"content":"ok"}}]}`),
		"data: [DONE]\n\n",
	)

	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	resp, err := pipeline.Collect(seq)
	if err != nil {
		t.Fatalf("junk frames must not fail the stream: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q, want the one good delta", resp.Content)
	}
}

func TestSendStreamNonOKStatusFailsUpFront(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"type":"server_error","message":"down"}}`)
	}))
	t.Cleanup(srv.Close)

	c, _ := New(Config{BaseURL: srv.URL})
	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "hi"})
	if seq != nil {
		t.Error("no sequence should be returned for a non-2xx status")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || !apiErr.Retryable() {
		t.Fatalf("err = %v, want a retryable *openai.Error", err)
	}
}

func TestSendStreamCanceledContextFailsUpFront(t *testing.T) {
	c, _ := sseServer(t, happyFrames()...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SendStream(ctx, &pipeline.Request{Query: "hi"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestAbandonedStreamIsCleanedUp(t *testing.T) {
	c, _ := sseServer(t, happyFrames()...)

	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	for range seq {
		break // walk away after one delta
	}

	// A second full stream proves the transport is still usable.
	seq2, err := c.SendStream(context.Background(), &pipeline.Request{Query: "again"})
	if err != nil {
		t.Fatalf("second SendStream: %v", err)
	}
	if resp, err := pipeline.Collect(seq2); err != nil || resp.Content != "Hello, world" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestSatisfiesStreamingClientAndWorksThroughRunStream(t *testing.T) {
	c, _ := sseServer(t, happyFrames()...)
	var _ pipeline.StreamingClient = c

	seq, err := pipeline.New(c).RunStream(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	resp, err := pipeline.Collect(seq)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.Content != "Hello, world" {
		t.Errorf("Content = %q", resp.Content)
	}
}
