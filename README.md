# agentkit

A Go library that cuts the input-token cost of LLM calls. It sits between your
code and your model provider as an ordered pipeline of optimizations: answer
deterministically when you can, reuse tool results, retrieve and compress
context, keep the prompt prefix stable so provider-side caching actually hits,
and route cheap work to a cheap model.

It is a library, not a service. No hosted component, no UI, no ML model, no
required provider.

```bash
go get agentkit
```

## Quickstart

```go
client, _ := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY")})

kit := agentkit.New(client,
    config.WithPreprocess(myRules...),                 // skip the LLM entirely
    config.WithToolCache(toolcache.NewMemoryCache(), myExecutor, time.Hour),
    config.WithRAG(embedder, store, 5),                // retrieve
    config.WithDefaultCompression(),                   // shrink what you retrieved
    config.WithCacheAlignment(),                       // keep the prefix cacheable
    config.WithRouter(router.NewHeuristicRouter(
        router.Tier{Client: cheap,  MaxComplexity: 0.35},
        router.Tier{Client: strong, MaxComplexity: 1.0},
    )),
)

resp, err := kit.Run(ctx, &pipeline.Request{
    Query:          "Why did the deploy fail?",
    Messages:       history,
    NeedsRetrieval: true,
})
```

Every option is opt-in. `agentkit.New(client)` with no options is a
pass-through pipeline — useful as the baseline you measure against.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                      Caller (any Go service)                     │
└──────────────────────────────┬───────────────────────────────────┘
                               │  agentkit.Request
                               ▼
┌──────────────────────────────────────────────────────────────────┐
│                         agentkit.Pipeline                        │
│  ┌──────────┐ ┌───────────┐ ┌─────┐ ┌──────────┐ ┌────────────┐  │
│  │Preprocess│→│ ToolCache │→│ RAG │→│ Compress │→│ LazyLoad   │→ │
│  └──────────┘ └───────────┘ └─────┘ └──────────┘ └────────────┘  │
│  ┌────────────┐ ┌────────┐ ┌────────┐                            │
│ →│ CacheAlign │→│ Router │→│ Budget │→ (call ModelClient.Send)    │
│  └────────────┘ └────────┘ └────────┘                            │
└──────────────────────────────┬───────────────────────────────────┘
                               │  agentkit.Response
                               ▼
                      Caller receives result
```

Each box is a `Stage`: one method, `Process(ctx, *Request) (*Request, error)`.
There is no hidden control flow — stages run in order, and `PreprocessStage`
is the only one that may short-circuit.

### The ordering is not a preference

Retrieval must run before compression, and both must run before cache
alignment. A cache breakpoint anchored after content that changes every turn
invalidates the cached prefix on every single call — which is worse than not
caching at all. `config` owns that ordering so callers cannot get it wrong;
`config.WithStage` appends custom stages *before* alignment for the same
reason. If you need a different order, compose `pipeline.New` by hand and own
the decision.

## Design rules

**Fail-open, always.** No optimization may break a turn. If compression errors,
the chunk goes through uncompressed. If the cache backend is down, it is a
miss. If the embedding service times out, the turn proceeds without retrieval.
The only errors `Run` returns come from the model call itself.

**No global state.** Every stage takes its dependencies through its
constructor. Nothing is a package-level singleton.

**Metrics are optional.** Stages take a `metrics.Recorder`; the default is a
no-op. Nothing requires a metrics backend.

**The core is stdlib-only.** Verified in CI by `go list -deps`. Adapters that
need a driver live in their own nested modules:

| Module | Dependency |
|---|---|
| `stores/pgvector` | `jackc/pgx/v5` |
| `toolcache/redis` | `redis/go-redis/v9` |

`providers/anthropic` is built on `net/http`, so it stays in the core module.

## Packages

| Package | Purpose |
|---|---|
| `pipeline` | `Request`, `Response`, `Stage`, `Pipeline`, `ModelClient`, `Router` |
| `config` | Functional options; owns the stage ordering |
| `preprocess` | Resolve deterministic requests without an LLM |
| `toolcache` | Deterministic `(tool, args)` hashing; memory + Redis backends |
| `rag` | Retrieval, fail-open |
| `compress` | JSON and prose compression (`code` is a Phase 2+ stub) |
| `cache` | Prompt-cache breakpoint placement |
| `lazyload` | Opt-in reference resolution with path-traversal protection |
| `router` | Complexity-scored tier selection |
| `budget` | Per-turn-type token budgets and turn classification |
| `metrics` | Provider-agnostic counters, no-op by default |
| `stores`, `providers` | Retrieval and model-client interfaces |
| `providers/anthropic` | Anthropic Messages API over `net/http` (needs an API key) |
| `providers/cli` | Any CLI agent as a backend — `claude`, `codex`, `opencode` (needs no API key) |

## Backends

`ModelClient` is the only seam a provider touches. Nothing in the core imports
a provider package, so adding one never means changing a stage.

Two ship in-tree:

```go
// API key, full control over cache breakpoints.
client, _ := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY")})

