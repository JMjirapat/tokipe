# agentkit — Business Requirements, Technical Specification & Implementation Plan

**Document type:** Combined BRD + Tech Spec + Implementation Plan
**Purpose:** Handoff document for Claude Code to implement `agentkit` from scratch
**Status:** Draft v1.0
**Owner:** Jamie

---

## How to use this document (for Claude Code)

This document is self-contained. Read it top to bottom before writing any code.

- **Section 1 (Business Requirements)** explains *why* this exists and what "done" means from a business perspective. Read this to understand priorities when tradeoffs come up.
- **Section 2 (Technical Specification)** is the contract — interfaces, package layout, data flow. Do not deviate from interface signatures without flagging it.
- **Section 3 (Implementation Plan)** is the execution order. Work phase by phase. Do not start Phase N+1 until Phase N's Definition of Done is met and tests pass.
- Every code block in Section 2 is a **starting contract**, not a suggestion — implement against these signatures unless a genuine blocker requires a documented deviation.
- When a task says "write tests," write them in the same PR/commit as the implementation, not after.

---

# 1. Business Requirements

## 1.1 Problem Statement

LLM-based agent systems (coding agents, RAG chatbots, orchestration workers) repeatedly send redundant or oversized context to LLM providers, resulting in:

1. **Unnecessary token cost** — repeated prefixes not reusing provider-side prompt caching, verbose tool outputs sent raw, duplicate tool calls re-executed and re-sent.
2. **Unnecessary LLM calls** — deterministic tasks (validation, formatting, extraction) routed through an LLM when plain code would suffice.
3. **Suboptimal model selection** — all tasks routed to the same (often most expensive) model regardless of complexity.
4. **No systematic RAG integration** — retrieval-augmented context, when used, is not consistently compressed or positioned to preserve provider-side caching.

There is currently no shared, reusable solution to these problems across the systems where they occur.

## 1.2 Goals

| Goal | Success Metric |
|---|---|
| Reduce input token cost per LLM call | ≥30% reduction in billed input tokens vs. baseline, on systems that adopt the full pipeline |
| Reduce redundant LLM calls | Measurable % of requests short-circuited by deterministic pre-processing or tool-result cache hits |
| Improve prompt-cache hit rate | Provider-reported cache hit rate (`cache_read_input_tokens` / total input tokens) demonstrably higher after adoption than before |
| Enable cost-aware model routing | % of eligible traffic successfully served by a cheaper/smaller model without quality regression |
| Reusability across systems | The same library, unmodified at its core, is successfully integrated into at least 2 unrelated systems (e.g., an agent loop and a RAG chatbot) without forking |

## 1.3 Non-Goals (explicitly out of scope for v1)

- Building a hosted/managed service — this is a **library**, not a SaaS product.
- Building a UI/dashboard — v1 exposes metrics via structured logs/Prometheus-compatible counters only; visualization is a future phase.
- Training or hosting any ML model (e.g., a learned compressor or router) — v1 uses heuristic/rule-based logic only.
- Multi-tenant billing/quota enforcement.
- Replacing existing orchestration tools (e.g., Hermes Agent) — `agentkit` complements them, it does not replace routing/provider-auth responsibilities they already handle well.

## 1.4 Stakeholders & Users

- **Primary user:** Go backend developers integrating LLM calls into their own services (internal, first-party consumers).
- **Consumers of output:** Any system currently making direct Anthropic API calls from Go code (agent loops, RAG backends, orchestration workers).

## 1.5 Target Use Cases (must all be supportable without core changes)

1. **Coding/orchestration agent loop** — long-running, multi-turn, heavy tool use, growing context.
2. **RAG-based Q&A backend** — retrieval-heavy, single or few-turn, latency-sensitive.
3. **Local/hybrid model routing** — mixed traffic across a local self-hosted model and a cloud provider, routed by task complexity.
4. **Lightweight validators/classifiers** — short-lived tasks where most traffic should never reach an LLM at all.

If a design decision would make any of these four use cases impossible without modifying core packages, that decision must be reconsidered.

## 1.6 Constraints

- Must be pure Go, no CGo dependencies, no external ML runtime requirements in the core module.
- Must not require any specific LLM provider, vector database, or cache backend — these are pluggable via interfaces.
- Must not import any application-specific code (no dependency on any specific company/business system).
- Must degrade gracefully (fail-open) — a failure in any optimization stage must never cause the overall LLM call to fail.

