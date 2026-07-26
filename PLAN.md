# agentkit — implementation plan

Full requirements: [docs/spec.md](docs/spec.md). This file is the execution
state: what is decided, what is done, who is doing what.

## Decisions (spec §4 open questions, resolved with defaults)

| # | Question | Decision |
|---|---|---|
| 1 | Module path | **`github.com/JMjirapat/tokipe`.** Was the spec's `agentkit` placeholder through v1.0.0; renamed afterwards so the path matches the repository. The root package is still named `agentkit` — see below. |
| 2 | License | Apache-2.0 |
| 3 | `providers/anthropic` placement | **Stays in the core module.** It was built on raw `net/http`, so it adds no dependency and the zero-dep guarantee holds (verified by `go list -deps`). Only `stores/pgvector` (pgx) and `toolcache/redis` (go-redis) are nested modules. |
| 4 | Minimum Go | 1.23 (toolchain on this machine is go1.23.3) |

### Module path vs package name

The module is `github.com/JMjirapat/tokipe`; the root package is still
`package agentkit`. So an importer writes:

```go
import "github.com/JMjirapat/tokipe"

kit := agentkit.New(client)   // package name, not path
```

That is legal Go and not unusual — a repository and the library it publishes
need not share a name — but it does surprise people, because the convention is
for the last path element to match the package name. Renaming the package would
touch every example, doc and `agentkit.New` reference in the tree; it is a
separate decision, deliberately not bundled into the path rename.

## Documented deviations from the spec's contracts

1. **`ModelClient` and `Router` are declared in `pipeline`, not `providers`/`router`.**
   The spec has `providers.ModelClient` taking `*pipeline.Request` while
   `pipeline.Pipeline` holds a `ModelClient` field — that is an import cycle.
   Both interfaces live in `pipeline` and are re-exported as type aliases
   (`providers.ModelClient`, `router.Router`), so every signature in the spec
   still compiles verbatim for callers.
2. **`pipeline.Message` gains a `Static bool` field.** §2.4.6 requires the
   aligner to reorder "caller-marked" static content, but the spec's `Message`
   had no field to carry that mark. Additive, breaks nothing.
3. **`Pipeline.Run` checks `ctx.Err()` before each stage** (§2.5 requirement 3)
   and type-asserts the short-circuit value safely instead of a bare
   `.(*Response)` panic.

## Phase status

- [x] **Phase 0 — bootstrap.** `go.mod`, `pipeline/stage.go`, `providers/provider.go`,
      `providers/mock`, `stores/vectorstore.go`, `metrics/metrics.go`,
      `pipeline/pipeline_test.go`, CI. `go test -race ./...` green.
- [x] **Phase 1 — foundation stages.** `preprocess`, `toolcache`, `cache` (aligner).
- [x] **Phase 2 — compression, RAG, providers.** `compress`, `rag`, `lazyload`,
      `stores/mock`, `stores/pgvector`, `providers/anthropic`.
- [x] **Phase 3 — routing & budget.** `router`, `budget`, router wired into `Pipeline`
      via `pipeline.NewWithRouter`.
- [x] **Phase 4 — hardening.** `toolcache/redis`, metrics wired everywhere,
      concurrency stress test (480 concurrent runs over shared state, `-race`),
      all three `examples/`, README, benchmark.

### Acceptance criteria (spec §3.2)

| Criterion | Status |
|---|---|
| ≥30% input-token reduction on a representative workload | **57.1%** — `go run ./benchmarks`, enforced in CI |
| Measurable % of requests short-circuited by preprocess | 3/12 turns (25%) in the benchmark workload |
| `CacheReadTokens > 0` on turns after the first, real endpoint | **BLOCKED — see below.** Test written and skips cleanly without a key: `providers/anthropic/integration_test.go` |
| Routing split across two tiers with no per-request routing code | `examples/local-routing` |
| Core usable from two independent example programs with no shared non-agentkit code | `examples/rag-chatbot` and `examples/local-routing` |

### Blocked: real-endpoint prompt-caching verification

The one acceptance criterion that cannot be closed here needs **Anthropic API
credit**, which is a separate product from a Claude Pro subscription. A Pro (or
Max) plan authenticates the Claude apps and Claude Code; it does not issue an
`ANTHROPIC_API_KEY` for `api.anthropic.com/v1/messages`, and routing a
subscription session into a library's HTTP client is not a supported or
permitted substitute. A ChatGPT/Codex subscription is likewise not Anthropic
API access.

What this does and does not leave unverified:

- **Verified without a key** — that agentkit emits `cache_control: ephemeral`
  on the right content blocks, truncates to the four deepest breakpoints, and
  parses `cache_read_input_tokens` / `cache_creation_input_tokens` into
  `Response.Usage`. All covered by `client_test.go` against `httptest`.
- **Not verified** — that Anthropic's cache actually *hits* for our prefix
  layout end to end. That is a claim about the provider's behaviour meeting
  ours, and only a real call can settle it.

Closing it costs roughly a few US cents: the test makes two calls capped at 64
output tokens. It needs pay-as-you-go credit on console.anthropic.com, not a
subscription upgrade. Until someone with API credit runs it, treat the
prompt-caching benefit as **designed and unit-tested, not field-proven**, and
do not quote a cache-hit rate in any external material.

`providers/cli` does not close this gap and is not meant to. It removes the
key requirement for *running* agentkit, but a CLI exposes no `cache_control`
hooks, so it cannot verify our breakpoint placement. Its live test
(`AGENTKIT_CLI_LIVE=1`) has been run and passes against `claude` 2.1.x,
returning real content, model id, and usage.

### Known gaps carried into v1.1

- ~~`compress.CodeCompressor` is a deliberate stub.~~ **Closed in Phase 8.**
  Real `go/ast` implementation: strips comments, optionally elides bodies, and
  verifies its own output by re-parsing and comparing declaration counts.
- ~~`toolcache` has no stampede protection.~~ **Closed.** `toolcache/singleflight.go`
  coalesces concurrent identical calls; the stress test now asserts exactly 2
  executions for 2 distinct calls across 480 concurrent runs (was 6).
- ~~`stores/pgvector` has no real-database test in CI.~~ **Closed in Phase 8.**
  A dedicated CI job runs it against a `pgvector/pgvector:pg16` service, and
  fails if the tests skip.

Phases 5–8 landed after v1.0.0; see [docs/ROADMAP.md](docs/ROADMAP.md) for what
each added and what it taught. Still genuinely open: the Anthropic
prompt-caching verification above, and Bedrock (dropped from Phase 8 because
SigV4 needs the AWS SDK, which the stdlib-only core cannot take).

## Conventions every contributor (human or agent) follows

- **Fail-open is the law.** No optimization stage may make `Pipeline.Run` fail.
  A stage that cannot do its job returns `(req, nil)` unchanged.
- **No global mutable state.** Dependencies come in through constructors.
- **Core is stdlib-only.** Anything needing a third-party dep goes in its own
  nested module under the package it belongs to.
- **Metrics are optional.** Take a `metrics.Recorder`, run it through
  `metrics.Or(...)`, never require it.
- Tests ship in the same commit as the code. Table-driven where the logic is pure.
- `go build ./... && go vet ./... && go test -race ./...` must be green before
  a phase is called done.
