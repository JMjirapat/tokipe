# QA Verification Report — agentkit Delivery 1

**Review date:** 2026-07-26  
**Reviewed revision:** `94277a1`  
**Source of truth:** [spec.md](spec.md)  
**Delivery claim:** [DELIVERY-1.md](DELIVERY-1.md)  
**QA procedure:** [QA-BRIEF.md](QA-BRIEF.md)  
**Result:** **NO-GO for `v1.0.0`**

## 1. Executive summary

Delivery 1 builds successfully, passes the race detector, satisfies the stated
coverage threshold, and reproduces the documented 57.1% synthetic benchmark
reduction. The core cache-aligner tests, concurrency tests, nested adapter
modules, and security checks reviewed during this pass were also green.

The release is not ready to tag because the review found:

| Severity | Count |
|---|---:|
| Blocker | 2 |
| Major | 3 |
| Minor | 1 |

The highest-impact defect is a violation of the fail-open guarantee: a panic
from a tool executor escapes `Pipeline.Run` and prevents the model call.
Additional findings affect deterministic tool-cache hashing and the Anthropic
serialization of cache breakpoints and retrieved context.

No production source code was changed during this review.

## 2. Findings

### [Blocker] Tool executor panic breaks the fail-open guarantee

**File:** `toolcache/stage.go:126`, `toolcache/singleflight.go:60`  
**Contract:** `spec.md` §2.5.1; `agentkit.go:14-16`

**Claim**

An internal optimization failure must not cause `Pipeline.Run` to fail. The
turn must continue to the final model call.

**Reality**

`toolcache.Stage` invokes the caller-supplied `Executor` without panic
recovery. `singleflight.group.Do` releases its waiters in a deferred cleanup,
but deliberately propagates the panic. The panic therefore escapes
`Pipeline.Run`, and the model is not called.

**Concrete input**

```go
stage := toolcache.NewStage(
    toolcache.NewMemoryCache(),
    func(context.Context, pipeline.ToolCall) (any, error) {
        panic("executor panic")
    },
)

p := pipeline.New(model, stage)
_, _ = p.Run(context.Background(), &pipeline.Request{
    ToolCalls: []pipeline.ToolCall{
        {Name: "demo", Args: map[string]any{"x": 1}},
    },
})
```

**Observed result**

```text
Pipeline.Run panicked instead of failing open: executor panic
model calls = 0, want 1
```

**Impact**

A failing tool integration can crash the request goroutine or the containing
process, depending on the caller's recovery policy. This directly violates the
load-bearing fail-open promise.

**Required before GO**

Recover executor panics at the tool-cache boundary, turn the call into an
unresolved/fail-open result, and add a regression test proving the final model
is still called.

---

### [Blocker] Test count in Delivery 1 evidence is incorrect

**File:** `docs/DELIVERY-1.md:37`

**Claim**

`go test -race ./...` covers 237 tests across 24 packages.

**Reality**

The reviewed revision produced 239 test/subtest run events across 24 root
module packages.

**Reproduction**

```bash
go test -race -json -count=1 ./... | rg -c '"Action":"run"'
go list ./... | wc -l
```

**Observed result**

```text
239
24
```

**Impact**

The package count is correct and all tests pass, but the evidence recorded in
Delivery 1 is not reproducible as written. `QA-BRIEF.md` classifies a false
claim in Delivery §2 as a Blocker.

**Required before GO**

Correct the count or document the exact counting method that reproducibly
produces 237.

---

### [Major] Distinct non-UTF-8 arguments collide in `HashToolCall`

**File:** `toolcache/key.go:28-49`  
**Contract:** `spec.md` §2.4.2 and §2.5.4

**Claim**

The hash deterministically and uniquely represents the tool name and its
JSON-compatible arguments for cache lookup.

**Reality**

The implementation first calls `json.Marshal`. Go's JSON encoder replaces
invalid UTF-8 bytes with the Unicode replacement character. Distinct byte
strings can therefore become identical before hashing.

**Concrete input**

```go
a, _ := toolcache.HashToolCall(
    "read",
    map[string]any{"path": string([]byte{0xff})},
)
b, _ := toolcache.HashToolCall(
    "read",
    map[string]any{"path": string([]byte{0xfe})},
)
```

**Observed result**

Both inputs produced:

```text
2e73534fad14b3856cfdec7da71eb4e87e51c6eab13a453e1d679ad0d04be569
```

**Impact**

The cache can return the result of a different tool call. This is incorrect
behaviour for callers that use Go strings as opaque byte-bearing values.

**Required before GO**

Reject invalid UTF-8 arguments as uncacheable, or introduce an unambiguous
byte-preserving canonical representation. Add a collision regression test.

---

### [Major] Anthropic breakpoint can end before caller-marked static content

**File:** `providers/anthropic/client.go:228-256`  
**Contract:** `spec.md` §2.4.6

**Claim**

The breakpoint emitted after the static segment covers all caller-marked
static content.

**Concrete input**

```go
req := &pipeline.Request{Messages: []pipeline.Message{
    {Role: "user", Content: "static tool definition", Static: true},
    {Role: "system", Content: "system policy"},
    {Role: "user", Content: "current question"},
}}
```

After cache alignment, the breakpoint is
`AfterMessageIndex: 1`, correctly indicating the end of the two-message static
segment.

**Reality**

The Anthropic adapter hoists the system message into the top-level `system`
field but leaves the caller-marked static user message in `messages`. It then
places `cache_control` on the system block. In Anthropic wire order, the static
user block occurs after that breakpoint.

**Observed request fragment**

