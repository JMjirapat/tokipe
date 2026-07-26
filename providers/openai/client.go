// Package openai implements pipeline.ModelClient against the OpenAI
// chat-completions API — and, because that shape has become a de facto
// standard, against everything that speaks it: Ollama, vLLM, llama.cpp, Groq,
// Together, OpenRouter, LM Studio and Azure OpenAI.
//
// One adapter rather than one per vendor is the whole point. What varies
// between them is a base URL, a model name and which optional fields the server
// honours; none of that needs a different Go type.
//
// Built on net/http and encoding/json only, so it lives in the core module
// without touching the zero-dependency guarantee (spec §2.6).
//
// # Cache breakpoints
//
// Request.CacheBreakpoints are ADVISORY here and are not transmitted. Unlike
// Anthropic, these servers have no cache_control field; those that cache do it
// automatically on the longest matching prefix. cache.Aligner still earns its
// keep by keeping that prefix stable — it just cannot be told about it.
//
// Usage is reported when the server reports it. Many OpenAI-compatible servers
// omit prompt_tokens_details, in which case CacheReadTokens stays zero — absent
// data, not a measurement of zero.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/JMjirapat/tokipe/pipeline"
)

// API defaults.
const (
	DefaultBaseURL    = "https://api.openai.com/v1"
	ChatPath          = "/chat/completions"
	DefaultModel      = "gpt-4o-mini"
	DefaultMaxTokens  = 4096
	DefaultTimeout    = 60 * time.Second
	roleUser          = "user"
	roleAssistant     = "assistant"
	roleSystem        = "system"
	retrievedRoleUser = roleUser
)

// Error is the typed error for a non-2xx response.
type Error struct {
	StatusCode int
	Status     string
	Type       string
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
	return fmt.Sprintf("openai: http %d %s: %s", e.StatusCode, e.Status, msg)
}

// Retryable reports whether the status suggests the request may succeed later.
func (e *Error) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// Config configures a Client.
type Config struct {
	// APIKey is sent as a bearer token. Optional: local servers such as Ollama
	// and llama.cpp accept requests without one, so an empty key is not an
	// error here — unlike the Anthropic adapter, where it always is.
	APIKey string

	// BaseURL defaults to DefaultBaseURL. Point it at any compatible server,
	// e.g. "http://localhost:11434/v1" for Ollama.
	BaseURL string

	// Model defaults to DefaultModel.
	Model string

	// MaxTokens defaults to DefaultMaxTokens. Sent as max_tokens.
	MaxTokens int

	// Temperature, if non-nil, is sent verbatim.
	Temperature *float64

	// Timeout bounds a single Send. Defaults to DefaultTimeout.
	Timeout time.Duration

	// HTTPClient defaults to a client with Timeout.
	HTTPClient *http.Client

	// ClientName overrides the value returned by Name().
	ClientName string

	// ExtraHeaders are added to every request — for OpenRouter's HTTP-Referer,
	// Azure's api-key, or anything else a compatible server requires.
	ExtraHeaders map[string]string
}

// Client is a pipeline.ModelClient for OpenAI-compatible servers.
type Client struct {
	cfg  Config
	http *http.Client
	url  string
}

var _ pipeline.ModelClient = (*Client)(nil)

// New builds a Client. It never fails on a missing API key, because local
// compatible servers legitimately do not use one.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = DefaultMaxTokens
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
		url:  strings.TrimRight(cfg.BaseURL, "/") + ChatPath,
	}, nil
}

// Name implements pipeline.ModelClient. It never includes the API key.
func (c *Client) Name() string {
	if c.cfg.ClientName != "" {
		return c.cfg.ClientName
	}
	return "openai/" + c.cfg.Model
}

// --- wire types -------------------------------------------------------------