---

# 2. Technical Specification

## 2.1 Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                      Caller (any Go service)                     │
└──────────────────────────────┬────────────────────────────────────┘
                                │  agentkit.Request
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│                         agentkit.Pipeline                        │
│  ┌──────────┐ ┌───────────┐ ┌─────┐ ┌──────────┐ ┌────────────┐   │
│  │Preprocess│→│ ToolCache │→│ RAG │→│ Compress │→│ LazyLoad   │→..│
│  └──────────┘ └───────────┘ └─────┘ └──────────┘ └────────────┘   │
│  ┌────────────┐ ┌────────┐ ┌────────┐                             │
│→│ CacheAlign │→│ Router │→│ Budget │→ (call ModelClient.Send)      │
│  └────────────┘ └────────┘ └────────┘                             │
└──────────────────────────────┬────────────────────────────────────┘
                                │  agentkit.Response
                                ▼
                       Caller receives result
```

Each stage implements a single `Stage` interface. The `Pipeline` is a plain ordered slice of stages executed sequentially. There is no hidden control flow — one stage in, one stage out, in order, except `PreprocessStage`, which may short-circuit (see 2.4.1).

## 2.2 Module Layout

```
agentkit/                        # go module root — github.com/<org>/agentkit
├── go.mod
├── pipeline/
│   ├── stage.go                  # Stage interface, Request, Response, Pipeline
│   └── pipeline_test.go
├── providers/
│   ├── provider.go                # ModelClient interface
│   ├── anthropic/
│   │   ├── client.go               # Anthropic implementation
│   │   └── client_test.go
│   └── mock/
│       └── mock_client.go          # test double for ModelClient
├── stores/
│   ├── vectorstore.go              # VectorStore, Embedder interfaces
│   ├── pgvector/
│   │   └── store.go                # optional first-party pgvector adapter
│   └── mock/
│       └── mock_store.go
├── cache/
│   ├── aligner.go                  # CacheAligner
│   ├── breakpoint.go
│   └── aligner_test.go
├── toolcache/
│   ├── cache.go                    # ToolCache interface + in-memory impl
│   ├── redis/
│   │   └── cache.go                # optional Redis-backed impl
│   ├── key.go                      # deterministic hashing
│   └── cache_test.go
├── preprocess/
│   ├── rule.go                     # PreprocessRule interface
│   ├── registry.go
│   └── examples/                   # example rules — NOT imported by core
│       └── json_validator.go
├── rag/
│   ├── stage.go                    # RAGStage
│   └── stage_test.go
├── compress/
│   ├── router.go                    # ContentRouter
│   ├── json.go
│   ├── code.go
│   ├── text.go
│   └── compress_test.go
├── lazyload/
│   ├── ref.go
│   └── loader.go
├── router/
│   ├── router.go                    # Router interface, RouteDecision
│   ├── heuristic.go                 # HeuristicRouter
│   └── router_test.go
├── budget/
│   ├── policy.go
│   └── classifier.go
├── metrics/
│   └── metrics.go                    # stage-level counters, provider-agnostic
├── config/
│   └── config.go                     # functional-options config surface
├── agentkit.go                        # top-level constructor: agentkit.New(...)
└── examples/                          # runnable, standalone example programs
    ├── coding-agent/
    ├── rag-chatbot/
    └── local-routing/
```

**Rule: nothing under `agentkit/` (excluding `examples/`) may import from an application repository.** `examples/` may reference toy/sample data only, never real credentials or real business logic.

## 2.3 Core Types

```go
// pipeline/stage.go

package pipeline

import "context"

// Request flows through every stage. Stages read and mutate fields relevant
// to their job and pass the (possibly modified) Request to the next stage.
type Request struct {
    // Query is the user-facing question/instruction for this turn.
    Query string

    // Messages is the full conversation history, oldest first.
    Messages []Message

    // ToolCalls, if non-nil, means this Request represents a tool-call
    // resolution step rather than a fresh LLM turn.
    ToolCalls []ToolCall

    // NeedsRetrieval signals to RAGStage whether retrieval should run at all.
    // Default false — callers must opt in explicitly.
    NeedsRetrieval bool

    // RetrievedChunks is populated by RAGStage; empty until then.
    RetrievedChunks []Chunk

    // TurnType classifies this turn for BudgetStage (see budget package).
    TurnType TurnType

    // CacheBreakpoints is populated by CacheAlignStage.
    CacheBreakpoints []CacheBreakpoint

    // Metadata is an open bag for stage-specific or caller-specific data
    // that doesn't warrant a first-class field. Stages must namespace keys
    // (e.g. "preprocess.matched_rule") to avoid collisions.
    Metadata map[string]any
}

