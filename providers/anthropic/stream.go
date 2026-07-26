package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"

	"agentkit/pipeline"
)

// Client implements the optional streaming interface as well as ModelClient.
var _ pipeline.StreamingClient = (*Client)(nil)

// maxSSELine bounds a single server-sent-event line. Anthropic's deltas are
// small; a line beyond this means something is wrong with the stream, and
// growing a buffer without limit on a remote's say-so is how you get an OOM.
const maxSSELine = 1 << 20

// SendStream implements pipeline.StreamingClient over the Messages API's
// server-sent events.
//
// Errors before the first byte — a rejected request, a non-2xx status — are
// returned. Failures after streaming begins are yielded as the error half of a
// pair, because by then the caller may already have shown partial text and
// needs to know the rest is not coming.
//
// The HTTP response body is closed when the sequence finishes or the consumer
// abandons it. A consumer that neither drains nor breaks out of the loop keeps
// the connection open; the request context is the backstop.
func (c *Client) SendStream(ctx context.Context, req *pipeline.Request) (iter.Seq2[pipeline.Delta, error], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// No defer cancel() here: the request must outlive this function, since the
	// body is read lazily as the caller ranges. Cancellation is tied to the
	// sequence's own lifetime below.
	streamCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)

	payload := c.buildRequest(req)
	payload.Stream = true

	body, err := json.Marshal(payload)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("anthropic: build http request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", APIVersion)
	httpReq.Header.Set("x-api-key", c.cfg.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("anthropic: send: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		cancel()

		apiErr := &Error{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(raw)}
		var env apiErrorEnvelope
		if json.Unmarshal(raw, &env) == nil {
			apiErr.Type = env.Error.Type
			apiErr.Message = env.Error.Message
		}
		return nil, apiErr
	}

	return c.streamEvents(resp, cancel), nil
}

// streamEvents turns an SSE body into deltas. It owns closing the body and
// cancelling the request context, on every exit path including an abandoned
// consumer — that cleanup is why this is a named function rather than a closure
// inlined above.
func (c *Client) streamEvents(resp *http.Response, cancel context.CancelFunc) iter.Seq2[pipeline.Delta, error] {
	model := c.Name()

	return func(yield func(pipeline.Delta, error) bool) {
		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			cancel()
		}()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), maxSSELine)

		var (
			usage     pipeline.Usage
			haveUsage bool
			reported  string // model id the API names, preferred over c.Name()
		)
		nameOf := func() string {
			if reported != "" {
				return reported
			}
			return model
		}

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			// SSE frames are "event: x" then "data: {...}"; the event name adds
			// nothing the payload's own "type" does not carry, so only data
			// lines are parsed. Blank lines separate frames.
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}

			var ev streamEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				// One malformed frame is not worth discarding a good stream.
				continue
			}

			switch ev.Type {
			case "message_start":
				if ev.Message != nil {
					if ev.Message.Model != "" {
						reported = ev.Message.Model
					}
					mergeUsage(&usage, ev.Message.Usage)
					haveUsage = true
				}
			case "content_block_delta":
				if ev.Delta != nil && ev.Delta.Text != "" {
					if !yield(pipeline.Delta{Text: ev.Delta.Text, ModelUsed: nameOf()}, nil) {
						return
					}
				}
			case "message_delta":
				if ev.Usage != nil {
					mergeUsage(&usage, *ev.Usage)
					haveUsage = true
				}
			case "error":
				msg := "unknown error"
				if ev.Error != nil && ev.Error.Message != "" {
					msg = ev.Error.Message
				}
				yield(pipeline.Delta{ModelUsed: nameOf()},
					fmt.Errorf("anthropic: stream error: %s", msg))
				return
			}
		}

		if err := scanner.Err(); err != nil {
			yield(pipeline.Delta{ModelUsed: nameOf()}, fmt.Errorf("anthropic: read stream: %w", err))
			return
		}

		// A final, text-free delta carries the accounting. Emitting it
		// separately keeps Usage off every increment, where it would be
		// misleading — only this one is the provider's total.
		if haveUsage {
			u := usage
			yield(pipeline.Delta{ModelUsed: nameOf(), Usage: &u}, nil)
		}
	}
}

// mergeUsage folds a partial usage report into the running total. Anthropic
// splits it: input and cache figures arrive with message_start, output tokens
// with message_delta, so later frames must not zero earlier ones.
func mergeUsage(dst *pipeline.Usage, src apiUsage) {
	if src.InputTokens > 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > 0 {
		dst.OutputTokens = src.OutputTokens
	}
	if src.CacheReadInputTokens > 0 {
		dst.CacheReadTokens = src.CacheReadInputTokens
	}
	if src.CacheCreationInputTokens > 0 {
		dst.CacheCreationTokens = src.CacheCreationInputTokens
	}
}

// streamEvent is the subset of Anthropic's SSE payloads this adapter reads.
// Unknown types and fields are ignored, so new event types do not break it.
type streamEvent struct {
	Type string `json:"type"`

	Message *struct {
		Model string   `json:"model"`
		Usage apiUsage `json:"usage"`
	} `json:"message"`

	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`

	Usage *apiUsage `json:"usage"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
