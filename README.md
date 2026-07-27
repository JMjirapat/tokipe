# tokipe

`tokipe` is a Go library for reducing the input-token cost and latency of
LLM-powered applications. It runs an ordered, opt-in pipeline before the final
model call:

```text
request
  → deterministic preprocess
  → tool-result cache
  → retrieval (RAG)
  → chunk dedupe
  → safe compression
  → history budget
  → custom stages
  → prompt-cache alignment
  → model routing
  → model send / stream
  → response or deltas
```

It is a library, not a hosted service. The core module uses only the Go
standard library, keeps no global state, and works with API or CLI model
backends.

## Start here

- [Slide deck — the system and how to use it](docs/presentation.html)
- [Full user manual](MANUAL.md)
- [High-level system summary](docs/system-summary.html)
- [Runnable examples](examples)
- [Delivery 1 evidence](docs/DELIVERY-1.md)
- [Delivery 2 evidence and current QA status](docs/DELIVERY-2.md)

Requirements: Go 1.23 or newer.

```bash
go get github.com/JMjirapat/tokipe
```

Published at `v1.0.0`. The optional backends are separate modules, so you pull
only the dependencies you actually use:

```bash
go get github.com/JMjirapat/tokipe/stores/pgvector   # pgvector retrieval
go get github.com/JMjirapat/tokipe/toolcache/redis   # shared tool-result cache
go get github.com/JMjirapat/tokipe/metrics/otel      # OpenTelemetry metrics
```

> `go get <path>@v1.0.0` on a nested module can report *"module
> github.com/JMjirapat/tokipe v1.0.0 found, but does not contain package …"* when
> the root module is already in your module cache — `go get` matches the shorter
> prefix and stops. Drop the `@v1.0.0`, or just import the package and run
> `go mod tidy`; both resolve correctly.

## 60-second quick start

This example uses the included mock model, so it needs no key or network:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/JMjirapat/tokipe"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/providers/mock"
)