type Message struct {
    Role    string // "user" | "assistant" | "system"
    Content string
}

type ToolCall struct {
    Name string
    Args map[string]any
}

type Chunk struct {
    Content    string
    SourceURL  string
    Similarity float64
}

type TurnType int

const (
    TurnUnknown TurnType = iota
    TurnNewQuestion
    TurnRoutineResume // e.g., resuming right after a tool result
    TurnErrorRecovery
)

type CacheBreakpoint struct {
    AfterMessageIndex int
    Reason            string
}

// Response is the final result returned to the caller after the pipeline
// (or a short-circuiting stage) has produced an answer.
type Response struct {
    Content        string
    ModelUsed      string
    ShortCircuited bool   // true if PreprocessStage handled it without an LLM call
    Usage          Usage
}

type Usage struct {
    InputTokens         int
    OutputTokens        int
    CacheReadTokens      int
    CacheCreationTokens  int
}

// Stage is the single extension point of the pipeline. Every optimization
// technique (compression, caching, routing, etc.) is a Stage implementation.
type Stage interface {
    // Process receives the current Request state and returns the next state.
    // Returning a non-nil error aborts the pipeline UNLESS the stage's own
    // contract says otherwise (see "fail-open" rule in 2.5).
    Process(ctx context.Context, req *Request) (*Request, error)

    // Name is used in logs/metrics to identify which stage did what.
    Name() string
}

// Pipeline runs an ordered list of stages, then performs the final model call.
type Pipeline struct {
    stages []Stage
    client ModelClient
}

func New(client ModelClient, stages ...Stage) *Pipeline {
    return &Pipeline{stages: stages, client: client}
}

func (p *Pipeline) Run(ctx context.Context, req *Request) (*Response, error) {
    for _, stage := range p.stages {
        var err error
        req, err = stage.Process(ctx, req)
        if err != nil {
            return nil, &StageError{Stage: stage.Name(), Err: err}
        }
        if shortCircuit, ok := req.Metadata["_short_circuit_response"]; ok {
            return shortCircuit.(*Response), nil
        }
    }
    return p.client.Send(ctx, req)
}

type StageError struct {
    Stage string
    Err   error
}

func (e *StageError) Error() string {
    return e.Stage + ": " + e.Err.Error()
}
func (e *StageError) Unwrap() error { return e.Err }
```

```go
// providers/provider.go

package providers

import (
    "context"
    "agentkit/pipeline"
)

// ModelClient abstracts a single LLM provider/model endpoint. Implementations
// must NOT be assumed to be Anthropic-specific anywhere outside this package
// and its subdirectories.
type ModelClient interface {
    Send(ctx context.Context, req *pipeline.Request) (*pipeline.Response, error)
    Name() string
}
```

```go
// stores/vectorstore.go

package stores

import (
    "context"
    "agentkit/pipeline"
)

type Embedder interface {
    Embed(ctx context.Context, text string) ([]float32, error)
}

type VectorStore interface {
    Search(ctx context.Context, vec []float32, topK int) ([]pipeline.Chunk, error)
}
```

## 2.4 Stage Specifications

### 2.4.1 PreprocessStage

**Package:** `preprocess`
**Purpose:** Handle deterministic tasks without invoking an LLM at all.

```go
package preprocess

import (
    "context"
    "agentkit/pipeline"
)

type Rule interface {
    // CanHandle returns true if this rule can fully resolve the request
    // without an LLM call.
    CanHandle(req *pipeline.Request) bool
    // Handle resolves the request and returns the final Response.
    Handle(req *pipeline.Request) (*pipeline.Response, error)
    Name() string
}

type Stage struct {
    rules []Rule
}

func NewStage(rules ...Rule) *Stage { return &Stage{rules: rules} }

func (s *Stage) Name() string { return "preprocess" }

