package runtime

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/JMjirapat/tokipe/budget"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/preprocess"
	"github.com/JMjirapat/tokipe/providers/anthropic"
	"github.com/JMjirapat/tokipe/providers/cli"
	"github.com/JMjirapat/tokipe/providers/mock"
	"github.com/JMjirapat/tokipe/providers/openai"
	"github.com/JMjirapat/tokipe/stores"
	"github.com/JMjirapat/tokipe/toolcache"
)

// A Registry maps the names a document uses to the Go constructors behind them.
//
// It is a value the caller holds and passes to Build, not a package-level map
// that packages mutate from init functions. That is a deliberate cost: a nested
// module cannot make itself available by being imported, and the program has to
// wire it in one visible line. In exchange, what a document can reach is exactly
// what the program in front of you granted it — which is a property worth having
// when the document is edited by an operator and the process holds API keys.
//
// The zero Registry has nothing registered. Use NewRegistry.
type Registry struct {
	providers map[string]ProviderFactory
	caches    map[string]CacheFactory
	stores    map[string]StoreFactory
	embedders map[string]EmbedderFactory
	counters  map[string]budget.TokenCounter
	rules     map[string]preprocess.Rule
	stages    map[string]pipeline.Stage
}

// ProviderFactory builds a model client from its declared spec. A factory is
// responsible for reading its own credential from the environment, via
// ProviderSpec.APIKeyEnv, so no key ever passes through the document.
type ProviderFactory func(ProviderSpec) (pipeline.ModelClient, error)

// CacheFactory builds a tool-result cache. opts carries backend-specific
// settings from the document, e.g. a Redis address.
type CacheFactory func(opts map[string]string) (toolcache.Cache, error)

// StoreFactory builds a vector store.
type StoreFactory func(opts map[string]string) (stores.VectorStore, error)

// EmbedderFactory builds an embedder.
type EmbedderFactory func(opts map[string]string) (stores.Embedder, error)

// NewRegistry returns a Registry with the built-in components of the core
// module registered: every provider that ships here, the in-memory tool cache,
// and the default token counter.
//
// Nothing that needs a third-party dependency is here, because nothing in the
// core module may have one. pgvector, Redis and OpenTelemetry are nested
// modules; a program that wants them registers them itself:
//
//	reg := runtime.NewRegistry()
//	reg.RegisterCache("redis", func(o map[string]string) (toolcache.Cache, error) {
//	    return redis.New(redis.Config{Addr: o["addr"]})
//	})
func NewRegistry() *Registry {
	r := &Registry{
		providers: map[string]ProviderFactory{},
		caches:    map[string]CacheFactory{},
		stores:    map[string]StoreFactory{},
		embedders: map[string]EmbedderFactory{},
		counters:  map[string]budget.TokenCounter{},
		rules:     map[string]preprocess.Rule{},
		stages:    map[string]pipeline.Stage{},
	}

	r.RegisterProvider("mock", newMockProvider)
	r.RegisterProvider("anthropic", newAnthropicProvider)
	r.RegisterProvider("openai", newOpenAIProvider)
	r.RegisterProvider("cli", newCLIProvider)

	r.RegisterCache("memory", func(map[string]string) (toolcache.Cache, error) {
		return toolcache.NewMemoryCache(), nil
	})

	r.RegisterCounter("estimate", budget.CharEstimator{})

	return r
}

// RegisterProvider adds or replaces a provider factory. Replacing is allowed
// and is how a program overrides a built-in — pointing "anthropic" at a
// recording client in tests, for instance.
func (r *Registry) RegisterProvider(name string, f ProviderFactory) { r.providers[name] = f }

// RegisterCache adds or replaces a tool-cache backend factory.
func (r *Registry) RegisterCache(name string, f CacheFactory) { r.caches[name] = f }

// RegisterStore adds or replaces a vector-store factory.
func (r *Registry) RegisterStore(name string, f StoreFactory) { r.stores[name] = f }

// RegisterEmbedder adds or replaces an embedder factory.
func (r *Registry) RegisterEmbedder(name string, f EmbedderFactory) { r.embedders[name] = f }