type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiRequest struct {
	Model       string       `json:"model"`
	Messages    []apiMessage `json:"messages"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
	// StreamOptions asks for a usage record on the final SSE frame. Servers
	// that do not understand it ignore it; those that do stop making streaming
	// a blind spot for accounting.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type apiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// PromptTokensDetails is where OpenAI reports its automatic prompt cache
	// hits. Absent on most compatible servers.
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type apiResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      apiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage apiUsage `json:"usage"`
}

type apiErrorEnvelope struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// --- request building -------------------------------------------------------

// buildRequest maps a pipeline.Request onto the chat-completions shape.
//
// Layout mirrors the Anthropic adapter so both providers see the same ordering
// the aligner produced: system messages first, then history, then retrieved
// context, then the newest turn exactly once.
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

	var system, conv []apiMessage
	for _, m := range req.Messages {
		msg := apiMessage{Role: normalizeRole(m.Role), Content: m.Content}
		if msg.Role == roleSystem {
			system = append(system, msg)
			continue
		}
		conv = append(conv, msg)
	}

	// Hold back the newest turn so retrieved context can precede it — the same
	// rule as the Anthropic adapter, and for the same reason: evidence after
	// the question reads differently to a model.
	var newest *apiMessage
	last := lastNonSystem(req.Messages)
	switch {
	case req.Query != "" && last >= 0 && req.Messages[last].Content == req.Query:
		if n := len(conv); n > 0 {
			newest = &conv[n-1]
			conv = conv[:n-1]
		}
	case req.Query != "":
		newest = &apiMessage{Role: roleUser, Content: req.Query}
	case last >= 0 && len(conv) > 0:
		newest = &conv[len(conv)-1]
		conv = conv[:len(conv)-1]
	}

	msgs := append(system, conv...)

	if len(req.RetrievedChunks) > 0 {
		var b strings.Builder
		b.WriteString("Retrieved context:\n")
		for _, ch := range req.RetrievedChunks {
			if ch.SourceURL != "" {
				fmt.Fprintf(&b, "[%s]\n", ch.SourceURL)
			}
			b.WriteString(ch.Content)
			b.WriteString("\n")
		}
		msgs = append(msgs, apiMessage{Role: roleUser, Content: b.String()})
	}

	if newest != nil {
		msgs = append(msgs, *newest)
	}
	if len(msgs) == 0 {
		msgs = []apiMessage{{Role: roleUser, Content: req.Query}}
	}

	out.Messages = msgs
	return out
}

func normalizeRole(role string) string {
	switch strings.ToLower(role) {
	case roleSystem:
		return roleSystem
	case roleAssistant:
		return roleAssistant
	default:
		return roleUser
	}
}

func lastNonSystem(msgs []pipeline.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if !strings.EqualFold(msgs[i].Role, roleSystem) {
			return i
		}
	}
	return -1
}

// --- send -------------------------------------------------------------------

// Send implements pipeline.ModelClient.
func (c *Client) Send(ctx context.Context, req *pipeline.Request) (*pipeline.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	httpReq, err := c.newRequest(ctx, c.buildRequest(req))
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: send: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("openai: read response: %w", err)
	}
	if apiErr := errorFor(resp, raw); apiErr != nil {
		return nil, apiErr
	}

	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("openai: response contained no choices")
	}

	model := out.Model
	if model == "" {
		model = c.Name()
	}
	return &pipeline.Response{
		Content:   out.Choices[0].Message.Content,
		ModelUsed: model,
		Usage: pipeline.Usage{
			InputTokens:     out.Usage.PromptTokens,
			OutputTokens:    out.Usage.CompletionTokens,
			CacheReadTokens: out.Usage.PromptTokensDetails.CachedTokens,
		},
	}, nil
}

func (c *Client) newRequest(ctx context.Context, payload apiRequest) (*http.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build http request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("authorization", "Bearer "+c.cfg.APIKey)
	}
	for k, v := range c.cfg.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}
	return httpReq, nil
}

// errorFor returns a typed error for a non-2xx response, or nil.
func errorFor(resp *http.Response, raw []byte) *Error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	apiErr := &Error{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(raw)}
	var env apiErrorEnvelope
	if json.Unmarshal(raw, &env) == nil {
		apiErr.Type = env.Error.Type
		apiErr.Message = env.Error.Message
	}
	return apiErr
}
