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
	"sync"
	"testing"

	"agentkit/budget"
)

func counterServer(t *testing.T, handler http.HandlerFunc) *TokenCounter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New(Config{APIKey: "k", BaseURL: srv.URL, Model: "claude-opus-5"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tc, err := NewTokenCounter(c)
	if err != nil {
		t.Fatalf("NewTokenCounter: %v", err)
	}
	return tc
}

func TestTokenCounterReportsTheProvidersCount(t *testing.T) {
	var gotPath, gotModel string
	tc := counterServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var req countRequest
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		fmt.Fprint(w, `{"input_tokens":137}`)
	})

	n, err := tc.CountTokens(context.Background(), "some text to measure")
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 137 {
		t.Errorf("count = %d, want the provider's 137", n)
	}
	if gotPath != CountTokensPath {
		t.Errorf("path = %q, want %q", gotPath, CountTokensPath)
	}
	if gotModel != "claude-opus-5" {
		t.Errorf("model = %q, want the client's model", gotModel)
	}
}

// A trimming pass asks about the same unchanged history on every turn. Without
// memoisation, enabling this counter would add an HTTP request per message per
// turn — which would make it unusable for the job it exists to do.
func TestRepeatedTextIsCountedOnce(t *testing.T) {
	var calls int
	tc := counterServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"input_tokens":42}`)
	})

	for range 10 {
		if _, err := tc.CountTokens(context.Background(), "identical text"); err != nil {
			t.Fatalf("CountTokens: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("made %d requests for one distinct string, want 1", calls)
	}
	if _, err := tc.CountTokens(context.Background(), "different text"); err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if calls != 2 {
		t.Errorf("made %d requests for two distinct strings, want 2", calls)
	}
}

func TestEmptyTextCostsNoRequest(t *testing.T) {
	var calls int
	tc := counterServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"input_tokens":1}`)
	})

	for _, text := range []string{"", "   ", "\n\t"} {
		n, err := tc.CountTokens(context.Background(), text)
		if err != nil || n != 0 {
			t.Errorf("CountTokens(%q) = %d, %v; want 0, nil", text, n, err)
		}
	}
	if calls != 0 {
		t.Errorf("made %d requests for empty input, want 0", calls)
	}
}

// An error means "unknown", and every agentkit caller treats that as a reason to
// skip trimming rather than fail. These cases must therefore be errors, not
// zero counts that would silently look like free text.
func TestCounterFailuresAreErrorsNotZero(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"rate limited": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
		},
		"malformed body": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `not json`)
		},
		"zero tokens for non-empty text": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"input_tokens":0}`)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			tc := counterServer(t, h)
			n, err := tc.CountTokens(context.Background(), "text")
			if err == nil {
				t.Fatalf("want an error, got count %d", n)
			}
			if n != 0 {
				t.Errorf("a failed count must return 0, got %d", n)
			}
		})
	}
}

func TestCounterSurfacesTypedAPIError(t *testing.T) {
	tc := counterServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
	})

	_, err := tc.CountTokens(context.Background(), "text")
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *anthropic.Error", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Type != "authentication_error" {
		t.Errorf("err = %+v", apiErr)
	}
	if strings.Contains(err.Error(), "k") && strings.Contains(err.Error(), "x-api-key") {
		t.Error("the error must not leak the API key")
	}
}

func TestCounterCanceledContextFailsWithoutARequest(t *testing.T) {
	var calls int
	tc := counterServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"input_tokens":1}`)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tc.CountTokens(ctx, "text"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("made %d requests on a canceled context, want 0", calls)
	}
}

// The cache is bounded: a long-running process must not grow it forever.
func TestCounterCacheIsBounded(t *testing.T) {
	tc := counterServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"input_tokens":1}`)
	})
	tc.maxCache = 8

	for i := range 40 {
		if _, err := tc.CountTokens(context.Background(), fmt.Sprintf("text %d", i)); err != nil {
			t.Fatalf("CountTokens: %v", err)
		}
	}
	if got := tc.CacheSize(); got > 8 {
		t.Errorf("cache holds %d entries, want at most 8", got)
	}
}

func TestCounterIsConcurrencySafe(t *testing.T) {
	tc := counterServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"input_tokens":7}`)
	})

	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 10 {
				text := fmt.Sprintf("goroutine %d item %d", i, j%3)
				if n, err := tc.CountTokens(context.Background(), text); err != nil || n != 7 {
					t.Errorf("CountTokens = %d, %v", n, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestNewTokenCounterRejectsANilClient(t *testing.T) {
	if _, err := NewTokenCounter(nil); err == nil {
		t.Error("a nil client should be rejected at construction")
	}
}

func TestSatisfiesBudgetTokenCounter(t *testing.T) {
	c, err := New(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tc, err := NewTokenCounter(c)
	if err != nil {
		t.Fatalf("NewTokenCounter: %v", err)
	}
	var _ budget.TokenCounter = tc
}
