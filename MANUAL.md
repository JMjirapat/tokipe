# tokipe User Manual

This manual explains how to integrate, configure, operate, and extend
`tokipe`. It describes the frozen v1 core plus the additive Delivery 2
capabilities in the current source tree.

## 1. When to use tokipe

Use tokipe when an application sends repeated or growing context to an LLM
and you want to reduce cost or latency without coupling business logic to one
provider.

It is most useful for:

- deterministic requests that should never reach a model;
- repeated tool calls whose results can be safely reused;
- retrieval workflows with verbose chunks;
- long-running conversations that can benefit from stable-prefix caching;
- mixed workloads that can route simple work to a cheaper model;
- coding agents that can carry references instead of eagerly loading files.

It does not host a model, run a service, execute tools autonomously, provide an
embedding implementation, or decide what content your application is allowed
to expose.

## 2. Installation

### Requirements

- Go 1.23+
- no CGo for normal builds;
- CGo only when running Go's race detector;
- provider credentials or an authenticated CLI only for live model calls.

### Current local-module setup

The module path is `github.com/JMjirapat/tokipe`; the root package name remains
`tokipe`. The repository is not published yet, so a separate application must
use a local replacement until the module is pushed.

For a separate application located next to this checkout:

```go
module myapp

go 1.23

require github.com/JMjirapat/tokipe v1.0.0

replace github.com/JMjirapat/tokipe => ../tokipe
```

After publication, remove the `replace` directive and keep the canonical module
path and tagged version.

The Redis, pgvector, and OpenTelemetry adapters are nested Go modules.
Consumers that need them must add those modules and their third-party
dependencies explicitly. The root module remains standard-library-only.

## 3. Mental model

A request is mutable state that flows through zero or more stages. The final
request is sent to one `ModelClient`.

```text
caller
  │
  ├─ preprocess ───── may return a final response immediately
  ├─ tool cache ───── resolves repeatable tool calls
  ├─ RAG ──────────── adds retrieved chunks
  ├─ chunk dedupe ─── removes duplicate evidence
  ├─ compression ──── shrinks retrieved chunks
  ├─ history budget ─ trims messages/chunks to the turn limit
  ├─ custom stages ── caller-specific processing
  ├─ cache alignment  reorders content and places breakpoints
  ├─ routing ───────── chooses the final client
  ▼
ModelClient.Send or StreamingClient.SendStream
```

`config.Config` owns this order. Do not treat it as a presentation detail:
placing a provider cache breakpoint after changing per-turn content can turn a
cache optimization into a permanent cache-write surcharge.

## 4. Core types

### Request

| Field | Meaning | Guidance |
|---|---|---|
| `Query` | Current user question or instruction | Prefer the exact current turn |
| `Messages` | Conversation history, oldest first | Include roles and stable system content |
| `ToolCalls` | Tool calls waiting to be resolved | Only include calls safe for the configured cache |
| `NeedsRetrieval` | Enables RAG for this request | False by default |
| `RetrievedChunks` | Chunks already present or added by RAG | Treat as dynamic |
| `TurnType` | New question, routine resume, or recovery | Classify before `Run` if budgets are used |
| `CacheBreakpoints` | Advisory provider-cache anchors | Normally populated by the aligner |
| `Metadata` | Namespaced stage/caller data | Use `req.SetMeta` to allocate safely |

Example:

```go
req := &pipeline.Request{
	Query:          "Summarize the retry policy.",
	NeedsRetrieval: true,
	TurnType:       pipeline.TurnNewQuestion,
	Messages: []pipeline.Message{
		{
			Role:    "system",
			Content: "Answer only from approved documentation.",
			Static:  true,
		},
		{Role: "user", Content: "Summarize the retry policy."},
	},
}
```

The current turn may appear in `Query`, as the last message, or both. The
provider adapters avoid duplicating it. Keeping `Query` populated is simplest.

### Message

Supported role values are normally `system`, `user`, and `assistant`.
Provider adapters normalize unsupported conversational roles as needed.

`Static: true` means the content is guaranteed not to change during the
session. Good candidates are:

- system instructions;
- tool schemas;
- invariant policy text.

Do not mark user messages, retrieved chunks, timestamps, request IDs, or
session-specific state as static.

### Response

