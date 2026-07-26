package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"agentkit/budget"
)

// CountTokensPath is the endpoint that reports an exact token count.
const CountTokensPath = "/v1/messages/count_tokens"

// TokenCounter is an exact, provider-backed budget.TokenCounter.
//
// It costs a network round trip per call, which is the whole trade-off:
// budget.CharEstimator is free and approximate, this is exact and not. Use it
// when trimming against a hard context limit, where a 20% estimate error means
// a rejected request; stay with the estimator when trimming for cost, where the
// error washes out.
//
// Counting is cached by content, because a trimming pass asks about the same
// unchanged history messages on every turn. Without the cache, enabling this
// counter would add one HTTP request per message per turn.
type TokenCounter struct {
	client *Client

	mu    sync.RWMutex
	cache map[string]int
	// maxCache bounds the memory a long-running process gives to the cache.
	maxCache int
}

var _ budget.TokenCounter = (*TokenCounter)(nil)

// DefaultCounterCacheSize bounds TokenCounter's memo table. Entries are small
// (a hash-sized key and an int), so this is generous; it exists to stop an
// unbounded conversation growing the map forever.
const DefaultCounterCacheSize = 4096

// NewTokenCounter returns a counter backed by c's credentials and model.
func NewTokenCounter(c *Client) (*TokenCounter, error) {
	if c == nil {
		return nil, fmt.Errorf("anthropic: NewTokenCounter requires a client")
	}
	return &TokenCounter{
		client:   c,
		cache:    make(map[string]int),
		maxCache: DefaultCounterCacheSize,
	}, nil
}

// CountTokens implements budget.TokenCounter.
//
// An error means "unknown"; every caller in agentkit treats that as a reason to
// skip trimming rather than to fail the turn, so a rate-limited or unreachable
// endpoint degrades to no trimming rather than to a broken request.
func (t *TokenCounter) CountTokens(ctx context.Context, text string) (int, error) {
	if strings.TrimSpace(text) == "" {
		return 0, nil
	}
	if n, ok := t.lookup(text); ok {
		return n, nil
	}

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, t.client.cfg.Timeout)
	defer cancel()

	payload := countRequest{
		Model: t.client.cfg.Model,
		Messages: []apiMessage{{
			Role:    retrievedRoleUser,
			Content: []contentBlock{{Type: "text", Text: text}},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("anthropic: marshal count request: %w", err)
	}

	url := strings.TrimRight(t.client.cfg.BaseURL, "/") + CountTokensPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("anthropic: build count request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", APIVersion)
	httpReq.Header.Set("x-api-key", t.client.cfg.APIKey)

	resp, err := t.client.http.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("anthropic: count tokens: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("anthropic: read count response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &Error{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(raw)}
		var env apiErrorEnvelope
		if json.Unmarshal(raw, &env) == nil {
			apiErr.Type = env.Error.Type
			apiErr.Message = env.Error.Message
		}
		return 0, apiErr
	}

	var out countResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("anthropic: decode count response: %w", err)
	}
	if out.InputTokens <= 0 {
		return 0, fmt.Errorf("anthropic: count response reported %d tokens", out.InputTokens)
	}

	t.store(text, out.InputTokens)
	return out.InputTokens, nil
}

func (t *TokenCounter) lookup(text string) (int, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, ok := t.cache[text]
	return n, ok
}

func (t *TokenCounter) store(text string, n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Simplest bound that cannot leak: at the cap, start over. An LRU would
	// keep more useful entries, but would also need eviction bookkeeping in a
	// hot path to save a round trip that only happens once per distinct string.
	if len(t.cache) >= t.maxCache {
		t.cache = make(map[string]int, t.maxCache)
	}
	t.cache[text] = n
}

// CacheSize reports how many counts are memoised. Test and diagnostic helper.
func (t *TokenCounter) CacheSize() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.cache)
}

type countRequest struct {
	Model    string       `json:"model"`
	Messages []apiMessage `json:"messages"`
}

type countResponse struct {
	InputTokens int `json:"input_tokens"`
}