func (s *Stage) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
    for _, r := range s.rules {
        if r.CanHandle(req) {
            resp, err := r.Handle(req)
            if err != nil {
                // Fail-open: if a rule errors, do NOT abort the pipeline.
                // Fall through to the LLM as if no rule matched.
                continue
            }
            resp.ShortCircuited = true
            if req.Metadata == nil {
                req.Metadata = map[string]any{}
            }
            req.Metadata["_short_circuit_response"] = resp
            req.Metadata["preprocess.matched_rule"] = r.Name()
            return req, nil
        }
    }
    return req, nil
}
```

**Requirement:** an errored rule must never abort the overall request — it must be treated as "did not match" and the pipeline continues.

### 2.4.2 ToolCache

**Package:** `toolcache`

```go
package toolcache

import (
    "context"
    "time"
)

type CachedResult struct {
    Value    any
    CachedAt time.Time
}

// Cache is the interface; ship an in-memory implementation in this package
// and a Redis-backed one under toolcache/redis.
type Cache interface {
    Get(ctx context.Context, toolName string, args map[string]any) (CachedResult, bool, error)
    Set(ctx context.Context, toolName string, args map[string]any, value any, ttl time.Duration) error
}
```

**Requirements:**
- Hashing must be deterministic regardless of map key iteration order — marshal `args` to JSON with sorted keys before hashing (SHA-256).
- TTL is per-`Set` call, not global, so callers can vary TTL by tool name.
- The in-memory implementation must be safe for concurrent use (`sync.RWMutex`).

### 2.4.3 RAGStage

**Package:** `rag`

```go
package rag

import (
    "context"
    "agentkit/pipeline"
    "agentkit/stores"
)

type Stage struct {
    embedder stores.Embedder
    store    stores.VectorStore
    topK     int
}

func NewStage(embedder stores.Embedder, store stores.VectorStore, topK int) *Stage {
    return &Stage{embedder: embedder, store: store, topK: topK}
}

func (s *Stage) Name() string { return "rag" }

func (s *Stage) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
    if !req.NeedsRetrieval {
        return req, nil
    }
    vec, err := s.embedder.Embed(ctx, req.Query)
    if err != nil {
        // Fail-open: proceed without retrieval rather than aborting.
        return req, nil
    }
    chunks, err := s.store.Search(ctx, vec, s.topK)
    if err != nil {
        return req, nil
    }
    req.RetrievedChunks = chunks
    return req, nil
}
```

**Placement requirement:** `RAGStage` MUST run before `compress.Stage` and before `cache.AlignStage` in any pipeline that uses it. This ordering is load-bearing for cache-hit behavior (see 2.4.6) — document this loudly in code comments, not just here.

### 2.4.4 CompressStage

**Package:** `compress`

```go
package compress

import (
    "context"
    "agentkit/pipeline"
)

type Compressor interface {
    Compress(content string) (string, error)
    CanHandle(content string) bool
}

type Stage struct {
    compressors []Compressor // checked in order; first CanHandle wins
}

func NewStage(compressors ...Compressor) *Stage { return &Stage{compressors: compressors} }

func (s *Stage) Name() string { return "compress" }

func (s *Stage) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
    for i, chunk := range req.RetrievedChunks {
        for _, c := range s.compressors {
            if c.CanHandle(chunk.Content) {
                compressed, err := c.Compress(chunk.Content)
                if err != nil {
                    break // fail-open: leave this chunk uncompressed
                }
                req.RetrievedChunks[i].Content = compressed
                break
            }
        }
    }
    return req, nil
}
```

**v1 scope:** ship `json.Compressor` (strip whitespace/redundant fields from JSON-looking content) and `text.Compressor` (basic whitespace/boilerplate stripping). `code.Compressor` (AST-aware) is Phase 2+ and may start as a no-op passthrough that just marks itself `CanHandle == false` until implemented.

### 2.4.5 LazyLoadStage

**Package:** `lazyload`

```go
package lazyload

import "context"

type Ref struct {
    ID   string
    Kind string // "file" | "doc" | "chunk"
}

type Loader interface {
    Resolve(ctx context.Context, ref Ref) (string, error)
}
```

v1: define the interface and one reference implementation (`FileLoader` reading from local disk). Do not wire it into the default pipeline by default — it's opt-in per caller since it changes tool-exposure surface (the caller must expose a `load_content` tool to the model).

### 2.4.6 CacheAlignStage

**Package:** `cache`

```go
package cache

