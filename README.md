# agentkit

`agentkit` is a Go library for reducing the input-token cost and latency of
LLM-powered applications. It runs an ordered, opt-in pipeline before the final
model call:

```text
request
  → deterministic preprocess
  → tool-result cache
  → retrieval (RAG)
  → safe compression
  → custom stages
  → prompt-cache alignment
  → model routing
  → response
```

It is a library, not a hosted service. The core module uses only the Go
standard library, keeps no global state, and works with API or CLI model
backends.

## Start here

- [Full user manual](MANUAL.md)
- [High-level system summary](docs/system-summary.html)
- [Runnable examples](examples)
- [Delivery evidence](docs/DELIVERY-1.md)

Requirements: Go 1.23 or newer.

> **Module path:** this source tree currently declares `module agentkit` and
> has no published remote module URL. Code inside this repository imports it
> directly. An external application can use a temporary `replace` directive
> until the project is published:
>
> ```go
> require agentkit v0.0.0
> replace agentkit => ../tokipe
> ```

## 60-second quick start

This example uses the included mock model, so it needs no key or network:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"agentkit"
	"agentkit/pipeline"
	"agentkit/providers/mock"
)

func main() {
	client := mock.New("demo-model", "Hello from agentkit")
	kit := agentkit.New(client) // no options = pass-through

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
kit := agentkit.New(defaultClient,
	config.WithMetrics(recorder),
	config.WithPreprocess(rules...),
	config.WithToolCache(toolcache.NewMemoryCache(), executeTool, 30*time.Minute),
	config.WithRAG(embedder, vectorStore, 5),
	config.WithDefaultCompression(),
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

Every option is independent. `agentkit.New(client)` is a valid pass-through
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
agentkit cache breakpoints into Anthropic `cache_control` blocks. It implements
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
| Compression | `config.WithDefaultCompression` | Minifies JSON and collapses redundant prose whitespace |
| Cache alignment | `config.WithCacheAlignment` | Keeps static prompt content first and emits safe breakpoints |
| Routing | `config.WithRouter` | Selects the cheapest suitable model after prompt shaping |
| Metrics | `config.WithMetrics` | Reports provider-neutral counters; no-op by default |
| Custom stage | `config.WithStage` | Adds caller-owned request processing before alignment |
| Lazy loading | caller-managed `lazyload.Loader` | Resolves protected file/content references on demand |
| History budget | `config.WithHistoryBudget` | Trims the conversation to fit a per-turn-type token budget |
| Budget policy | caller-managed `budget.Policy` | Classifies turns and supplies recommended token budgets |
| Streaming | `kit.RunStream` instead of `kit.Run` | Delivers the answer incrementally; every stage still runs first |

The built-in order is a correctness rule: retrieval and compression must finish
before cache alignment. Custom stages added through `config.WithStage` also run
before alignment. Routing runs last so it scores the final prompt shape.

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

## Streaming

`RunStream` is `Run` with an incremental result. Every stage runs first, in the
same order — streaming is a property of the final model call only, which is why
no stage changed to support it.

```go
seq, err := kit.RunStream(ctx, req)
if err != nil {
	return err // a stage failed, or the call was rejected before any output
}
for delta, err := range seq {
	if err != nil {
		return err // mid-stream failure; text already yielded is still valid
	}
	fmt.Print(delta.Text)
	if delta.Usage != nil {
		log.Printf("usage: %+v", *delta.Usage) // final delta only
	}
}
```

Use `pipeline.Collect(seq)` to accumulate a stream into an ordinary `*Response`.

Two properties remove the branches you would otherwise write:

- **A client that cannot stream still works.** Implement the optional
  `pipeline.StreamingClient` to stream for real; without it `RunStream` calls
  `Send` and yields one delta. Callers never ask which kind they hold.
- **A short-circuited turn yields exactly one delta.** A preprocess rule
  answering without a model looks like any other stream.

Errors split by timing: returned from `RunStream` if nothing was produced,
yielded inside the sequence if the failure came after some text. A partial
answer is usually still worth showing.

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
kit := agentkit.New(client,
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

If it cannot fit the budget without breaking those rules, it leaves the request
alone and reports `history.over_budget` rather than trimming something it
promised not to.

For an exact count instead of an estimate, pass a provider-backed counter —
`anthropic.NewTokenCounter(client)` calls `/v1/messages/count_tokens` and
memoises per string, since a trimming pass re-measures unchanged history every
turn. It costs a round trip; the estimator costs nothing and is systematically
wrong on code and CJK. Use the exact counter when trimming against a hard
context limit, the estimator when trimming for cost.

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

## Run the examples

All examples except live CLI mode are safe to run without credentials:

```bash
go run ./examples/rag-chatbot
go run ./examples/local-routing
go run ./examples/coding-agent
go run ./examples/cli-provider
go run ./examples/streaming
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

<!-- inventory:test-funcs=287 -->
<!-- inventory:packages=27 -->

| Quantity | Value | Command |
|---|---|---|
| Test/Example functions | **287** | `grep -rhoE '^func (Test\|Example)[A-Za-z0-9_]*' --include='*_test.go' . \| wc -l` |
| Packages in the root module | **27** | `go list ./... \| wc -l` |
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
interface, so existing providers do not need to change.

## Next

Read [MANUAL.md](MANUAL.md) for configuration recipes, extension interfaces,
Redis and pgvector adapters, observability, security guidance, troubleshooting,
and production rollout advice.