| Field | Meaning |
|---|---|
| `Content` | Final text result |
| `ModelUsed` | Backend/model that produced it |
| `ShortCircuited` | True when preprocess answered without a model |
| `Usage` | Input, output, cache-read, and cache-creation token counts |

Usage fields are backend-dependent. The Anthropic adapter maps API usage.
CLI usage describes the CLI's own prompt scaffold when the CLI exposes it.

## 5. Creating a pipeline

### Pass-through baseline

```go
kit := tokipe.New(client)
resp, err := kit.Run(ctx, &pipeline.Request{Query: "Hello"})
```

Start here. Record latency, token usage, and quality before enabling
optimizations so improvements can be measured rather than assumed.

### Full configuration

```go
rec := metrics.NewInMemory()

kit := tokipe.New(strongClient,
	config.WithMetrics(rec),
	config.WithPreprocess(rules...),
	config.WithToolCache(
		toolcache.NewMemoryCache(),
		executeTool,
		30*time.Minute,
	),
	config.WithRAG(embedder, store, 5),
	config.WithChunkDedupe(),
	config.WithDefaultCompression(),
	config.WithHistoryBudget(
		budget.DefaultPolicy(),
		nil, // nil uses budget.CharEstimator
	),
	config.WithStage(myStage),
	config.WithCacheAlignment(),
	config.WithRouter(router.NewHeuristicRouter(
		router.Tier{Client: cheapClient, MaxComplexity: 0.35},
		router.Tier{Client: strongClient, MaxComplexity: 1.00},
	)),
)
```

`tokipe.NewFromConfig` is available when configuration is resolved earlier:

```go
cfg := config.New(options...)
kit := tokipe.NewFromConfig(defaultClient, cfg)
```

## 6. Model providers

### Anthropic Messages API

```go
client, err := anthropic.New(anthropic.Config{
	APIKey:    os.Getenv("ANTHROPIC_API_KEY"),
	Model:     anthropic.DefaultModel,
	MaxTokens: 4096,
	Timeout:   60 * time.Second,
})
if err != nil {
	return err
}
```

Optional fields include `BaseURL`, `HTTPClient`, `Temperature`, and
`ClientName`. Non-2xx responses are returned as `*anthropic.Error`:

```go
var apiErr *anthropic.Error
if errors.As(err, &apiErr) && apiErr.Retryable() {
	// 429 or 5xx: retry according to application policy.
}
```

The adapter:

- moves system messages into Anthropic's top-level system field;
- puts retrieved chunks before the newest turn;
- emits at most four `cache_control` blocks;
- implements incremental SSE streaming through `RunStream`;
- preserves token and cache usage;
- never includes the API key in `Name` or errors.

### CLI provider

Presets are included for Claude, Codex, and OpenCode:

```go
claudeClient, err := cli.New(cli.ClaudePreset(workDir))
codexClient, err := cli.New(cli.CodexPreset(workDir))
openCodeClient, err := cli.New(cli.OpenCodePreset(workDir, "provider/model"))
```

Operational notes:

- the executable must be on `PATH` and already authenticated;
- set `workDir` deliberately because coding CLIs inspect that directory;
- Codex expects a trusted Git working directory;
- prompts use stdin by default, keeping them out of process arguments;
- OpenCode uses an argument placeholder as required by its CLI;
- the default timeout is five minutes;
- no shell is used.

To adapt another command:

```go
client, err := cli.New(cli.Config{
	Command:    "my-agent",
	Args:       []string{"run", "--json"},
	PromptMode: cli.PromptViaStdin,
	Parse:      myParser,
	StreamParse: myStreamParser, // optional; nil = one completed delta
	Dir:        workDir,
	Timeout:    2 * time.Minute,
	ClientName: "my-agent/model",
})
```

Set `Env` only when you intend to replace the subprocess environment entirely.

`cli.Client` can stream stdout line by line when `StreamParse` matches the
command's incremental output format. Built-in parsers are
`ClaudeStreamParser`, `CodexStreamParser`, and `LineStreamParser`. The command
arguments and parser must describe the same format:

```go
cfg := cli.CodexPreset(workDir) // already uses `codex exec --json`
cfg.StreamParse = cli.CodexStreamParser()
client, err := cli.New(cfg)
```