```json
{
  "system": [
    {
      "type": "text",
      "text": "system policy",
      "cache_control": {"type": "ephemeral"}
    }
  ],
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "static tool definition"}
      ]
    },
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "current question"}
      ]
    }
  ]
}
```

**Impact**

Supported caller-marked static content is excluded from the cached prefix.
This reduces cache effectiveness and contradicts the cache-alignment contract.

**Required before GO**

Translate logical message breakpoints after constructing the final Anthropic
wire order, ensuring the deepest cache marker covers every static block.

---

### [Major] Anthropic can serialize retrieved context after the newest message

**File:** `providers/anthropic/client.go:259-276`  
**Contract:** `spec.md` §2.4.6

**Claim**

The final layout is:

```text
static → append-only history → dynamic retrieved context → newest message
```

**Concrete input**

```go
req := &pipeline.Request{
    Query: "current question",
    Messages: []pipeline.Message{
        {Role: "system", Content: "stable"},
        {Role: "user", Content: "current question"},
    },
    RetrievedChunks: []pipeline.Chunk{
        {Content: "retrieved evidence"},
    },
}
```

**Reality**

Because the last non-system message already equals `req.Query`,
`endsWithQuery` suppresses the final query append. Retrieved chunks are then
appended after the existing current-question message.

**Observed wire order**

```text
1. user: current question
2. user: Retrieved context / retrieved evidence
```

**Impact**

The provider receives an order different from the cache-aligner contract.
Besides being a contract violation, placing evidence after the question can
change model behaviour for otherwise equivalent request shapes.

**Required before GO**

Separate the newest message from history while constructing the provider
payload, append retrieved context, then append the newest message exactly once.
Add coverage for both representations of the current turn: query-only and
query duplicated as the last message.

---

### [Minor] Public documentation overstates possible `Pipeline.Run` errors

**File:** `agentkit.go:14-16`, `pipeline/stage.go:187-203`

**Claim**

The only errors returned by `Run` come from the model call.

**Reality**

`Pipeline.Run` also returns:

- `context.Canceled` before a stage runs;
- `StageError` from a custom stage;
- `StageError` for malformed short-circuit metadata.

These behaviours are explicitly covered by the pipeline tests.

**Reproduction**

```bash
go test \
  -run 'TestStageErrorAbortsAndWraps|TestBadShortCircuitValueIsAnErrorNotAPanic|TestCanceledContextStopsBeforeStages' \
  ./pipeline
```

**Impact**

The implementation is internally consistent, but callers relying on the
package-level promise may implement incomplete error handling.

**Required before GO**

Narrow the documentation to built-in optimization dependency failures and
document cancellation, custom-stage errors, and programming/configuration
errors separately.

## 3. Verification results

### Baseline

The required root-module baseline passed:

```bash
go build ./...
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build ./...
```

### Nested modules

Both nested modules passed their test suites:

```bash
cd stores/pgvector && go test ./...
cd toolcache/redis && go test ./...
```

No external database or Redis service was used; credential-gated integration
tests retained their documented skip behaviour.

### Benchmark

```bash
go run ./benchmarks
```

Observed:

```text
baseline billed input tokens : 5005
agentkit billed input tokens : 2148
reduction                    : 57.1%
turns short-circuited        : 3/12
tool executions              : 6 → 4
```

The synthetic comparison is reasonable under its documented assumptions. The
review did not find evidence that the fixed workload exploits the benchmark's
metering implementation to manufacture the reported ratio. As Delivery 1
already states, the result is comparative evidence, not real-provider token
accounting.

### Coverage

The reported package coverage values were reproduced:

| Package | Coverage |
|---|---:|
| `agentkit` | 100.0% |
| `budget` | 100.0% |
| `router` | 100.0% |
| `compress` | 98.9% |
| `preprocess` | 98.8% |
| `rag` | 97.6% |
| `cache` | 97.2% |
| `pipeline` | 97.0% |
| `stores/mock` | 96.7% |
| `providers/anthropic` | 94.3% |
| `providers/cli` | 94.3% |
| `toolcache` | 93.0% |
| `config` | 90.5% |
| `lazyload` | 88.1% |

The five packages named by the specification all exceed the required 80%.

## 4. Areas reviewed with no demonstrated defect

- Cache-aligner breakpoint computation is deterministic for identical inputs.
- The core aligner does not place a breakpoint after retrieved chunks.
- `config.Stages()` keeps custom stages before the final built-in aligner.
- Shared-state race tests passed, including repeated runs of cache,
  tool-cache, router, and CLI test suites.
- CLI prompt rendering remained byte-deterministic across repeated tests.
- CLI execution uses `exec.CommandContext` with an argv vector and no shell.
- No API key was found in client names or constructed error strings.
- `lazyload.FileLoader` rejected the reviewed absolute-path, `..`, backslash,
  and symlink-escape cases.
- pgvector identifiers use a strict ASCII identifier allowlist and query
  values are parameterized.
- Empty router tiers correctly fall back to the configured model client.
- Documented interface aliases and the four deviations listed in
  `DELIVERY-1.md` were present.

The known gaps in `DELIVERY-1.md` §3 were not re-reported as defects.

## 5. Release decision

**Current decision: NO-GO**

The decision can change to **GO** when all of the following are complete:

1. Tool executor panics fail open and the model call is preserved.
2. Non-UTF-8 tool arguments cannot collide silently.
3. Anthropic cache breakpoints cover the complete static prefix.
4. Anthropic serialization always places retrieved context before the newest
   message.
5. The Delivery 1 test count and `Pipeline.Run` error documentation are
   corrected.
6. Regression tests are added for all four behavioural defects.
7. The full root and nested-module verification commands pass again.