// No API key: runs whatever CLI your subscription already authenticates.
client, _ := cli.New(cli.ClaudePreset(""))   // or CodexPreset / OpenCodePreset
```

The CLI backend exists because API credit and a subscription are different
products. If you have a Claude Pro, Codex, or OpenCode plan and no API key, the
CLI is a real backend, not a workaround.

**What the CLI backend cannot do:** a CLI builds its own prompt scaffold and
exposes no `cache_control` hooks, so `Request.CacheBreakpoints` are ignored
rather than mistranslated, and the `Usage` it reports describes the CLI's
prompt, not yours. Every other optimization still applies — preprocess rules
still skip calls entirely, the tool cache still prevents re-execution,
compression still shrinks what you send, and routing still picks a backend.

`CacheBreakpoints` is the one place a provider's shape shows through into a
core type: explicit, caller-placed breakpoints are an Anthropic idea, and most
providers cache automatically instead. It is advisory — a backend that cannot
use it ignores it, and `cache.Aligner`'s reordering still pays off, because a
stable prefix is what automatic caching keys on too.

## Examples

All run against mocks — no API key, no network, no database.

```bash
go run ./examples/rag-chatbot     # retrieval → compression → cacheable prefix
go run ./examples/local-routing   # cost-aware split across two model tiers
go run ./examples/coding-agent    # every stage wired into one agent loop
go run ./benchmarks               # measures billed input tokens, baseline vs agentkit
```

The benchmark is the acceptance test for the headline claim. On a synthetic
12-turn workload (growing history, repeated tool calls, retrieval, and some
deterministic turns) it currently reports:

```
billed input tokens  : 5005 → 2148
reduction            : 57.1%   (target ≥ 30%)
turns short-circuited: 3/12
tool executions      : 6 → 4
```

Read `benchmarks/main.go` before quoting that number — the accounting
assumptions (4 chars/token, cache reads at 0.1×, writes at 1.25×) are
documented at the top of the file, and the baseline is a plausible naive
implementation rather than a strawman.

## Stability

Once `v1.0.0` is tagged, these interfaces are **frozen**; changing them
requires a new major version:

- `pipeline.Stage`
- `pipeline.ModelClient` (aliased as `providers.ModelClient`)
- `stores.VectorStore`, `stores.Embedder`
- `toolcache.Cache`

Additive changes — new options, new stages, new fields on `Request` — are
minor releases.

## Development

```bash
go build ./... && go vet ./... && go test -race ./...
```

Go 1.23+. `CGO_ENABLED=0` builds clean; the `-race` pass needs cgo.

See [PLAN.md](PLAN.md) for phase status and the documented deviations from the
original spec, and [docs/spec.md](docs/spec.md) for the full requirements.

## License

Apache-2.0. See [LICENSE](LICENSE).