// RegisterCounter adds or replaces a token counter.
func (r *Registry) RegisterCounter(name string, c budget.TokenCounter) { r.counters[name] = c }

// RegisterRule makes a Go-written preprocess rule available to a document by
// name. This is the escape hatch for the rules a document cannot express: the
// predicate lives in Go, the decision to enable it lives in the document.
func (r *Registry) RegisterRule(name string, rule preprocess.Rule) { r.rules[name] = rule }

// RegisterStage makes a caller-supplied stage available by name, on the same
// terms as RegisterRule.
func (r *Registry) RegisterStage(name string, s pipeline.Stage) { r.stages[name] = s }

// known lists what is registered under kind, for an error message that tells
// the operator what they could have typed instead.
func known[T any](m map[string]T) string {
	if len(m) == 0 {
		return "none registered"
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (r *Registry) provider(spec ProviderSpec) (pipeline.ModelClient, error) {
	if spec.Type == "" {
		return nil, fmt.Errorf("runtime: provider.type is required (known: %s)", known(r.providers))
	}
	f, ok := r.providers[spec.Type]
	if !ok {
		return nil, fmt.Errorf("runtime: unknown provider type %q (known: %s)", spec.Type, known(r.providers))
	}
	c, err := f(spec)
	if err != nil {
		return nil, fmt.Errorf("runtime: provider %q: %w", spec.Type, err)
	}
	if c == nil {
		return nil, fmt.Errorf("runtime: provider %q factory returned no client and no error", spec.Type)
	}
	return c, nil
}

// apiKey reads the credential a provider spec points at. An APIKeyEnv naming an
// unset variable is an error, not an empty key: the failure a caller wants is
// "TOKIPE_KEY is not set", at startup, not an authentication error per request
// once traffic is flowing.
func apiKey(spec ProviderSpec) (string, error) {
	if spec.APIKeyEnv == "" {
		return "", nil
	}
	v, ok := os.LookupEnv(spec.APIKeyEnv)
	if !ok || v == "" {
		return "", fmt.Errorf("api_key_env names %s, which is unset or empty", spec.APIKeyEnv)
	}
	return v, nil
}

func newMockProvider(spec ProviderSpec) (pipeline.ModelClient, error) {
	name := spec.Model
	if name == "" {
		name = "mock"
	}
	return mock.New(name, spec.Options["content"]), nil
}

func newAnthropicProvider(spec ProviderSpec) (pipeline.ModelClient, error) {
	key, err := apiKey(spec)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fmt.Errorf("api_key_env is required (the Anthropic API needs a key)")
	}
	return anthropic.New(anthropic.Config{
		APIKey:  key,
		Model:   spec.Model,
		BaseURL: spec.BaseURL,
		Timeout: spec.Timeout.D(),
	})
}

func newOpenAIProvider(spec ProviderSpec) (pipeline.ModelClient, error) {
	// Deliberately not required: a local server (Ollama, llama.cpp, vLLM) needs
	// no key, and demanding one would exclude the case this provider is most
	// often used for in a self-hosted runtime.
	key, err := apiKey(spec)
	if err != nil {
		return nil, err
	}
	return openai.New(openai.Config{
		APIKey:  key,
		BaseURL: spec.BaseURL,
		Model:   spec.Model,
		Timeout: spec.Timeout.D(),
	})
}

func newCLIProvider(spec ProviderSpec) (pipeline.ModelClient, error) {
	var cfg cli.Config
	switch strings.ToLower(spec.Options["preset"]) {
	case "claude", "":
		cfg = cli.ClaudePreset(spec.WorkDir)
	case "codex":
		cfg = cli.CodexPreset(spec.WorkDir)
	case "opencode":
		cfg = cli.OpenCodePreset(spec.WorkDir, spec.Model)
	default:
		return nil, fmt.Errorf("unknown cli preset %q (known: claude, codex, opencode)", spec.Options["preset"])
	}
	if spec.Timeout > 0 {
		cfg.Timeout = time.Duration(spec.Timeout)
	}
	return cli.New(cfg)
}