Without `StreamParse`, `RunStream` remains valid but waits for `Send` and emits
one completed delta. This avoids exposing protocol/log lines as answer text.

### OpenAI-compatible provider

One adapter supports OpenAI's chat-completions API and compatible servers such
as Ollama, vLLM, llama.cpp, Groq, Together, OpenRouter, LM Studio, and Azure
OpenAI:

```go
client, err := openai.New(openai.Config{
	APIKey:  os.Getenv("OPENAI_API_KEY"), // optional for local servers
	BaseURL: "https://api.openai.com/v1",
	Model:   "gpt-4o-mini",
	ExtraHeaders: map[string]string{
		// Add provider-specific headers only when required.
	},
})
```

Set `BaseURL` to the server's `/v1` endpoint. `APIKey` may be empty for local
servers. Non-2xx responses are returned as `*openai.Error`, whose `Retryable`
method recognizes 429 and 5xx responses.

The adapter supports streaming and usage reporting. Explicit
`CacheBreakpoints` are not serialized because this API has no portable
`cache_control` field; cache alignment still improves automatic longest-prefix
caches by stabilizing request order.

### Custom provider

```go
type myClient struct{}

func (myClient) Name() string { return "vendor/model" }

func (myClient) Send(
	ctx context.Context,
	req *pipeline.Request,
) (*pipeline.Response, error) {
	// Translate req, make the provider call, and map usage.
	return &pipeline.Response{
		Content:   "answer",
		ModelUsed: "vendor/model",
	}, nil
}
```

Respect context cancellation and treat `CacheBreakpoints` as advisory. Ignore
features your provider cannot represent instead of translating them
incorrectly.

## 7. Deterministic preprocessing

Rules are evaluated in order. The first successful result ends the pipeline
without a model call.

```go
rule := preprocess.RuleFunc{
	RuleName: "health_check",
	Match: func(req *pipeline.Request) bool {
		return strings.EqualFold(strings.TrimSpace(req.Query), "health")
	},
	Fn: func(*pipeline.Request) (*pipeline.Response, error) {
		return &pipeline.Response{
			Content:        `{"status":"ok"}`,
			ModelUsed:      "none",
			ShortCircuited: true,
		}, nil
	},
}

kit := tokipe.New(client, config.WithPreprocess(rule))
```

A rule error, nil result, or panic is treated as “not handled”; later rules and
the model remain available. Rule registration order is therefore both
precedence and fallback order.

Use `preprocess.Registry` when rules are assembled from multiple packages:

```go
registry := preprocess.NewRegistry(defaultRules...)
registry.Register(applicationRules...)
kit := tokipe.New(client, config.WithPreprocess(registry.Rules()...))
```

Registering the same name replaces the earlier rule without changing its
position.

## 8. Tool-result caching

tokipe never decides which tools to call. It receives `Request.ToolCalls`,
looks up deterministic `(name, args)` keys, and invokes your executor for
misses.

```go
execute := func(
	ctx context.Context,
	call pipeline.ToolCall,
) (any, error) {
	switch call.Name {
	case "lookup_customer":
		return lookupCustomer(ctx, call.Args["id"])
	default:
		return nil, fmt.Errorf("unknown tool %q", call.Name)
	}
}

kit := tokipe.New(client,
	config.WithToolCache(
		toolcache.NewMemoryCache(),
		execute,
		15*time.Minute,
	),
)
```

Resolved values are placed in `req.Metadata[toolcache.MetaResults]`. The CLI
renderer includes them in `<tool_results>`.

Only cache deterministic and authorization-safe tools. Do not cache operations
such as money transfer, email sending, mutation, clock reads, or user-specific
lookups unless identity and all relevant state are part of the key and reuse
is explicitly safe.

`HashToolCall` canonicalizes supported argument maps. Uncacheable arguments are
executed normally without storage. Concurrent identical misses are
singleflight-coalesced by default.

### Redis cache

The Redis adapter is a nested module:

```go
rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
cacheStore, err := agentredis.New(
	rdb,
	agentredis.WithPrefix("myapp:tokipe:"),
)

kit := tokipe.New(client,
	config.WithToolCache(cacheStore, execute, 15*time.Minute),
)
```

Redis failures degrade to misses. Values must be JSON-encodable.

