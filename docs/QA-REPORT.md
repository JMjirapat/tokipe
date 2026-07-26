# QA Verification Report — agentkit Delivery 1

**Review date:** 2026-07-26  
**Round 1 revision:** `94277a1`

**Round 2 revision:** `b5cdd38`

**Source of truth:** [spec.md](spec.md)  
**Delivery claim:** [DELIVERY-1.md](DELIVERY-1.md)  
**QA procedure:** [QA-BRIEF.md](QA-BRIEF.md)  
**Current result:** **NO-GO for `v1.0.0` after QA round 2**

## 1. Executive summary

All six inputs reported in QA round 1 now pass. Revision `b5cdd38` also builds
successfully, passes the race detector, satisfies the stated coverage
threshold, and reproduces the documented 57.1% synthetic benchmark reduction.

Adversarial re-verification of the new recovery and wire-order boundaries found
three additional cases not covered by the added regression tests:

| Severity | Count |
|---|---:|
| Blocker | 1 |
| Major | 2 |
| Minor | 0 |

The release remains NO-GO because `preprocess.Rule.Name()` is still outside
the new panic boundary and can panic out of `Pipeline.Run`. Cache backend
panics do not yet behave like ordinary cache errors, and the Anthropic adapter
still violates the dynamic-before-newest ordering for requests that carry the
newest message in `Messages` while leaving `Query` empty.

No production source code was changed during this review.

## 2. QA round 2 findings

### [Blocker] `Rule.Name()` panic still escapes `Pipeline.Run`

**File:** `preprocess/rule.go:144`

**Contract:** `spec.md` §2.5.1; `agentkit.go:14-17`

**Claim**

Revision `b5cdd38` states that caller-supplied preprocess rules now run behind
`internal/safe` and that a panic in an optimization still reaches the model.

**Reality**

`Rule.CanHandle` and `Rule.Handle` are wrapped, but `Rule.Name()` is invoked
outside the recovery boundary after a successful result. A rule whose name
method panics still takes down the turn.

**Concrete input**

```go
type panicNameRule struct{}

func (panicNameRule) CanHandle(*pipeline.Request) bool { return true }
func (panicNameRule) Handle(*pipeline.Request) (*pipeline.Response, error) {
    return &pipeline.Response{Content: "handled"}, nil
}
func (panicNameRule) Name() string { panic("rule name panic") }

kit := agentkit.New(model, config.WithPreprocess(panicNameRule{}))
_, _ = kit.Run(context.Background(), &pipeline.Request{Query: "q"})
```

**Observed result**

```text
Pipeline.Run panicked instead of containing Rule.Name: rule name panic
```

**Impact**

The load-bearing fail-open guarantee is still incomplete for a supported
extension interface.

**Required before GO**

Evaluate the rule name behind the same recovery boundary, or obtain a safe
fallback name before committing the short-circuit result. Add regression
coverage for panics from every `Rule` method.

---

### [Major] A panicking cache `Get` does not degrade to a cache miss

**File:** `toolcache/stage.go:127-145`

**Contract:** `spec.md` §2.5.1; `docs/DELIVERY-1.md` §0

**Claim**

A cache backend failure degrades to a miss. Delivery round 1 additionally
states that dependency panics are contained exactly like ordinary errors.

**Reality**

The whole get/execute/set sequence is wrapped in one `safe.Value` call. If
`Cache.Get` panics, `safe.Value` returns immediately with a `PanicError`.
The executor is never called and no fresh result is published. An ordinary
`Get` error, by contrast, is treated as a miss and proceeds to execution.

**Concrete input**

```go
type panicGetCache struct{}

func (panicGetCache) Get(
    context.Context, string, map[string]any,
) (toolcache.CachedResult, bool, error) {
    panic("cache get panic")
}
func (panicGetCache) Set(
    context.Context, string, map[string]any, any, time.Duration,
) error {
    return nil
}
```

Run one tool call through a `toolcache.Stage` backed by `panicGetCache`.

**Observed result**

```text
executor calls = 0, want 1 after cache panic
fresh result was not published
```

**Impact**

A cache outage can suppress the real tool execution instead of merely removing
the cache optimization. The model call still occurs, but without the tool
result the request was resolving.

The same recovery shape also means a panic from `Cache.Set` discards an
already-successful executor result, whereas an ordinary `Set` error is
best-effort and preserves that result.

**Required before GO**

Place separate recovery boundaries around `Get`, `Executor`, and `Set`.
Treat a recovered `Get` panic as a miss, and treat a recovered `Set` panic as
best-effort failure while preserving the executor value.

---

### [Major] Empty `Query` still leaves retrieved context after the newest message

**File:** `providers/anthropic/client.go:250-297`

**Contract:** `spec.md` §2.4.6

**Claim**

Anthropic serialization always emits:

```text
static → history → retrieved context → newest message
```

**Reality**

The newest message is held back only inside `if req.Query != ""`. A valid
request shape with the current turn represented by the final message and an
empty `Query` leaves that message in `conv`; retrieved chunks are then appended
after it.

**Concrete input**

```go
req := &pipeline.Request{
    Messages: []pipeline.Message{
        {Role: "system", Content: "stable"},
        {Role: "user", Content: "current question"},
    },
    RetrievedChunks: []pipeline.Chunk{
        {Content: "retrieved evidence"},
    },
}
```

**Observed wire order**

```text
1. user: current question
2. user: Retrieved context / retrieved evidence
```

**Impact**

The provider sees a different order depending on whether callers duplicate
the current turn into `Query`. Empty-query requests are plausible for
tool-resolution and preassembled-message callers, and the `Request` contract
does not prohibit them.

**Required before GO**

Determine the newest message from `Messages` independently of whether `Query`
is populated, while continuing to avoid duplicating the current turn when both
representations are present.

## 3. QA round 1 findings — resolved at their original inputs

The six findings below are retained as the historical round 1 record. Their
exact original inputs all pass on revision `b5cdd38`; round 2 findings above
are additional boundary cases discovered while verifying the fixes.

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

## 4. Verification results

### Baseline

The required root-module baseline passed:

```bash
go build ./...
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build ./...
```

### Nested modules

Both nested modules passed under the race detector:

```bash
cd stores/pgvector && go test -race ./...
cd toolcache/redis && go test -race ./...
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
| `rag` | 97.7% |
| `cache` | 97.2% |
| `pipeline` | 97.0% |
| `stores/mock` | 96.7% |
| `providers/anthropic` | 95.1% |
| `providers/cli` | 94.3% |
| `toolcache` | 94.7% |
| `config` | 90.5% |
| `lazyload` | 88.1% |

The five packages named by the specification all exceed the required 80%.

## 5. Areas reviewed with no demonstrated defect

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

## 6. Release decision

**Current decision: NO-GO**

The six original round 1 criteria are satisfied at their reported inputs. The
decision can change to **GO** when the round 2 findings are complete:

1. Every method on a caller-supplied preprocess `Rule`, including `Name`, is
   contained by the fail-open boundary.
2. Cache `Get` panics degrade to misses, and cache `Set` panics preserve
   successfully executed tool results.
3. Anthropic serialization preserves dynamic-before-newest ordering when
   `Query` is empty and the newest turn is carried by `Messages`.
4. Regression tests cover these three cases.
5. The full root and nested-module verification commands pass again.