func main() {
	client := mock.New("demo-model", "Hello from tokipe")
	kit := tokipe.New(client) // no options = pass-through

	resp, err := kit.Run(context.Background(), &pipeline.Request{
		Query: "Say hello",
		Messages: []pipeline.Message{
			{Role: "system", Content: "Answer briefly.", Static: true},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.Content)
}
```

For a useful production pipeline, enable only the capabilities you need:

```go
kit := tokipe.New(defaultClient,
	config.WithMetrics(recorder),
	config.WithPreprocess(rules...),
	config.WithToolCache(toolcache.NewMemoryCache(), executeTool, 30*time.Minute),
	config.WithRAG(embedder, vectorStore, 5),
	config.WithChunkDedupe(),
	config.WithDefaultCompression(),
	config.WithHistoryBudget(budget.DefaultPolicy(), nil),
	config.WithCacheAlignment(),
	config.WithRouter(router.NewHeuristicRouter(
		router.Tier{Client: cheapClient, MaxComplexity: 0.35},
		router.Tier{Client: strongClient, MaxComplexity: 1.00},
	)),
)

resp, err := kit.Run(ctx, &pipeline.Request{
	Query:          "Why did the deployment fail?",
	Messages:       history,
	NeedsRetrieval: true,
})
```

Every option is independent. `tokipe.New(client)` is a valid pass-through
baseline and is the right starting point for measuring savings.

## Choose a model backend

### Anthropic API

```go
client, err := anthropic.New(anthropic.Config{
	APIKey: os.Getenv("ANTHROPIC_API_KEY"),
	Model:  anthropic.DefaultModel,
})
```

The Anthropic adapter uses `net/http`, reports token/cache usage, and translates
tokipe cache breakpoints into Anthropic `cache_control` blocks. It implements
true incremental streaming over the Messages API's server-sent events.

### Existing CLI subscription

```go
client, err := cli.New(cli.ClaudePreset(workDir))
// Also available: cli.CodexPreset(workDir)
//                 cli.OpenCodePreset(workDir, "provider/model")
```

The CLI adapter launches the executable directly without a shell. It needs no
API key, but the CLI must already be installed and authenticated. CLI backends
cannot transmit explicit provider cache breakpoints; all other pipeline
optimizations still apply. `cli.Client` supports incremental stdout parsing
when `Config.StreamParse` is configured; the standard presets otherwise use
the safe one-delta fallback through `RunStream`.

### OpenAI-compatible servers

```go
client, err := openai.New(openai.Config{
	APIKey:  os.Getenv("OPENAI_API_KEY"),
	BaseURL: "https://api.openai.com/v1", // or any compatible server
	Model:   "gpt-4o-mini",
})
```

One adapter covers OpenAI, Ollama, vLLM, llama.cpp, Groq, Together, OpenRouter,
LM Studio and Azure OpenAI — what differs between them is a base URL, a model
name and a header, none of which needs a separate Go type. `APIKey` is optional,
because local servers do not use one. Streaming is supported.

`CacheBreakpoints` are advisory here and are not transmitted: these servers have
no `cache_control` field, and those that cache do it automatically on the
longest matching prefix. Cache alignment still helps by keeping that prefix
stable — it just cannot be told about it.

### Your own provider

Implement two methods:

```go
type ModelClient interface {
	Send(context.Context, *pipeline.Request) (*pipeline.Response, error)
	Name() string
}
```

## What each capability does

| Capability | Enable with | Effect |
|---|---|---|
| Preprocess | `config.WithPreprocess` | Answers deterministic requests without an LLM |
| Tool cache | `config.WithToolCache` | Reuses identical tool results and coalesces concurrent misses |
| RAG | `config.WithRAG` | Embeds the query and retrieves top-K chunks |
| Compression | `config.WithDefaultCompression` | Minifies JSON, collapses prose whitespace, strips Go comments via `go/ast` |
| Chunk dedupe | `config.WithChunkDedupe` | Drops exact normalized duplicates by default; lower similarity thresholds are explicit lossy opt-in |
| Cache alignment | `config.WithCacheAlignment` | Keeps static prompt content first and emits safe breakpoints |
| Routing | `config.WithRouter` | Selects the cheapest suitable model after prompt shaping |
| Metrics | `config.WithMetrics` | Provider-neutral counters, plus optional histograms, gauges and degradation events; no-op by default |
| Custom stage | `config.WithStage` | Adds caller-owned request processing before alignment |
| Lazy loading | caller-managed `lazyload.Loader` | Resolves protected file/content references on demand |
| History budget | `config.WithHistoryBudget` | Trims the conversation to fit a per-turn-type token budget |
| Budget policy | caller-managed `budget.Policy` | Classifies turns and supplies recommended token budgets |
| Streaming | `kit.RunStream` instead of `kit.Run` | Delivers the answer incrementally; every stage still runs first |

The built-in order is a correctness rule: retrieval and compression must finish
before history budgeting and cache alignment. Custom stages added through
`config.WithStage` also run before alignment. Routing runs last so it scores the
final prompt shape.

## Request and result

The common request fields are:

```go
req := &pipeline.Request{
	Query:          "Current user question",
	Messages:       history,           // oldest first
	ToolCalls:      pendingToolCalls,  // optional
	NeedsRetrieval: true,              // RAG is request-level opt-in
	TurnType:       pipeline.TurnNewQuestion,
}
```

Mark stable system instructions and tool definitions with `Static: true`.
Never mark per-turn evidence or retrieved content static.

The result reports the answer, selected model, short-circuit status, and usage:

```go
fmt.Println(resp.Content)
fmt.Println(resp.ModelUsed)
fmt.Println(resp.ShortCircuited)
fmt.Println(resp.Usage.InputTokens, resp.Usage.CacheReadTokens)
```

### Streaming responses

`RunStream` uses the same stages, short-circuit logic, and router as `Run`:

```go
seq, err := kit.RunStream(ctx, req)
if err != nil {
	return err // failure before streaming starts
}

for delta, streamErr := range seq {
	if streamErr != nil {
		return streamErr // may arrive after partial text
	}
	fmt.Print(delta.Text)
}
```

Streaming is an optional provider capability. A regular `ModelClient` still
works through `RunStream`, yielding its completed response as one delta.
Preprocess short circuits also yield one delta. Implement
`pipeline.StreamingClient` for true incremental provider output.

Errors split by timing: returned from `RunStream` when nothing was produced,
yielded inside the sequence when text had already arrived. A partial answer is
usually still worth showing. `pipeline.Collect(seq)` accumulates a stream into
an ordinary `*Response`, and `delta.Usage` is non-nil only on the final delta.

### Backend granularity

Not all backends stream at the same resolution, and no adapter can improve on
what a backend emits:

| Backend | Granularity | Usage reported |
|---|---|---|
| `providers/anthropic` | per text delta (token-level) | yes, on the final delta |
| `cli.ClaudeStreamPreset` | per assistant message — measured as 1 delta | yes |
| `cli.CodexStreamPreset` | per agent message — measured as 1 delta | yes |
| `cli.OpenCodeStreamPreset` | per line | no |

If your UI needs per-token updates, use the API backend. CLI backends need an
explicit `Config.StreamParse`; the stream presets set one. Without it,
`SendStream` buffers and yields a single delta rather than guessing which of a
CLI's lines are answer text and which are protocol noise.


## Keeping context bounded

A long agent loop's dominant cost is history that never stops growing.
`WithHistoryBudget` enforces the budget `budget.Policy` already described:

```go
kit := tokipe.New(client,
	config.WithHistoryBudget(budget.DefaultPolicy(), nil), // nil = char estimate
	config.WithCacheAlignment(),
)
req.TurnType = budget.Classify(req) // budgets vary by turn type
```

Measured over 100 turns (`go run ./benchmarks`, long-loop section): 195,691 →
88,930 billed tokens, peak request 1,192 against a 1,200 limit.

What it will not do, by design:

- **It never touches static content.** That is the prefix cache alignment anchors
  to; moving one byte of it forfeits every cache hit, which costs more than the
  tokens trimming saved.
- **It never drops the newest message.** That is the question being asked.
- **It trims the middle, not the oldest.** Dropping from the front changes the
  first non-static bytes every turn, so providers that cache on longest-common-
  prefix never get a hit. Head and tail survive; `WithRetention` sets how much.

If messages are protected but retrieved chunks are present, it can remove
lower-ranked chunks while retaining at least one. If the request still cannot
fit without breaking its retention rules, it reports `history.over_budget`
rather than trimming something it promised not to.

For an exact count instead of an estimate, pass a provider-backed counter —
`anthropic.NewTokenCounter(client)` calls `/v1/messages/count_tokens` and
memoises per string, since a trimming pass re-measures unchanged history every
turn. It costs a round trip; the estimator costs nothing and is systematically
wrong on code and CJK. Use the exact counter when trimming against a hard
context limit, the estimator when trimming for cost.

## Compression and duplicate safety

`WithDefaultCompression` enables conservative JSON minification and prose
whitespace normalization. To compress complete Go files, register the AST-aware
compressor explicitly:

```go
config.WithCompression(
	compress.NewJSONCompressor(),
	compress.NewCodeCompressor(), // comments removed; bodies retained by default
	compress.NewTextCompressor(), // catch-all must stay last
)
```

`CodeCompressor` refuses files containing compiler directives or `import "C"`,
returns the original when output is not smaller or no longer parses, and keeps
function bodies unless `compress.WithBodyElision()` is explicitly selected.

Chunk dedupe is separate from compression:

```go
config.WithChunkDedupe() // safe default: exact normalized word sequence
```

Setting `compress.WithDedupeThreshold` below `1.0` opts into lossy lexical
near-deduplication. Use that only when duplicate-token savings are worth the
risk of discarding subtly different evidence.

## Failure contract

Built-in optimizations fail open:

- cache failures become misses;
- retrieval failures continue without chunks;
- compressor failures preserve the original content;
- broken preprocess rules are skipped;
- router failures use the default model;
- metrics failures never break a turn.

`Run` returns an error when the context ends, the final model call fails, or a
custom `Stage` returns an error/malformed short-circuit value. A panic from a
custom stage is caller-owned and propagates; recover inside that stage if that
is not desired.

## Seeing what degraded

Fail-open keeps a turn alive when an optimization breaks. That is right for
availability and blind for operations: a dead cache backend degrades every
request and, on its own, nothing says so.

Implement `metrics.DegradationReporter` — an optional interface on `Recorder` —
and each fail-open event reports itself:

```go
rec := metrics.DegradeFunc(baseRecorder, func(d metrics.Degradation) {
	slog.Warn("tokipe degraded",
		"stage", d.Stage, "reason", d.Reason, "detail", d.Detail, "err", d.Err)
})
kit := tokipe.New(client, config.WithMetrics(rec), ...)
```

`Reason` is short and stable, so it groups in a dashboard and works in an alert
rule; `Err` and `Detail` carry the varying part. The library picks no logger,
format or destination — it hands over a struct.

Two more optional interfaces, ignored by recorders that do not implement them:

| Interface | Gives you |
|---|---|
| `metrics.HistogramRecorder` | Per-stage latency (`metrics.StageLatency`) and context size after trimming |
| `metrics.GaugeRecorder` | Values you derive yourself, e.g. cache hit rate |

`metrics.NewObservability()` implements all three in memory for tests and
dashboards. For production, `metrics/otel` is a nested module adapting the lot
to OpenTelemetry:

```go
rec := otel.New(meterProvider.Meter("myservice"),
	otel.WithDegradationHandler(func(d metrics.Degradation) { /* log it */ }))
```

See it end to end, including the case where everything is broken:

```bash
go run ./examples/observability
go run ./examples/observability -break
```

## Run the examples

All examples except live CLI mode are safe to run without credentials:

```bash
go run ./examples/rag-chatbot
go run ./examples/local-routing
go run ./examples/coding-agent
go run ./examples/cli-provider
go run ./examples/streaming
go run ./examples/observability
go run ./benchmarks
```

Live CLI mode consumes subscription quota:

```bash
go run ./examples/cli-provider -live -cli claude
go run ./examples/cli-provider -live -cli codex
go run ./examples/cli-provider -live -cli opencode -model provider/model
go run ./examples/streaming -cli claude
```

## Verify the checkout

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=0 go build ./...
```

A healthy checkout reports:

<!-- inventory:test-funcs=394 -->
<!-- inventory:packages=29 -->

| Quantity | Value | Command |
|---|---|---|
| Test/Example functions | **394** | `grep -rhoE '^func (Test\|Example)[A-Za-z0-9_]*' --include='*_test.go' . \| wc -l` |
| Packages in the root module | **29** | `go list ./... \| wc -l` |
| Failures | **0** | `go test -race -count=1 ./...` |

Those numbers are enforced, not decorative: `inventory_test.go` reads the markers
above and fails the build when they drift. Adding a test means updating them in
the same commit — which is the point, since two separate QA rounds were failed by
a count that had quietly gone stale.

The benchmark currently reports a 57.1% reduction on its documented synthetic
workload. Treat it as comparative evidence, not a forecast for every workload.
Measure your own traffic before setting production targets.

## API stability

The v1 core interfaces remain frozen:

- `pipeline.Stage`
- `pipeline.ModelClient`
- `stores.Embedder` and `stores.VectorStore`
- `toolcache.Cache`

Streaming is additive through the optional `pipeline.StreamingClient`
interface, so existing providers do not need to change. History budgeting,
OpenAI-compatible providers, observability extensions, code compression, and
chunk dedupe are additive post-v1 capabilities covered by Delivery 2.

## Next

Read [MANUAL.md](MANUAL.md) for configuration recipes, extension interfaces,
Redis and pgvector adapters, observability, security guidance, troubleshooting,
and production rollout advice.