## 9. Retrieval-augmented generation

Provide an embedder and vector store:

```go
type Embedder interface {
	Embed(context.Context, string) ([]float32, error)
}

type VectorStore interface {
	Search(context.Context, []float32, int) ([]pipeline.Chunk, error)
}
```

Enable RAG:

```go
kit := tokipe.New(client, config.WithRAG(embedder, store, 5))

resp, err := kit.Run(ctx, &pipeline.Request{
	Query:          "What is the retry policy?",
	NeedsRetrieval: true,
})
```

Retrieval only runs when `NeedsRetrieval` is true. Embedding or store errors
fail open and the request continues without new chunks.

For local development, `stores/mock` provides a deterministic embedder and
in-memory cosine store. See `examples/rag-chatbot`.

### pgvector

The pgvector adapter is a nested module:

```go
store, pool, err := pgvector.Connect(ctx, dsn, pgvector.Config{
	Table:           "documents",
	ContentColumn:   "content",
	EmbeddingColumn: "embedding",
	SourceColumn:    "source_url",
	MaxTopK:         20,
})
if err != nil {
	return err
}
defer pool.Close()
```

Configured identifiers accept only plain ASCII SQL identifiers. Query values
and limits are bound parameters. The database must already have the pgvector
extension, table, embeddings, and a suitable index.

## 10. Compression

Default compression tries JSON first, then prose:

```go
config.WithDefaultCompression()
```

JSON is minified without converting numbers through `float64`. You can drop
known low-value keys:

```go
config.WithCompression(
	compress.NewJSONCompressor("_trace", "_debug", "raw_html"),
	compress.NewTextCompressor(),
)
```

The text compressor only normalizes whitespace; it does not summarize, remove
sentences, or change non-whitespace tokens. Register catch-all text compression
last.

`CodeCompressor` handles complete Go files with `go/ast`:

```go
config.WithCompression(
	compress.NewJSONCompressor(),
	compress.NewCodeCompressor(),
	compress.NewTextCompressor(),
)
```

By default it removes comments and retains function bodies. It refuses invalid
Go, compiler directives, and cgo files (`import "C"`), and returns the original
when the result grows, stops parsing, or loses a declaration.

`compress.WithBodyElision()` replaces function bodies while retaining the API
surface. That is intentionally lossy: use it for API discovery, never when the
model must inspect behavior.

Compression applies to retrieved chunks. If a compressor errors or panics, the
original chunk is preserved.

### Chunk dedupe

Enable dedupe separately:

```go
config.WithChunkDedupe()
```

It runs after RAG and before compression. The safe default drops a chunk only
when its complete normalized word sequence equals one already kept; formatting,
case, and surrounding punctuation may normalize away, but reordered or changed
words remain distinct.

Lowering the threshold explicitly enables lossy lexical near-deduplication:

```go
config.WithChunkDedupe(
	compress.WithDedupeThreshold(0.90),
)
```

Any threshold below `1.0` can discard a near-copy with a meaningful difference.
Keep the default unless that quality trade-off has been evaluated on real
retrieval results.

## 11. Prompt-cache alignment

Enable the safe default:

```go
config.WithCacheAlignment()
```

The aligner:

1. moves static/system messages to the front;
2. preserves append-only history order;
3. keeps retrieved chunks in the dynamic segment;
4. keeps the newest message last;
5. emits a breakpoint after the static prefix.

```go
messages := []pipeline.Message{
	{Role: "system", Content: systemPrompt, Static: true},
	{Role: "user", Content: previousQuestion},
	{Role: "assistant", Content: previousAnswer},
}
```

Do not mark changing content static. Cache hits require the bytes before a
breakpoint to remain identical, not merely semantically equivalent.

Anthropic consumes explicit breakpoints. CLI and other providers may ignore
the markers, but stable ordering can still benefit automatic prefix caches.

## 12. Model routing

`HeuristicRouter` scores final prompt length, code density, and retrieved-chunk
count. It selects the first tier whose maximum complexity covers the score.

```go
r := router.NewHeuristicRouter(
	router.Tier{Client: local, MaxComplexity: 0.35},
	router.Tier{Client: frontier, MaxComplexity: 1.00},
)

kit := tokipe.New(frontier, config.WithRouter(r))
```