import (
    "context"
    "agentkit/pipeline"
)

type BreakpointSpec struct {
    // Static breakpoints are placed after content that should never change
    // within a session (system prompt, tool definitions).
    AfterStaticContent bool
    Reason              string
}

type Aligner struct {
    specs []BreakpointSpec
}

func NewAligner(specs ...BreakpointSpec) *Aligner { return &Aligner{specs: specs} }

func (a *Aligner) Name() string { return "cache_align" }

func (a *Aligner) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
    // 1. Reorder: static content (system/tools) first, then append-only
    //    history, then dynamic content (RAG chunks), then the newest message.
    // 2. Emit CacheBreakpoint entries at the boundary of each static segment.
    // 3. MUST NOT place a breakpoint after RetrievedChunks — those change
    //    every turn and would poison the cache if marked as a breakpoint.
    req.CacheBreakpoints = computeBreakpoints(req, a.specs)
    return req, nil
}
```

**Non-negotiable requirement:** this stage must run *after* `RAGStage`, `compress.Stage`, and `lazyload` (if used), and it must never emit a cache breakpoint immediately after dynamic content (`RetrievedChunks`). Add a unit test that asserts this explicitly — construct a `Request` with `RetrievedChunks` populated and verify no `CacheBreakpoint` lands within that segment.

### 2.4.7 Router

**Package:** `router`

```go
package router

import (
    "context"
    "agentkit/pipeline"
    "agentkit/providers"
)

type RouteDecision struct {
    Client     providers.ModelClient
    Reason     string
    Confidence float64
}

type Router interface {
    Route(ctx context.Context, req *pipeline.Request) RouteDecision
}

type Tier struct {
    Client        providers.ModelClient
    MaxComplexity float64 // this tier handles requests scoring <= this value
}

type HeuristicRouter struct {
    tiers []Tier // must be sorted ascending by MaxComplexity by the caller
}

func NewHeuristicRouter(tiers ...Tier) *HeuristicRouter {
    return &HeuristicRouter{tiers: tiers}
}

func (r *HeuristicRouter) Route(ctx context.Context, req *pipeline.Request) RouteDecision {
    score := estimateComplexity(req)
    for _, t := range r.tiers {
        if score <= t.MaxComplexity {
            return RouteDecision{Client: t.Client, Reason: "heuristic", Confidence: 1 - score}
        }
    }
    last := r.tiers[len(r.tiers)-1]
    return RouteDecision{Client: last.Client, Reason: "heuristic_overflow", Confidence: 0}
}

// estimateComplexity is intentionally simple in v1: a weighted function of
// message length, number of retrieved chunks, and presence of code blocks.
// Document the exact formula in code comments; do not hide it in a magic
// number. Make weights configurable via functional options.
func estimateComplexity(req *pipeline.Request) float64 {
    // implementation detail — see Phase 3 task list
    return 0
}
```

`Router` does not implement `pipeline.Stage` directly in v1 — it's invoked by the `Pipeline` after all context-shaping stages run, immediately before the model call, so it can see final prompt size. Wire it as a special final step in `Pipeline.Run`, not as an ordinary stage in the `[]Stage` slice, to keep this ordering explicit and un-misconfigurable.

### 2.4.8 BudgetStage

**Package:** `budget`

```go
package budget

import (
    "context"
    "agentkit/pipeline"
)

type Policy struct {
    RoutineStepBudget int
    NewQuestionBudget int
    ErrorRecoveryBudget int
}

