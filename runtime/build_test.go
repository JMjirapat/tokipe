package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/runtime"
	"github.com/JMjirapat/tokipe/stores"
)

func mustParse(t *testing.T, doc string) *runtime.Spec {
	t.Helper()
	s, err := runtime.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

// The smallest useful document: a provider and nothing else, which must build
// the pass-through pipeline the README calls the measurement baseline.
func TestBuildPassThrough(t *testing.T) {
	spec := mustParse(t, `{"provider": {"type": "mock", "model": "demo", "options": {"content": "hi"}}}`)

	kit, err := runtime.Build(spec, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	resp, err := kit.Run(context.Background(), &pipeline.Request{Query: "anything"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("content = %q, want %q", resp.Content, "hi")
	}
}

// A declared exact rule must short-circuit before the model, and must not
// capture a request it was not told to.
func TestDeclaredPreprocessRule(t *testing.T) {
	spec := mustParse(t, `{
	  "provider": {"type": "mock", "options": {"content": "from model"}},
	  "stages": {"preprocess": {"rules": [
	    {"match": "/help", "respond": "the menu"},
	    {"match": "PING", "respond": "pong", "case_insensitive": true}
	  ]}}
	}`)

	kit, err := runtime.Build(spec, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, tc := range []struct {
		query   string
		want    string
		shorted bool
	}{
		{query: "/help", want: "the menu", shorted: true},
		{query: "ping", want: "pong", shorted: true},   // case-insensitive rule
		{query: "/HELP", want: "from model"},           // case-sensitive rule must not match
		{query: "explain quantum", want: "from model"}, // nothing matches
	} {
		t.Run(tc.query, func(t *testing.T) {
			resp, err := kit.Run(context.Background(), &pipeline.Request{Query: tc.query})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if resp.Content != tc.want {
				t.Errorf("content = %q, want %q", resp.Content, tc.want)
			}
			if resp.ShortCircuited != tc.shorted {
				t.Errorf("shortCircuited = %v, want %v", resp.ShortCircuited, tc.shorted)
			}
		})
	}
}

// Routing across two providers is the cross-provider claim the deck makes; it
// has to hold when the tiers come from a document rather than from Go.
func TestRouterTiersFromDocument(t *testing.T) {
	spec := mustParse(t, `{
	  "provider": {"type": "mock", "model": "fallback", "options": {"content": "fallback"}},
	  "router": {"tiers": [
	    {"provider": {"type": "mock", "model": "cheap", "options": {"content": "cheap"}}, "max_complexity": 0.35},
	    {"provider": {"type": "mock", "model": "strong", "options": {"content": "strong"}}, "max_complexity": 1.0}
	  ]}
	}`)

	kit, err := runtime.Build(spec, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	short := &pipeline.Request{Query: "hi"}
	if _, err := kit.Run(context.Background(), short); err != nil {
		t.Fatalf("Run short: %v", err)
	}
	if got := short.Metadata["router.client"]; got != "cheap" {
		t.Errorf("short request routed to %v, want cheap", got)
	}

	long := &pipeline.Request{Query: strings.Repeat("a complicated question ", 500)}
	if _, err := kit.Run(context.Background(), long); err != nil {
		t.Fatalf("Run long: %v", err)
	}
	if got := long.Metadata["router.client"]; got != "strong" {
		t.Errorf("long request routed to %v, want strong", got)
	}
}

// Every one of these is a document an operator could plausibly write, and every
// one has to fail loudly rather than start a pipeline that is not what it says.
func TestBuildRejectsBadDocuments(t *testing.T) {
	for _, tc := range []struct {
		name    string
		doc     string
		wantErr string
	}{
		{
			name:    "unknown field",
			doc:     `{"provider": {"type": "mock"}, "stages": {"tool_cahce": {}}}`,
			wantErr: "tool_cahce",
		},
		{
			name:    "unknown provider",
			doc:     `{"provider": {"type": "gpt5"}}`,
			wantErr: "unknown provider type",
		},
		{
			name:    "missing provider",
			doc:     `{"stages": {}}`,
			wantErr: "provider is required",
		},
		{
			name:    "duration as number",
			doc:     `{"provider": {"type": "mock", "timeout": 30}}`,
			wantErr: "must be a string",
		},
		{
			name:    "catch-all compressor not last",
			doc:     `{"provider": {"type": "mock"}, "stages": {"compression": {"compressors": ["text", "json"]}}}`,
			wantErr: "must be last",
		},
		{
			name:    "unknown compressor",
			doc:     `{"provider": {"type": "mock"}, "stages": {"compression": {"compressors": ["gzip"]}}}`,
			wantErr: "unknown compressor",
		},
		{
			name:    "empty history budget",
			doc:     `{"provider": {"type": "mock"}, "stages": {"history_budget": {}}}`,
			wantErr: "sets no budgets",
		},
		{
			name: "router tiers out of order",
			doc: `{"provider": {"type": "mock"}, "router": {"tiers": [
			        {"provider": {"type": "mock"}, "max_complexity": 1.0},
			        {"provider": {"type": "mock"}, "max_complexity": 0.35}]}}`,
			wantErr: "ascending max_complexity",
		},
		{
			name: "last router tier does not reach 1",
			doc: `{"provider": {"type": "mock"}, "router": {"tiers": [
			        {"provider": {"type": "mock"}, "max_complexity": 0.35},
			        {"provider": {"type": "mock"}, "max_complexity": 0.9}]}}`,
			wantErr: "last router tier",
		},
		{
			name:    "tool cache cannot come from a document alone",
			doc:     `{"provider": {"type": "mock"}, "stages": {"tool_cache": {"backend": "memory"}}}`,
			wantErr: "needs a tool executor",
		},
		{
			name:    "rag without registered components",
			doc:     `{"provider": {"type": "mock"}, "stages": {"rag": {"embedder": "e", "store": "s"}}}`,
			wantErr: "unknown embedder",
		},
		{
			name:    "dedupe threshold out of range",
			doc:     `{"provider": {"type": "mock"}, "stages": {"dedupe": {"threshold": 1.5}}}`,
			wantErr: "between 0 and 1",
		},
		{
			name:    "unknown custom stage",
			doc:     `{"provider": {"type": "mock"}, "stages": {"custom": ["mine"]}}`,
			wantErr: "unknown custom stage",
		},
		{
			name:    "rule without a response",
			doc:     `{"provider": {"type": "mock"}, "stages": {"preprocess": {"rules": [{"match": "/help"}]}}}`,
			wantErr: "respond is required",
		},
		{
			name:    "two concatenated documents",
			doc:     `{"provider": {"type": "mock"}} {"provider": {"type": "mock"}}`,
			wantErr: "trailing content",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := runtime.Parse([]byte(tc.doc))
			if err == nil {
				_, err = runtime.Build(spec, nil)
			}
			if err == nil {
				t.Fatalf("want an error containing %q, got none", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// An unset variable named by api_key_env must fail at build time. Discovering it
// per-request, once traffic is flowing, is the failure this prevents.
func TestAPIKeyEnvMustBeSet(t *testing.T) {
	spec := mustParse(t, `{"provider": {"type": "anthropic", "api_key_env": "TOKIPE_TEST_MISSING_KEY"}}`)

	if _, err := runtime.Build(spec, nil); err == nil {
		t.Fatal("want an error for an unset api_key_env, got none")
	} else if !strings.Contains(err.Error(), "TOKIPE_TEST_MISSING_KEY") {
		t.Errorf("error = %q, want it to name the missing variable", err)
	}

	t.Setenv("TOKIPE_TEST_MISSING_KEY", "sk-test")
	if _, err := runtime.Build(spec, nil); err != nil {
		t.Errorf("Build with the variable set: %v", err)
	}
}

// The registry is the seam between a document and the Go-only components it
// cannot contain. Registering an embedder and a store must make RAG declarable.
func TestRegistryEnablesRAG(t *testing.T) {
	reg := runtime.NewRegistry()
	reg.RegisterEmbedder("fake", func(map[string]string) (stores.Embedder, error) {
		return embedderFunc(func(context.Context, string) ([]float32, error) {
			return []float32{1, 0}, nil
		}), nil
	})
	reg.RegisterStore("fake", func(opts map[string]string) (stores.VectorStore, error) {
		return storeFunc(func(context.Context, []float32, int) ([]pipeline.Chunk, error) {
			return []pipeline.Chunk{{Content: opts["chunk"]}}, nil
		}), nil
	})

	spec := mustParse(t, `{
	  "provider": {"type": "mock", "options": {"content": "ok"}},
	  "stages": {"rag": {"embedder": "fake", "store": "fake", "top_k": 3, "options": {"chunk": "retrieved!"}}}
	}`)

	kit, err := runtime.Build(spec, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	req := &pipeline.Request{Query: "q", NeedsRetrieval: true}
	if _, err := kit.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(req.RetrievedChunks) != 1 || req.RetrievedChunks[0].Content != "retrieved!" {
		t.Errorf("chunks = %v, want one chunk %q", req.RetrievedChunks, "retrieved!")
	}
}

// Extra options must compose with a document rather than be shadowed by it —
// this is how a tool cache, which a document cannot enable, joins a declared
// pipeline.
func TestBuildAcceptsGoOnlyOptions(t *testing.T) {
	spec := mustParse(t, `{"provider": {"type": "mock", "options": {"content": "ok"}}, "stages": {"cache_alignment": {}}}`)

	var seen bool
	stage := stageFunc{name: "witness", fn: func(_ context.Context, r *pipeline.Request) (*pipeline.Request, error) {
		seen = true
		return r, nil
	}}

	kit, err := runtime.Build(spec, nil, configWithStage(stage))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := kit.Run(context.Background(), &pipeline.Request{Query: "q"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !seen {
		t.Error("the Go-supplied stage did not run")
	}
}

// A duration reads as a string in both directions, so a document that was
// written by hand and one that was generated agree.
func TestDurationRoundTrip(t *testing.T) {
	spec := mustParse(t, `{"provider": {"type": "mock", "timeout": "1h30m"}}`)
	if got := spec.Provider.Timeout.D(); got != 90*time.Minute {
		t.Errorf("timeout = %v, want 1h30m", got)
	}
}

// Registered rules run ahead of declared ones, because a Go rule exists exactly
// when exact-match was not expressive enough.
func TestRegisteredRulesRunFirst(t *testing.T) {
	reg := runtime.NewRegistry()
	reg.RegisterRule("prefix", ruleFunc{
		name:  "prefix",
		match: func(r *pipeline.Request) bool { return strings.HasPrefix(r.Query, "!") },
		fn: func(*pipeline.Request) (*pipeline.Response, error) {
			return &pipeline.Response{Content: "from Go", ShortCircuited: true}, nil
		},
	})

	spec := mustParse(t, `{
	  "provider": {"type": "mock", "options": {"content": "from model"}},
	  "stages": {"preprocess": {"use": ["prefix"], "rules": [{"match": "!x", "respond": "from document"}]}}
	}`)

	kit, err := runtime.Build(spec, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	resp, err := kit.Run(context.Background(), &pipeline.Request{Query: "!x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Content != "from Go" {
		t.Errorf("content = %q, want the registered rule to win", resp.Content)
	}
}

// A pipeline built from a document must be usable with a mock provider that
// records what it received — the shaped request, not the raw one.
func TestDocumentBuiltPipelineShapesTheRequest(t *testing.T) {
	spec := mustParse(t, `{
	  "provider": {"type": "mock", "options": {"content": "ok"}},
	  "stages": {"cache_alignment": {}}
	}`)

	kit, err := runtime.Build(spec, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	req := &pipeline.Request{
		Query: "q",
		Messages: []pipeline.Message{
			{Role: "user", Content: "history"},
			{Role: "system", Content: "instructions", Static: true},
		},
	}
	if _, err := kit.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(req.CacheBreakpoints) == 0 {
		t.Fatal("cache alignment was declared but emitted no breakpoints")
	}
	if req.Messages[0].Content != "instructions" {
		t.Errorf("static content was not hoisted to the front: %v", req.Messages)
	}
}