Tiers are defensively sorted. The default client is used if no router is
configured, the router gives no opinion, or the router fails.

Tune the policy for your workload:

```go
r := router.NewHeuristicRouterWith(
	[]router.Tier{
		{Client: local, MaxComplexity: 0.30},
		{Client: frontier, MaxComplexity: 1.00},
	},
	router.WithWeights(router.Weights{
		Length: 0.3,
		Code:   0.2,
		Chunks: 0.5,
	}),
	router.WithChunkSaturation(8),
)
```

The selected client and reason are recorded in request metadata:

```go
clientName := req.Metadata["router.client"]
reason := req.Metadata["router.reason"]
```

Route quality must be evaluated against real task outcomes, not token cost
alone.

## 13. Turn budgets

`WithHistoryBudget` installs a stage that enforces `budget.Policy` after
compression and before cache alignment:

```go
kit := tokipe.New(client,
	config.WithHistoryBudget(
		budget.DefaultPolicy(),
		nil, // nil = budget.CharEstimator
		history.WithRetention(2, 4),
	),
	config.WithCacheAlignment(),
)

req.TurnType = budget.Classify(req)
```

Defaults are:

| Turn type | Budget |
|---|---:|
| Routine resume | 4,000 tokens |
| New question / unknown | 12,000 tokens |
| Error recovery | 20,000 tokens |

The classifier is heuristic. If your orchestrator knows the actual turn type,
set it directly.

The stage never removes static/system content or the newest message. It trims
eligible middle history first, then lower-ranked retrieved chunks while keeping
at least one. If protected content alone exceeds the limit, the request
continues and `history.over_budget` is reported.

The default counter is `budget.CharEstimator`: free and approximate. For a hard
Anthropic context limit, use `anthropic.NewTokenCounter(client)`. A counter
failure is fail-open and reported as a degradation.

An optional `history.Summarizer` may replace dropped messages with a smaller
summary. It can add model cost and latency, so it is opt-in.

## 14. Lazy loading

Lazy loading is intentionally caller-managed because exposing a content loader
changes the model's tool surface.

```go
loader, err := lazyload.NewFileLoader(
	repositoryRoot,
	lazyload.WithMaxBytes(1<<20),
)

body, err := loader.Resolve(ctx, lazyload.Ref{
	ID:   "src/main.go",
	Kind: lazyload.KindFile,
})
```

`FileLoader` rejects absolute paths, `..`, backslashes, symlink escapes,
non-regular files, and oversized files. Treat reference IDs as untrusted input
and preserve those boundaries in custom loaders.

The normal integration pattern is:

1. expose references rather than content in the prompt;
2. expose a `load_content` tool;
3. validate its reference;
4. resolve through a `lazyload.Loader`;
5. return only the requested content to the model.

## 15. Custom stages

```go
type tenantStage struct{}

func (tenantStage) Name() string { return "tenant_context" }

func (tenantStage) Process(
	ctx context.Context,
	req *pipeline.Request,
) (*pipeline.Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req.SetMeta("tenant.id", "tenant-123")
	return req, nil
}
```

Add it with `config.WithStage(tenantStage{})`. It runs after built-in
compression and before cache alignment.

A returned error becomes `*pipeline.StageError`. A panic from a caller-owned
stage propagates. If the stage calls unreliable dependencies, implement its
own recovery and fail-open policy.

Compose `pipeline.New` or `pipeline.NewWithRouter` directly only when you
intentionally need a different order and accept responsibility for cache
alignment correctness.

## 16. Metrics and degradation observability

All metrics are optional:

```go
rec := metrics.NewInMemory()
kit := tokipe.New(client, config.WithMetrics(rec))

snapshot := rec.Snapshot()
```

`metrics.InMemory` records counters. `metrics.NewObservability()` additionally
records histograms, gauges, and degradation events:

```go
rec := metrics.NewObservability()
kit := tokipe.New(client, config.WithMetrics(rec))

degradations := rec.Degradations()
latencyByStage := rec.SummaryBy(metrics.StageLatency, "stage")
```

Production systems can implement the minimal counter interfaces plus any of
the optional interfaces:

```go
type Recorder interface {
	Counter(name string) Counter
}

type Counter interface {
	Inc(labels map[string]string)
}

type HistogramRecorder interface {
	Histogram(name string) Histogram
}

type GaugeRecorder interface {
	Gauge(name string) Gauge
}

type DegradationReporter interface {
	Degraded(metrics.Degradation)
}
```

Recorder panics and nil counters are contained. Diagnostics never become a
hard dependency.

`Degradation` contains a stable stage and reason plus variable error/detail
data. Keep error strings out of metric labels. Important signals include
`metrics.StageLatency`, `metrics.StageDegraded`,
`history.MetricTokensAfter`, cache hit/miss rates, and preprocess short
circuits.

For OpenTelemetry, use the separate `metrics/otel` module:

```go
rec := otel.New(meterProvider.Meter("myservice"),
	otel.WithDegradationHandler(func(d metrics.Degradation) {
		slog.Warn("tokipe degraded",
			"stage", d.Stage, "reason", d.Reason, "err", d.Err)
	}),
)
```

The adapter pins an OpenTelemetry version compatible with Go 1.23. See
`examples/observability`, including `-break`, for a complete operational view.

## 17. Streaming responses

`Pipeline.RunStream` performs the same preparation as `Run`: every enabled
stage runs in the same order, preprocess can short-circuit, and routing selects
the same final client.

```go
seq, err := kit.RunStream(ctx, req)
if err != nil {
	return err // stage, routing preparation, or pre-stream provider failure
}

for delta, streamErr := range seq {
	if delta.Text != "" {
		render(delta.Text)
	}
	if delta.Usage != nil {
		recordUsage(*delta.Usage)
	}
	if streamErr != nil {
		return streamErr
	}
}
```

`Delta.Text` is an increment, not accumulated content. `ModelUsed` is available
on each delta. `Usage` is non-nil only when the provider reports final
accounting.

Streaming has three paths:

| Final client/result | Behaviour |
|---|---|
| Implements `pipeline.StreamingClient` | Produces true incremental deltas |
| Implements only `ModelClient` | `Send` runs once and yields one delta |
| Preprocess short circuit | Yields one delta without reaching the client |

The in-tree Anthropic client implements true incremental SSE streaming.
`cli.Client` implements streaming too: it is incremental when a matching
`Config.StreamParse` is configured and otherwise deliberately falls back to
one completed delta. `providers/mock.NewStream` is available for tests.

A streaming provider adds this optional interface without changing
`ModelClient`:

```go
type StreamingClient interface {
	ModelClient
	SendStream(
		context.Context,
		*pipeline.Request,
	) (iter.Seq2[pipeline.Delta, error], error)
}
```

The direct error return means streaming never started. A mid-stream failure is
yielded through the sequence, possibly after partial content. To accumulate a
stream while preserving partial output:

```go
resp, err := pipeline.Collect(seq)
if err != nil {
	// resp.Content contains everything received before the failure.
}
```

Provider implementations own iterator cleanup. The in-tree CLI adapter
terminates its subprocess tree when the consumer stops early; HTTP providers
close response bodies. Custom streaming providers should implement the same
contract. A cancellable context is still required for request deadlines and
application-driven cancellation.

## 18. Error handling

```go
resp, err := kit.Run(ctx, req)
if err != nil {
	var stageErr *pipeline.StageError
	switch {
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		// Request lifecycle ended.
	case errors.As(err, &stageErr):
		// A caller-owned stage failed or produced invalid short-circuit data.
	default:
		// Final model/backend call failed.
	}
}
```

Built-in optimizations fail open:

| Failure | Behaviour |
|---|---|
| Preprocess error/panic | Try later rules, then model |
| Cache get error/panic | Treat as miss and execute |
| Tool executor error/panic | Leave that result unresolved |
| Cache set error/panic | Return the fresh result |
| Embedder/store error/panic | Continue without retrieval |
| Compressor error/panic | Keep original chunk |
| Router error/panic | Use default client |
| Metrics error/panic | Ignore diagnostics failure |

Context cancellation and final model failures do not fail open because the
application cannot produce the requested answer safely.

## 19. Concurrency and lifecycle

Built-in shared state is designed for concurrent pipeline use:

- memory tool cache is synchronized;
- identical concurrent tool misses are coalesced;
- preprocess registry is synchronized;
- in-memory metrics are synchronized;
- heuristic router and file loader are immutable after construction.

Construct long-lived clients, caches, stores, and pipelines once, then reuse
them. Do not recreate a cache per request or no reuse is possible.

## 20. Security checklist

- Keep API keys in environment/secret management, never request metadata.
- Set CLI working directories and inherited environment deliberately.
- Cache only deterministic, non-sensitive operations with complete keys.
- Treat tool arguments, lazy-load references, and retrieved content as
  attacker-influenced.
- Do not mark dynamic or tenant-specific text as static.
- Apply authorization before retrieval and before returning cached values.
- Bound request timeouts, retrieval top-K, file sizes, and model output.
- Log provider errors safely; avoid logging full prompts by default.
- Use separate cache namespaces when tenants or environments must not share.

## 21. Production rollout

Recommended sequence:

1. Deploy pass-through mode and capture baseline latency, tokens, errors, and
   task-quality metrics.
2. Enable metrics and deterministic preprocess rules.
3. Add tool caching for one proven-safe tool at a time.
4. Add RAG, exact dedupe, and compression; evaluate answer quality and citation
   correctness.
5. Add history budgets and alert on over-budget/degradation signals.
6. Enable cache alignment and inspect real provider cache-read/write usage.
7. Add routing in shadow mode, then progressively shift simple traffic.
8. Enable streaming where user experience benefits from partial output.
9. Revisit TTLs, top-K, router weights, and budgets from production data.

Keep a kill switch for each option. Because configuration is compositional, a
problematic optimization can be removed without redesigning the request path.

## 22. Troubleshooting

### “My RAG stage does nothing”

Set `NeedsRetrieval: true`, confirm both embedder and store were passed to
`WithRAG`, and inspect RAG metrics. Retrieval is request-level opt-in.

### “Tool calls execute every time”

Reuse the same cache instance, confirm arguments are deterministic and
hashable, choose a positive TTL if expiry is desired, and ensure the executor
returns successfully.

### “No Anthropic cache reads”

Mark invariant messages `Static: true`, enable cache alignment, ensure the
prefix bytes remain identical, and verify requests are large enough and
repeated according to the provider's caching rules.

### “The CLI provider is not found”

Install the CLI, authenticate it, and make sure its executable is on `PATH`.
For Codex, use a trusted Git working directory. Run the example in dry mode
before consuming quota.

### “Routing always picks one model”

Run `examples/local-routing`, inspect prompt shapes and chunk counts, then tune
tier thresholds or signal weights. A policy appropriate for long code may be
wrong for chunk-heavy synthesis.

### “History stays over budget”

Set `TurnType`, confirm the selected policy has a positive budget, and inspect
`history.tokens_before`, `history.tokens_after`, and degradation events. Static
messages, configured head/tail retention, the newest message, and at least one
retrieved chunk are protected.

### “Dedupe removed evidence I needed”

Use the default threshold of `1.0`. Any lower value is explicitly lossy.
Re-evaluate whether lexical near-deduplication is appropriate for policy,
generated, or repetitive content.

### “A custom stage panic crashes the caller”

That is the documented contract for caller-owned stages. Recover inside
`Process` if the stage should fail open, or return a normal error to receive a
`pipeline.StageError`.

## 23. Examples and verification

```bash
go run ./examples/rag-chatbot
go run ./examples/local-routing
go run ./examples/coding-agent
go run ./examples/cli-provider
go run ./examples/streaming
go run ./examples/observability
go run ./benchmarks
```

Repository verification:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=0 go build ./...

(cd stores/pgvector && GOTOOLCHAIN=local go test -race -count=1 ./...)
(cd toolcache/redis && GOTOOLCHAIN=local go test -race -count=1 ./...)
(cd metrics/otel && GOTOOLCHAIN=local go test -race -count=1 ./...)
```

Credential-gated checks:

```bash
TOKIPE_CLI_LIVE=1 go test -run TestLiveCLIs -v ./providers/cli/
(cd stores/pgvector && \
  TOKIPE_PGVECTOR_DSN='postgres://…' go test -run Integration -v ./...)
```

See [README.md](README.md) for the short path and
[docs/system-summary.html](docs/system-summary.html) for the visual system
overview.