func (p Policy) BudgetFor(t pipeline.TurnType) int {
    switch t {
    case pipeline.TurnRoutineResume:
        return p.RoutineStepBudget
    case pipeline.TurnErrorRecovery:
        return p.ErrorRecoveryBudget
    default:
        return p.NewQuestionBudget
    }
}
```

v1: classification of `TurnType` is the caller's responsibility (set on `Request` before calling `Pipeline.Run`). A `TurnClassifier` heuristic helper may be added in Phase 3 but is not required for v1 correctness.

## 2.5 Cross-Cutting Requirements

1. **Fail-open, always.** No stage may cause the overall `Pipeline.Run` call to fail due to an internal optimization error (compression failure, cache backend unavailable, embedding API down, etc.). The only errors that should propagate are: (a) the final `ModelClient.Send` call itself failing, or (b) a genuine programming error (nil pointer on a required, non-optional dependency) — and those should be caught by tests, not by production fail-open behavior.
2. **No global mutable state.** Every stage is constructed with its dependencies explicitly (dependency injection via constructor), no package-level singletons.
3. **Context propagation.** Every `Process` method must respect `ctx.Done()` for any I/O it performs (cache backend calls, embedding calls, model calls).
4. **Deterministic where it matters.** `toolcache` key hashing must be deterministic. `cache.Aligner` breakpoint placement must be deterministic given the same `Request` shape.
5. **Metrics.** Every stage that does meaningful work (skips an LLM call, hits a cache, compresses content, routes to a specific tier) must emit a counter via the `metrics` package. Use a minimal, provider-agnostic interface:

```go
// metrics/metrics.go
package metrics

type Counter interface {
    Inc(labels map[string]string)
}

