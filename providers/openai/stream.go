package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"

	"github.com/JMjirapat/tokipe/pipeline"
)

var _ pipeline.StreamingClient = (*Client)(nil)

// maxSSELine bounds one server-sent-event line. Deltas are small; a line beyond
// this means something is wrong, and growing a buffer on a remote's say-so is
// how a client gets OOM-killed.
const maxSSELine = 1 << 20

// SendStream implements pipeline.StreamingClient over the chat-completions SSE
// stream.
//
// Errors before the first byte are returned; failures after streaming begins
// are yielded, because by then the caller may already have shown partial text.
//
// Usage arrives only if the server honours stream_options.include_usage, which
// OpenAI does and many compatible servers do not. When it does not, the final
// delta carries no Usage — absent data rather than zeros.
func (c *Client) SendStream(ctx context.Context, req *pipeline.Request) (iter.Seq2[pipeline.Delta, error], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Not deferred: the body is read lazily as the caller ranges, so the
	// request must outlive this function. The sequence owns cleanup.
	streamCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)

	payload := c.buildRequest(req)
	payload.Stream = true
	payload.StreamOptions = &streamOptions{IncludeUsage: true}

	httpReq, err := c.newRequest(streamCtx, payload)
	if err != nil {
		cancel()
		return nil, err
	}
	httpReq.Header.Set("accept", "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("openai: send: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		cancel()
		return nil, errorFor(resp, raw)
	}

	return c.streamEvents(resp, cancel), nil
}

// streamEvents turns an SSE body into deltas, owning the body and the request
// context on every exit path — including an abandoned consumer.
func (c *Client) streamEvents(resp *http.Response, cancel context.CancelFunc) iter.Seq2[pipeline.Delta, error] {
	fallbackModel := c.Name()

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
			model     string
		)
		nameOf := func() string {
			if model != "" {
				return model
			}
			return fallbackModel
		}

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue // comments, blank separators, event: lines
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			// "[DONE]" terminates an OpenAI stream and is not JSON.
			if data == "[DONE]" {
				break
			}

			var ev streamChunk
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue // one malformed frame is not worth discarding a good stream
			}
			if ev.Model != "" {
				model = ev.Model
			}
			if ev.Error != nil && ev.Error.Message != "" {
				yield(pipeline.Delta{ModelUsed: nameOf()},
					fmt.Errorf("openai: stream error: %s", ev.Error.Message))
				return
			}
			if ev.Usage != nil {
				usage.InputTokens = ev.Usage.PromptTokens
				usage.OutputTokens = ev.Usage.CompletionTokens
				usage.CacheReadTokens = ev.Usage.PromptTokensDetails.CachedTokens
				haveUsage = true
			}

			for _, ch := range ev.Choices {
				if ch.Delta.Content == "" {
					continue
				}
				if !yield(pipeline.Delta{Text: ch.Delta.Content, ModelUsed: nameOf()}, nil) {
					return
				}
			}
		}

		if err := scanner.Err(); err != nil {
			yield(pipeline.Delta{ModelUsed: nameOf()}, fmt.Errorf("openai: read stream: %w", err))
			return
		}

		// A final, text-free delta carries the accounting — emitted only when
		// the server actually reported it.
		if haveUsage {
			u := usage
			yield(pipeline.Delta{ModelUsed: nameOf(), Usage: &u}, nil)
		}
	}
}

// streamChunk is the subset of a chat-completions SSE frame this adapter reads.
// Unknown fields are ignored, so vendor extensions do not break it.
type streamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *apiUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
