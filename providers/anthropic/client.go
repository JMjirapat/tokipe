// Package anthropic implements pipeline.ModelClient against the Anthropic
// Messages API using nothing but net/http and encoding/json.
//
// It is a separate Go module (see go.mod in this directory) purely so the
// agentkit core never grows a dependency on it; the module itself has zero
// third-party dependencies.
//
// See docs/spec.md §3 Phase 2.1.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agentkit/pipeline"
)

// API constants.
const (
	DefaultBaseURL    = "https://api.anthropic.com"
	MessagesPath      = "/v1/messages"
	APIVersion        = "2023-06-01"
	DefaultModel      = "claude-opus-5"
	DefaultMaxTokens  = 4096
	DefaultTimeout    = 60 * time.Second
	MaxCacheBreakpts  = 4 // Anthropic accepts at most 4 cache_control blocks.
	cacheControlType  = "ephemeral"
	retrievedRoleUser = "user"
)

// Error is the typed error returned for a non-2xx response. It carries the
// HTTP status so callers can distinguish 401 from 429 from 529.
type Error struct {
	StatusCode int
	Status     string
	Type       string // Anthropic error.type, when parseable
	Message    string
	Body       string
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Body
	}
	if len(msg) > 512 {
		msg = msg[:512] + "…"
	}
	return fmt.Sprintf("anthropic: http %d %s: %s", e.StatusCode, e.Status, msg)
}

// Retryable reports whether the status suggests the request may succeed later.
func (e *Error) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// Config configures a Client.
type Config struct {
	// APIKey is sent as the x-api-key header. Required. Never logged.
	APIKey string

	// Model defaults to DefaultModel.
	Model string

	// MaxTokens defaults to DefaultMaxTokens.
	MaxTokens int

	// BaseURL defaults to DefaultBaseURL. Override for tests or a proxy.
	BaseURL string

	// Timeout bounds a single Send. Defaults to DefaultTimeout.
	Timeout time.Duration

	// HTTPClient defaults to a client with Timeout.
	HTTPClient *http.Client

	// Temperature, if non-nil, is sent verbatim.
	Temperature *float64

	// ClientName overrides the value returned by Name().
	ClientName string
}

// Client is a pipeline.ModelClient talking to the Anthropic Messages API.
type Client struct {
	cfg  Config
	http *http.Client
	url  string
}

var _ pipeline.ModelClient = (*Client)(nil)

// New builds a Client. It returns an error only for a missing API key.
func New(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("anthropic: APIKey is required")
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = DefaultMaxTokens
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{
		cfg:  cfg,
		http: hc,
		url:  strings.TrimRight(cfg.BaseURL, "/") + MessagesPath,
	}, nil
}

// Name implements pipeline.ModelClient. It never includes the API key.
func (c *Client) Name() string {
	if c.cfg.ClientName != "" {
		return c.cfg.ClientName
	}
	return "anthropic/" + c.cfg.Model
}

// --- wire types -------------------------------------------------------------

type cacheControl struct {
	Type string `json:"type"`
}

type contentBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text,omitempty"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type apiMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type apiRequest struct {
	Model       string         `json:"model"`
	MaxTokens   int            `json:"max_tokens"`
	System      []contentBlock `json:"system,omitempty"`
	Messages    []apiMessage   `json:"messages"`
	Temperature *float64       `json:"temperature,omitempty"`
}

type apiUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type apiResponse struct {
	ID         string `json:"id"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage apiUsage `json:"usage"`
}

type apiErrorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// --- request building -------------------------------------------------------

// buildRequest maps a pipeline.Request onto the Messages API shape.
//
// Mapping rules:
//   - "system"-role messages are hoisted out of Messages into the top-level
//     `system` field, in order. Their relative index is preserved for
//     breakpoint mapping.
//   - Consecutive same-role messages are NOT merged; the API tolerates them.
//   - RetrievedChunks are appended as a final user-role message containing one
//     text block per chunk. They are dynamic content and are therefore never
//     eligible for a cache breakpoint (docs/spec.md §2.4.6).
//   - req.Query, if non-empty and not already the last user message, is
//     appended last so the newest content sits at the end of the prompt.
//
// CACHE BREAKPOINT TRUNCATION: Anthropic accepts at most MaxCacheBreakpts (4)
// `cache_control` blocks per request. When req.CacheBreakpoints holds more, the
// LAST 4 by message index are kept. The last breakpoints are the most valuable:
// a cache_control block caches everything *before* it, so the deepest
// breakpoint yields the longest cache prefix, and the earlier ones only add
// shorter fallback prefixes.
func (c *Client) buildRequest(req *pipeline.Request) apiRequest {
	out := apiRequest{
		Model:       c.cfg.Model,
		MaxTokens:   c.cfg.MaxTokens,
		Temperature: c.cfg.Temperature,
	}
	if req == nil {
		out.Messages = []apiMessage{}
		return out
	}

	breakpoints := selectBreakpoints(req.CacheBreakpoints, MaxCacheBreakpts)

	// systemIdx / convIdx map an index in req.Messages to a slot in the
	// system slice or the conversation slice, so breakpoints land correctly.
	var (
		system   []contentBlock
		conv     []apiMessage
		sysOfMsg = map[int]int{}
		cnvOfMsg = map[int]int{}
	)
	for i, m := range req.Messages {
		block := contentBlock{Type: "text", Text: m.Content}
		if strings.EqualFold(m.Role, "system") {
			sysOfMsg[i] = len(system)
			system = append(system, block)
			continue
		}
		role := strings.ToLower(m.Role)
		if role != "assistant" {
			role = "user"
		}
		cnvOfMsg[i] = len(conv)
		conv = append(conv, apiMessage{Role: role, Content: []contentBlock{block}})
	}

	// Apply cache_control to the block that ends each breakpoint's segment.
	for _, bp := range breakpoints {
		idx := bp.AfterMessageIndex
		if idx < 0 || idx >= len(req.Messages) {
			continue
		}
		if s, ok := sysOfMsg[idx]; ok {
			system[s].CacheControl = &cacheControl{Type: cacheControlType}
			continue
		}
		if v, ok := cnvOfMsg[idx]; ok {
			blocks := conv[v].Content
			blocks[len(blocks)-1].CacheControl = &cacheControl{Type: cacheControlType}
		}
	}

	// Retrieved chunks: dynamic, appended after all cacheable content, never
	// marked with cache_control.
	if len(req.RetrievedChunks) > 0 {
		blocks := make([]contentBlock, 0, len(req.RetrievedChunks)+1)
		blocks = append(blocks, contentBlock{Type: "text", Text: "Retrieved context:"})
		for _, ch := range req.RetrievedChunks {
			blocks = append(blocks, contentBlock{Type: "text", Text: formatChunk(ch)})
		}
		conv = append(conv, apiMessage{Role: retrievedRoleUser, Content: blocks})
	}

	// The newest user turn goes last, unless it is already the final message.
	if req.Query != "" && !endsWithQuery(req.Messages, req.Query) {
		conv = append(conv, apiMessage{
			Role:    retrievedRoleUser,
			Content: []contentBlock{{Type: "text", Text: req.Query}},
		})
	}

	// The API requires at least one message.
	if len(conv) == 0 {
		conv = append(conv, apiMessage{
			Role:    retrievedRoleUser,
			Content: []contentBlock{{Type: "text", Text: req.Query}},
		})
	}

	out.System = system
	out.Messages = conv
	return out
}

func endsWithQuery(msgs []pipeline.Message, query string) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.EqualFold(msgs[i].Role, "system") {
			continue
		}
		return msgs[i].Content == query
	}
	return false
}

func formatChunk(ch pipeline.Chunk) string {
	if ch.SourceURL == "" {
		return ch.Content
	}
	return "[source: " + ch.SourceURL + "]\n" + ch.Content
}

// selectBreakpoints keeps at most max breakpoints, preferring the ones deepest
// in the conversation (see buildRequest for why). Order is preserved and
// duplicates on the same message index are collapsed.
func selectBreakpoints(bps []pipeline.CacheBreakpoint, max int) []pipeline.CacheBreakpoint {
	if len(bps) == 0 || max <= 0 {
		return nil
	}
	// Collapse duplicates, keeping first occurrence order.
	seen := make(map[int]bool, len(bps))
	uniq := make([]pipeline.CacheBreakpoint, 0, len(bps))
	for _, bp := range bps {
		if seen[bp.AfterMessageIndex] {
			continue
		}
		seen[bp.AfterMessageIndex] = true
		uniq = append(uniq, bp)
	}
	if len(uniq) <= max {
		return uniq
	}
	// Keep the max deepest by message index, preserving input order.
	threshold := nthLargestIndex(uniq, max)
	kept := make([]pipeline.CacheBreakpoint, 0, max)
	for _, bp := range uniq {
		if bp.AfterMessageIndex >= threshold && len(kept) < max {
			kept = append(kept, bp)
		}
	}
	return kept
}

// nthLargestIndex returns the n-th largest AfterMessageIndex in bps.
func nthLargestIndex(bps []pipeline.CacheBreakpoint, n int) int {
	idxs := make([]int, len(bps))
	for i, bp := range bps {
		idxs[i] = bp.AfterMessageIndex
	}
	// Simple selection: n is at most 4.
	for i := 0; i < n; i++ {
		maxAt := i
		for j := i + 1; j < len(idxs); j++ {
			if idxs[j] > idxs[maxAt] {
				maxAt = j
			}
		}
		idxs[i], idxs[maxAt] = idxs[maxAt], idxs[i]
	}
	return idxs[n-1]
}

// --- send -------------------------------------------------------------------

// Send implements pipeline.ModelClient.
func (c *Client) Send(ctx context.Context, req *pipeline.Request) (*pipeline.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	body, err := json.Marshal(c.buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build http request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", APIVersion)
	httpReq.Header.Set("x-api-key", c.cfg.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: send: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("anthropic: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &Error{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(raw),
		}
		var env apiErrorEnvelope
		if json.Unmarshal(raw, &env) == nil {
			apiErr.Type = env.Error.Type
			apiErr.Message = env.Error.Message
		}
		return nil, apiErr
	}

	return parseResponse(raw, c.Name())
}

func parseResponse(raw []byte, fallbackModel string) (*pipeline.Response, error) {
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	var sb strings.Builder
	for _, blk := range ar.Content {
		if blk.Type == "text" {
			sb.WriteString(blk.Text)
		}
	}

	model := ar.Model
	if model == "" {
		model = fallbackModel
	}
	return &pipeline.Response{
		Content:   sb.String(),
		ModelUsed: model,
		Usage: pipeline.Usage{
			InputTokens:         ar.Usage.InputTokens,
			OutputTokens:        ar.Usage.OutputTokens,
			CacheReadTokens:     ar.Usage.CacheReadInputTokens,
			CacheCreationTokens: ar.Usage.CacheCreationInputTokens,
		},
	}, nil
}
