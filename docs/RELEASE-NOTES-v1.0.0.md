tokipe is a Go library that cuts the input-token cost and latency of LLM
applications. It runs an ordered, opt-in pipeline before the final model call —
nothing is enabled unless you ask for it, and enabling nothing gives you a
pass-through.

```text
request → preprocess → tool-result cache → RAG → chunk dedupe → compression
        → history budget → cache alignment → routing → send / stream → response
```

## Measured, not claimed

| Workload | Billed input tokens | Reduction |
|---|---|---|
| 12-turn agent workload | 5005 → 2148 | **57.1%** |
| 100-turn loop with a 1200-token budget | 195691 → 88930 | **54.6%**, peak request 1192 |

Reproduce both with `go run ./benchmarks`. Both arms record identical content —
the comparison is worthless otherwise.

## Fail-open is the core guarantee

No optimization may cost you a turn. Every extension point you supply — rules,
compressors, routers, caches, counters, summarizers, metric sinks — runs inside
a recover boundary. If it panics or errors, the pipeline degrades to the
unoptimized request and continues.

This is verified, not asserted. `go run ./examples/observability -break` breaks
every dependency at once: **40/40 turns succeed**, with 80 degradations
reported. A separate machine-checked test enumerates every method tokipe calls
on caller-supplied code and fails if a new extension point is added without a
guard.

## What is in the box

**Pipeline stages** — deterministic preprocessing, tool-result caching with
singleflight, RAG retrieval, lexical chunk dedupe, AST-aware Go code
compression, history trimming against a token budget, prompt-cache alignment,
and heuristic model routing.

**Providers, all in the core module** — Anthropic Messages API (with
`cache_control` breakpoints and a token counter), any OpenAI-compatible server
(OpenAI, Ollama, vLLM, llama.cpp, Groq, Together, OpenRouter, LM Studio, Azure),
and **CLI-backed providers** that drive `claude`, `codex` or `opencode` through
the subscription you already have. No API key required for that last one.

**Streaming** — `RunStream` yields incremental deltas without changing the
frozen `ModelClient` interface.

**Observability** — latency histograms, degradation reporting, and an
OpenTelemetry adapter, so a pipeline that silently stopped optimizing is
visible instead of invisible.

## Zero dependencies in the core

The core module imports only the Go standard library. This is enforced by CI on
every push, not by convention. Optional backends are separate modules, so you
pay for only what you use:

```bash
go get github.com/JMjirapat/tokipe

go get github.com/JMjirapat/tokipe/stores/pgvector   # pgvector retrieval
go get github.com/JMjirapat/tokipe/toolcache/redis   # shared tool-result cache
go get github.com/JMjirapat/tokipe/metrics/otel      # OpenTelemetry metrics
```

Requires **Go 1.23+**. Builds with `CGO_ENABLED=0` on Linux, macOS and Windows.

## Quick start

No key, no network — the mock provider is included:

```go
kit := tokipe.New(mock.New("m", "an answer"), config.WithCacheAlignment())
resp, err := kit.Run(ctx, &pipeline.Request{Query: "hello"})
```

Six runnable examples ship in [`examples/`](examples), all of which run without
credentials.

## Verification

| Check | Result |
|---|---|
| Test suite | 518 tests, 29 packages, clean under `-race` |
| Coverage | 100% on the root package, router and the `internal/safe` recover boundary |
| Nested modules | build and pass on Go 1.23 with `GOTOOLCHAIN=local` |
| Zero third-party deps in core | enforced by CI |
| pgvector | integration tests run against a real `pgvector/pgvector:pg16` database, with a CI step that fails if they skip |
| Live CLI backends | `claude`, `codex` and `opencode` all stream |

This release cleared five QA rounds on Delivery 1 and four more on Delivery 2,
each performed against the code rather than against the documentation. Every
finding is recorded in [`docs/`](docs), including the ones that were failures of
verification rather than of code.

## Prompt caching, end to end

The acceptance test passes against a live Messages-API endpoint: turn 1 writes
the static prefix to the cache, turn 2 reads it back. That confirms
`cache_control` is emitted in a form a real server accepts, the prefix stays
byte-identical across turns, and the cache usage fields are parsed correctly.

The recorded run went through a gateway rather than `api.anthropic.com`
directly, so behaviour against Anthropic's own endpoint is not yet on record.
The test takes `ANTHROPIC_BASE_URL` and `ANTHROPIC_MODEL`, so you can point it
at whichever endpoint you actually use — see
[`providers/anthropic/README.md`](providers/anthropic/README.md).

## Compatibility

The public API is frozen at v1. `Request`, `Response`, `Stage`, `ModelClient`
and `Router` will not change shape within v1.x; new capability arrives through
optional interfaces, which is how streaming, histograms, gauges and degradation
reporting were added without breaking anything.

Metric names (`tokipe.stage_latency_ms`, `tokipe.stage_degraded`) are part of
that contract too — dashboards built on them will keep working.

---

Full documentation: [README](README.md) · [user manual](MANUAL.md) ·
[system summary](docs/system-summary.html) ·
[production readiness review](docs/PRODUCTION-READINESS.md)
