package anthropic

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

// sseServer serves the given frames as text/event-stream, flushing each so the
// client genuinely reads them incrementally rather than all at once.
func sseServer(t *testing.T, frames ...string) (*Client, *httptest.Server, *[]byte) {
	t.Helper()

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = body

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

	c, err := New(Config{APIKey: "test-key", BaseURL: srv.URL, Model: "claude-opus-5", Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv, &captured
}

func frame(payload string) string { return "event: x\ndata: " + payload + "\n\n" }

func happyFrames() []string {
	return []string{
		frame(`{"type":"message_start","message":{"model":"claude-opus-5-20990101","usage":{"input_tokens":100,"cache_read_input_tokens":4096,"cache_creation_input_tokens":512}}}`),
		frame(`{"type":"content_block_start","index":0}`),
		frame(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
		frame(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", "}}`),
		frame(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`),
		frame(`{"type":"content_block_stop","index":0}`),
		frame(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`),
		frame(`{"type":"message_stop"}`),
	}
}

func TestSendStreamYieldsTextDeltasAndFinalUsage(t *testing.T) {
	c, _, captured := sseServer(t, happyFrames()...)

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
		t.Errorf("deltas = %q, want the three text increments only", got)
	}
	if usage == nil {
		t.Fatal("no usage delta was emitted")
	}
	// Input/cache figures come with message_start, output with message_delta:
	// a later frame must not zero an earlier one.
	want := pipeline.Usage{InputTokens: 100, OutputTokens: 7, CacheReadTokens: 4096, CacheCreationTokens: 512}
	if *usage != want {
		t.Errorf("usage = %+v, want %+v", *usage, want)
	}
	// The API names the exact model; prefer it over our configured alias.
	if model != "claude-opus-5-20990101" {
		t.Errorf("ModelUsed = %q, want the model the API reported", model)
	}

	var sent apiRequest
	if err := json.Unmarshal(*captured, &sent); err != nil {
		t.Fatalf("decode captured request: %v", err)
	}
	if !sent.Stream {
		t.Error(`the streaming request must set "stream": true`)
	}
}

// A non-streaming Send must not gain a stream flag — the request body is part
// of what the provider hashes for its prompt cache.
func TestNonStreamingRequestOmitsStreamFlag(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, okResponse)
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Send(context.Background(), &pipeline.Request{Query: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(string(captured), `"stream"`) {
		t.Errorf("non-streaming body must not mention stream:\n%s", captured)
	}
}

func TestSendStreamNonOKStatusFailsUpFront(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "hi"})
	if seq != nil {
		t.Error("no sequence should be returned for a non-2xx status")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *anthropic.Error", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Type != "rate_limit_error" {
		t.Errorf("err = %+v, want the typed rate-limit error", apiErr)
	}
	if !apiErr.Retryable() {
		t.Error("429 should be retryable")
	}
}

// An error frame mid-stream is yielded, so text already delivered survives.
func TestStreamErrorFrameIsYieldedAfterPartialText(t *testing.T) {
	c, _, _ := sseServer(t,
		frame(`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":5}}}`),
		frame(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}`),
		frame(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`),
	)

	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("SendStream should not fail up front: %v", err)
	}

	resp, err := pipeline.Collect(seq)
	if err == nil {
		t.Fatal("the error frame must surface")
	}
	if !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("err = %v, want the provider's message", err)
	}
	if resp.Content != "partial" {
		t.Errorf("Content = %q, want the text delivered before the error", resp.Content)
	}
}

// A stream is a remote's output; malformed and unknown frames must not derail it.
func TestStreamToleratesJunkAndUnknownEvents(t *testing.T) {
	c, _, _ := sseServer(t,
		": this is an SSE comment\n\n",
		"event: ping\n\n",
		frame(`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":1}}}`),
		frame(`not json at all`),
		frame(`{"type":"some_future_event","payload":{"deeply":{"nested":true}}}`),
		frame(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}`),
		"data: [DONE]\n\n",
		frame(`{"type":"message_stop"}`),
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

// Empty text deltas carry nothing and must not become deltas of their own.
func TestEmptyTextDeltasAreSkipped(t *testing.T) {
	c, _, _ := sseServer(t,
		frame(`{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}`),
		frame(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}`),
		frame(`{"type":"message_stop"}`),
	)

	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	count := 0
	for d, err := range seq {
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if d.Text != "" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("got %d text deltas, want 1", count)
	}
}

func TestSendStreamCanceledContextFailsUpFront(t *testing.T) {
	c, _, _ := sseServer(t, happyFrames()...)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SendStream(ctx, &pipeline.Request{Query: "hi"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Abandoning the loop must release the body rather than wedge the connection.
func TestAbandonedStreamIsCleanedUp(t *testing.T) {
	c, _, _ := sseServer(t, happyFrames()...)

	seq, err := c.SendStream(context.Background(), &pipeline.Request{Query: "hi"})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	for range seq {
		break // walk away after one delta
	}

	// If cleanup were broken this would hang or race; a second full stream on
	// the same client proves the transport is still usable.
	seq2, err := c.SendStream(context.Background(), &pipeline.Request{Query: "again"})
	if err != nil {
		t.Fatalf("second SendStream: %v", err)
	}
	if resp, err := pipeline.Collect(seq2); err != nil || resp.Content != "Hello, world" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestClientSatisfiesStreamingClient(t *testing.T) {
	c, err := New(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var _ pipeline.StreamingClient = c

	// And it must work through the pipeline's streaming entry point.
	if _, err := pipeline.New(c).RunStream(context.Background(), &pipeline.Request{}); err == nil {
		t.Log("RunStream accepted the client (a real call would need the network)")
	}
}