type Recorder interface {
    Counter(name string) Counter
}
```

Provide a no-op `Recorder` as the default so metrics are opt-in and never a hard dependency.

## 2.6 Non-Functional Requirements

| Requirement | Target |
|---|---|
| No CGo | Core module must build with `CGO_ENABLED=0` |
| Concurrency safety | All stateful components (`ToolCache`, `Aligner` if it caches anything) must pass `go test -race` |
| Test coverage | ≥80% for `pipeline`, `toolcache`, `cache`, `router`, `preprocess` packages |
| Dependency footprint | Core module (excluding `stores/pgvector`, `toolcache/redis`, `providers/anthropic`) has zero third-party dependencies beyond the Go standard library |
| Backward compatibility | Once v1.0.0 is tagged, `Stage`, `ModelClient`, `VectorStore`, `Cache` interface signatures are frozen; changes require a new major version |

---

# 3. Implementation Plan

Work through phases in order. Each phase has a Definition of Done (DoD) that must be met — including passing tests — before starting the next phase.

## Phase 0 — Repository Bootstrap

- [ ] `go mod init` with module path decided (placeholder: `agentkit`)
- [ ] Create directory structure exactly as specified in 2.2
- [ ] `pipeline/stage.go` — implement `Request`, `Response`, `Stage`, `Pipeline` exactly as specified in 2.3
- [ ] `providers/provider.go` — `ModelClient` interface
- [ ] `providers/mock/mock_client.go` — a `ModelClient` implementation for tests that returns a configurable canned `Response`
- [ ] `pipeline/pipeline_test.go` — test that `Pipeline.Run` with zero stages calls `ModelClient.Send` directly and returns its result unmodified
- [ ] CI config (GitHub Actions or equivalent): run `go build ./...`, `go vet ./...`, `go test -race ./...` on every push

**DoD:** `go build ./...` succeeds, `go test ./...` passes with the one bootstrap test above, CI is green.

## Phase 1 — Foundation Stages (no ML, highest ROI)

### 1.1 `preprocess` package
- [ ] Implement `Rule` interface and `Stage` exactly as specified in 2.4.1, including the fail-open behavior on rule errors
- [ ] `preprocess/examples/json_validator.go` — one example `Rule` implementation (validates JSON against a schema, returns a `Response` directly, no LLM involved) — lives in `examples`, not imported by core
- [ ] Tests: (a) a matching rule short-circuits and `ShortCircuited == true`; (b) an erroring rule is skipped and the request proceeds to the next rule/pipeline; (c) no matching rule leaves the request unchanged

### 1.2 `toolcache` package
- [ ] `Cache` interface as specified in 2.4.2
- [ ] In-memory implementation (`toolcache/memory.go`): `sync.RWMutex` + `map[string]CachedResult`, per-entry TTL
- [ ] `key.go`: `HashToolCall(toolName string, args map[string]any) (string, error)` — marshals `args` via `encoding/json` after sorting keys, hashes with `crypto/sha256`
- [ ] Tests: (a) identical args in different map insertion order produce the same hash; (b) `Get` after TTL expiry returns `found == false`; (c) concurrent `Get`/`Set` from multiple goroutines passes `-race`

### 1.3 `cache` package (Aligner)
- [ ] `Aligner`, `BreakpointSpec`, `computeBreakpoints` as specified in 2.4.6
- [ ] Reordering logic: static segment (caller-marked messages) → append-only history → dynamic segment (`RetrievedChunks`, if present) → newest message
- [ ] Explicit test: given a `Request` with non-empty `RetrievedChunks`, assert no `CacheBreakpoint.AfterMessageIndex` falls inside that segment
- [ ] Explicit test: given two `Request`s with identical static+history segments but different `RetrievedChunks`, assert the computed breakpoints for the static+history segment are identical (this is the property that keeps provider-side caching effective)

**Phase 1 DoD:** all three packages have ≥80% coverage, `go test -race ./...` is green, and there is a runnable example under `examples/` demonstrating `preprocess` + `toolcache` + `cache` wired into a `Pipeline` against `providers/mock`.

## Phase 2 — Compression, RAG, Providers

### 2.1 `providers/anthropic`
- [ ] Implement `ModelClient` against the Anthropic Go SDK (or raw HTTP if no SDK dependency is desired)
- [ ] Must translate `Request.CacheBreakpoints` into the appropriate `cache_control` fields on outgoing content blocks
- [ ] Must populate `Response.Usage` (including `CacheReadTokens`/`CacheCreationTokens`) from the API response
- [ ] Integration test (may be skipped in CI without credentials, but must exist and be runnable manually): two sequential calls with an identical static prefix show `CacheReadTokens > 0` on the second call

### 2.2 `stores` + `stores/pgvector`
- [ ] `Embedder`, `VectorStore` interfaces as specified in 2.3
- [ ] `stores/pgvector/store.go` — implementation using `pgx`/`pgxpool`, parameterized SQL, no string concatenation of user input
- [ ] `stores/mock/mock_store.go` — in-memory `VectorStore` for tests (linear scan cosine similarity is fine)

### 2.3 `rag` package
- [ ] `Stage` as specified in 2.4.3, including fail-open on embed/search errors
- [ ] Test: `NeedsRetrieval == false` skips retrieval entirely (no calls to embedder/store — verify via mock call-count assertions)
- [ ] Test: embedder error results in `req.RetrievedChunks` remaining empty and `Process` returning `nil` error (fail-open)

### 2.4 `compress` package
- [ ] `Compressor` interface, `Stage` as specified in 2.4.4
- [ ] `json.Compressor` — detects JSON-shaped content, strips whitespace and (configurably) specified low-value fields
- [ ] `text.Compressor` — strips redundant whitespace/boilerplate from prose; must be conservative (never alter meaning-bearing content) — no semantic summarization in v1
- [ ] Test: compressed output is a strict subset transformation (no content invented) — assert via round-trip checks appropriate to each compressor
- [ ] Test: a compressor that returns an error leaves the original chunk content untouched (fail-open)

### 2.5 `lazyload` package
- [ ] `Ref`, `Loader` interfaces as specified in 2.4.5
- [ ] `FileLoader` implementation reading from local disk with path traversal protection (reject `..` path segments)

**Phase 2 DoD:** an example under `examples/rag-chatbot/` runs end-to-end against `stores/mock` and `providers/mock`, demonstrating `RAGStage → CompressStage → CacheAlignStage` ordering and the non-negotiable cache-breakpoint placement rule from 2.4.6.

## Phase 3 — Routing & Budget

### 3.1 `router` package
- [ ] `Router` interface, `Tier`, `HeuristicRouter` as specified in 2.4.7
- [ ] Implement `estimateComplexity` — document the exact scoring formula in a code comment (weights for message length, chunk count, code-block presence); expose weights as constructor options so they're tunable without code changes
- [ ] Test: a request scoring below the lowest tier's `MaxComplexity` routes to that tier; a request scoring above all tiers routes to the last tier with `Reason == "heuristic_overflow"`

### 3.2 `budget` package
- [ ] `Policy`, `BudgetFor` as specified in 2.4.8
- [ ] Test: each `TurnType` maps to its corresponding budget field

### 3.3 Wire `Router` into `Pipeline`
- [ ] Extend `Pipeline` (or add a `PipelineWithRouting` variant) so that, after all `[]Stage` entries run, a `Router` (if configured) selects the `ModelClient` used for the final `Send` call, overriding the `Pipeline`'s default client
- [ ] Test: with two tiers configured, a low-complexity request and a high-complexity request are demonstrably routed to different mock clients

**Phase 3 DoD:** an example under `examples/local-routing/` demonstrates two `ModelClient` mocks standing in for a "local" and "cloud" model, routed by a `HeuristicRouter`.

## Phase 4 — Hardening

- [ ] `toolcache/redis` — Redis-backed `Cache` implementation, same interface as the in-memory one, integration-tested against a `miniredis` or real Redis in CI
- [ ] `metrics` package wired into every stage (Phase 1–3 stages updated to emit counters via the `Recorder` interface, no-op by default)
- [ ] Full concurrency stress test: spin up N goroutines calling `Pipeline.Run` concurrently against shared `ToolCache`/`Aligner` instances, run under `-race`
- [ ] `examples/coding-agent/` — the most complete example, wiring all stages together (preprocess, toolcache, rag, compress, lazyload, cache align, router, budget)
- [ ] README.md at repo root: quickstart, architecture diagram (reuse 2.1), functional-options usage example, explicit statement of the interface-freeze policy from 2.6
- [ ] Verify zero-dependency claim: `go list -deps ./pipeline/... ./toolcache/... ./cache/... ./router/... ./preprocess/...` contains no third-party imports

**Phase 4 DoD:** `go vet ./...`, `go test -race ./...`, and `go build ./...` all pass; README exists; all four target use cases from Business Requirements §1.5 have a corresponding working example.

## 3.1 Testing Strategy Summary

| Layer | Test approach |
|---|---|
| Pure logic (hashing, breakpoint computation, complexity scoring) | Table-driven unit tests, no I/O |
| Stateful components (`ToolCache`, in-memory `VectorStore`) | Unit tests + `-race` concurrency tests |
| Stage `Process` methods | Unit tests using `providers/mock` and `stores/mock`, asserting both the happy path and the fail-open path |
| Full pipelines | Example programs under `examples/` serve as executable integration tests; wire at least one into CI using only mocks (no real API keys required) |
| Real-provider integration (Anthropic cache hit behavior, pgvector queries) | Documented manual test procedures; may be skipped in CI, must not be skipped in code review sign-off before a release tag |

## 3.2 Acceptance Criteria (maps back to Business Requirements §1.2)

- [ ] A benchmark script (can live under `examples/coding-agent/` or a dedicated `benchmarks/` dir) demonstrates ≥30% input-token reduction on a representative synthetic workload (long conversation history + repeated tool calls + RAG retrieval) compared to the same workload with no `agentkit` stages applied.
- [ ] The same benchmark demonstrates a measurable % of requests short-circuited by `preprocess` rules on a workload where such rules are applicable.
- [ ] The same benchmark, run against a real Anthropic endpoint (manual, documented procedure, not CI), shows `CacheReadTokens > 0` on turns after the first in a multi-turn conversation.
- [ ] `examples/local-routing/` demonstrates traffic split across two `ModelClient` tiers driven purely by `HeuristicRouter`, with no manual per-request routing code in the example's business logic.
- [ ] The core module (per 2.6) is successfully `go get`-able and used from at least two separate, independent `examples/` programs without any shared non-`agentkit` code between them.

---

# 4. Open Questions for Product Owner (resolve before or during Phase 0)

1. Final module path / import path (`github.com/<org>/agentkit` — org/repo name TBD).
2. License for the repository (relevant if open-sourcing per prior discussion — Apache-2.0 recommended for parity with RTK/Headroom precedent).
3. Whether `providers/anthropic` ships in the core repo or as a separate go module (`agentkit-anthropic`) to keep the core truly zero-dependency — recommendation: separate module, imported by convenience in `examples/` only.
4. Minimum supported Go version (recommendation: latest two stable Go releases at time of v1.0.0 tag).

---

# 5. Glossary

| Term | Definition |
|---|---|
| Stage | A single unit of pipeline processing implementing the `Stage` interface |
| Fail-open | An error-handling policy where a component's internal failure is swallowed and processing continues without that component's benefit, rather than aborting the whole operation |
| Cache breakpoint | A marker indicating where provider-side prompt caching should be anchored in the outgoing request |
| Short-circuit | When `PreprocessStage` fully resolves a request without invoking an LLM at all |
| Tier | A `(ModelClient, complexity threshold)` pair used by `HeuristicRouter` |
