# QA & Verification Brief — tokipe

**Audience:** independent reviewers, human or agent.

**Scope:** this brief is the living procedure and now covers two deliveries.

| Delivery | Covers | Status |
|---|---|---|
| [DELIVERY-1.md](DELIVERY-1.md) | The original spec, Phases 0–4 | Verified over five QA rounds; see [QA-REPORT.md](QA-REPORT.md) |
| [DELIVERY-2.md](DELIVERY-2.md) | Phases 5–8 and the module rename | **Awaiting verification — this is the work in front of you** |

Delivery 1's areas (§2 A–G) still apply: the code beneath them changed. Start
with §2H, which covers only what is new.

You are checking work you did not write. Assume nothing in this document is
true until you have run something that shows it. Where a claim here is wrong,
that is itself a finding worth reporting.

---

## 0. Ground rules

**Report what you can demonstrate.** A finding should name a file and line, and
either a failing command, a concrete input that produces wrong output, or a
spec clause the code contradicts. "This looks fragile" is not a finding; "with
`args` containing a NaN float, `HashToolCall` returns X and the spec requires
Y" is.

**Do not re-report the known gaps in DELIVERY-1 §3 or DELIVERY-2 §3.** They are
deliberate and documented. *Do* report if you find one of them to be worse than
described, or if one exists that is not listed — that has happened before, and
finding it is worth more than confirming the rest.

**Do not fix what you find.** Report it. A reviewer who edits the code being
reviewed destroys the independence that makes the review worth anything.

**Severity, and be honest about it:**

| Severity | Meaning |
|---|---|
| **Blocker** | Data loss, a security hole, a broken fail-open guarantee, or a false claim in a delivery document's evidence table |
| **Major** | Wrong behaviour under a plausible input; a spec contract violated |
| **Minor** | Correct but misleading, undocumented, or awkward to use |
| **Note** | Observation, question, or improvement idea for a later phase |

Over-reporting severity is not caution — it costs the next reader real time.

## 1. Getting oriented

Read in this order. Do not skip to the code.

1. [spec.md](spec.md) §1 (why) and §2 (the contract) — the source of truth
2. [DELIVERY-2.md](DELIVERY-2.md) — what this delivery claims, what it does not,
   and §5, which lists the bugs found while building it. That list is the best
   available map of where the risk actually was.
3. [DELIVERY-1.md](DELIVERY-1.md) — the established baseline
4. [../PLAN.md](../PLAN.md) — decisions and documented deviations
5. [../docs/ROADMAP.md](ROADMAP.md) — why each phase exists and what it taught
6. [../README.md](../README.md) — the intended shape from a caller's view
7. `pipeline/stage.go`, then `pipeline/streaming.go` — everything is downstream

Baseline commands, expected to be green before you start. If any fails, stop
and report that first — everything downstream is suspect:

```bash
go build ./... && go vet ./... && go test -race ./...
CGO_ENABLED=0 go build ./...
go run ./benchmarks
```

## 2. Verification areas

Each area below names what to check and the property that must hold. Pick up
whichever area you are assigned; they are independent.

### A. The fail-open guarantee (spec §2.5.1) — highest value

This is the load-bearing promise: **no optimization may cause `Pipeline.Run` to
fail.** The only errors that may escape are from the final `ModelClient.Send`,
or a nil required dependency.

- For every stage, force its dependency to fail — cache backend down, embedder
  erroring, compressor returning an error, tool executor panicking, parser
  returning garbage — and confirm the turn still reaches the model.
- Look for any `return nil, err` in a stage's `Process`. Each one is either a
  bug or needs a comment justifying it.
- Check the seams too: `config.Stages()` with half-configured options, a nil
  `metrics.Recorder`, an empty tier list.

### B. Cache-breakpoint placement (spec §2.4.6) — highest risk

The non-negotiable rule: **never anchor a breakpoint after content that changes
every turn.** Getting this wrong is worse than not caching, because it pays the
cache-write premium on every call and never reads.

- `cache/breakpoint.go` — is `computeBreakpoints` genuinely pure and
  deterministic? Try to find an input where two identical requests produce
  different breakpoints.
- Try to construct a `Request` where a breakpoint lands at or after the
  retrieved-chunk segment. The tests assert it cannot; try to prove them wrong.
- `config.Stages()` claims the ordering is un-misconfigurable. Try to
  misconfigure it — particularly via `WithStage`.

### C. Determinism (spec §2.5.4)

- `toolcache.HashToolCall` — same args, different construction order, nested
  maps, slices of maps, numeric types (`int` vs `float64` vs `json.Number`),
  `nil` values, non-UTF-8 strings. Any pair that *should* hash the same but
  does not is a correctness bug; any pair that should differ but collides is
  worse.
- `cli.RenderPrompt` — must be byte-identical across runs for identical input.
  Map iteration order is the classic leak.

### D. Concurrency

- `go test -race ./...` is necessary, not sufficient. The race detector only
  sees what executes.
- `toolcache/singleflight.go` — a hand-rolled singleflight, written because the
  core is stdlib-only. Read it adversarially: panics, a leader that never
  returns, a follower arriving as the leader deletes the key, `inFlight`
  leaking entries.
- Shared-state components: `MemoryCache`, `Aligner`, `Stage`, `HeuristicRouter`.
  Any of them holding per-request state across goroutines is a Blocker.

### E. Security

- `providers/cli` executes subprocesses. Verify no path reaches a shell.
  Check argv construction, `PromptPlaceholder` substitution, and `Config.Env`.
- `lazyload.FileLoader` — path traversal. Try `..`, absolute paths, symlinks
  out of the root, `.` tricks, and platform-specific separators.
- `stores/pgvector` — identifier validation is the only defence against
  injection, since identifiers cannot be parameterised. Try to defeat it.
- Anywhere an API key could reach a log, an error string, `Name()`, or a
  process argument.

### F. Claims in the delivery evidence tables

Re-run each one. Do not take the table's word for it. The benchmark in
particular: read `benchmarks/main.go`, decide whether the baseline arm is a
fair comparison or a strawman, and say so plainly. A rigged baseline would make
the headline number worthless, so this is worth real scrutiny.

### G. Spec conformance

Walk spec §2.3–§2.4 signature by signature against the code. Deviations are
listed in DELIVERY-1 §4 — an *undocumented* deviation is a finding.

## 2H. Delivery 2 areas

Ranked by where this delivery's risk actually sits, not by how much code each
one is.

### H1. History trimming and the static prefix — highest risk

`history.Stage` is the only thing in the library that *removes* content a
caller supplied. Getting it wrong is expensive in two different directions.

- **The static prefix must be byte-identical before and after trimming.**
  `cache.Aligner` anchors its breakpoint there; moving one byte forfeits every
  cache hit, and a cache write costs *more* than an ordinary token — so a bug
  here is worse than not trimming at all. Try to construct any request where a
  static message is altered, reordered or dropped.
- **The newest message must never be dropped.** It is the question being asked.
- Trimming removes the *middle*, keeping head and tail. Check the boundary
  arithmetic in `candidates()`: off-by-one there silently drops a turn that
  retention was supposed to protect.
- The stage mutates the Request in place but must never write through the
  caller's `Messages` backing array. There is a test; try to defeat it.
- A `Summarizer` is caller code that usually calls a model. Check the headroom
  reservation (`trimTarget`), and what happens when a summary is larger than
  what it replaced, errors, panics, or returns whitespace.

### H2. Streaming resource cleanup

Every streaming path holds something that must be released: an HTTP body, a
subprocess, a cancel func. The failure mode is a leak under load, which no unit
test will show you.

- Abandon a stream mid-iteration (`break` out of the range loop) for each of
  `providers/anthropic`, `providers/cli`, `providers/openai`. Is the body
  closed, the subprocess reaped, the context cancelled?
- `providers/cli` reaps with `cmd.Wait()` exactly once — a second call would
  mask the real exit status. Verify that holds on every path, including the
  scanner-error path.
- Errors split by timing: returned when nothing was produced, yielded when text
  had already arrived. Check both providers agree, and that partial text
  survives a mid-stream failure.
- `Run` and `RunStream` share `prepare`. Try to make them disagree about stage
  order, short-circuit handling, routing, or the shaped request.

### H3. Observability sinks

- A Phase 7 bug had `metrics.Or`'s safety wrapper hiding every optional
  interface, silently discarding all histograms, gauges and degradations. It is
  fixed and pinned — but look for the same *class* elsewhere: anywhere a
  wrapper implements a subset of what it wraps.
- Every fail-open site should report a degradation. Find one that does not.
  Grep for the fail-open returns and compare against the `metrics.Degrade`
  calls.
- `Degradation.Err` must never reach a metric label — unbounded strings are a
  cardinality explosion. Check `metrics/otel` in particular.
- Diagnostics must never break a turn. `TestObservabilitySinkPanicsAreContained`
  covers the methods; check nothing else calls a recorder unguarded.

### H4. Compression and dedupe — the "silently wrong" pair

Both of these *remove* content, and both fail quietly if they are wrong.

- `compress.CodeCompressor` must never lose a declaration or emit invalid Go.
  It re-parses its own output and compares declaration counts; try to find
  input where that check passes but the output is wrong anyway. Generic type
  parameters, build tags, `//go:embed`, cgo preamble comments and struct tags
  are the interesting cases.
- String literals and raw strings that *look like* comments must survive. That
  is the whole reason it is AST-based.
- `compress.DedupeStage` drops chunks. The dangerous direction is a false
  positive — discarding something the model needed. Try to make two genuinely
  different chunks score above the threshold: repeated boilerplate headers,
  templated text, short chunks near the word floor, non-Latin scripts where
  word splitting behaves differently.

### H5. The OpenAI provider

- Wire order must match the Anthropic adapter's: system, history, retrieved
  context, then the newest turn exactly once. Both the `Query`-set and
  `Query`-empty representations.
- `CacheBreakpoints` are advisory and must not be transmitted. Confirm nothing
  leaks onto the wire.
- An empty `APIKey` is legitimate (local servers). Confirm no `authorization`
  header is sent, and that no key ever reaches `Name()` or an error string.
- Usage is absent, not zero, when a server omits it.

### H6. The module rename

- `"tokipe.stage_latency_ms"` and `"tokipe.stage_degraded"` are metric
  names, not import paths, and must NOT have been rewritten. A blanket rename
  would have broken every dashboard and alert rule built on them.
- No stale `tokipe/...` import path anywhere buildable.
  `docs/spec.md` is deliberately excluded — it is the historical brief.
- All four `go.mod` files and their `replace` directives agree.
- `v1.0.0` and `v1.0.0-agentkit-path` point where DELIVERY-2 §4.8 says.

### H7. Claims, again

Rounds 4 and 5 of the last review found false *claims* rather than defective
code — a coverage claim and a test-count claim. Both are now machine-checked
(`TestMatrixCoversEveryExtensionMethod`, `TestDeliveryInventoryIsCurrent`).

Attack those guards directly:

- Can the extension matrix's completeness test pass while something is
  uncovered? Its exemption list is prose; check every entry is true.
- Can the inventory guard pass while README's numbers are wrong?
- Does DELIVERY-2 §2 reproduce exactly? Every row is a command.

## 3. Environment notes

- Go 1.23+. `-race` needs cgo; the `CGO_ENABLED=0` check is build-only.
- Module path is `github.com/JMjirapat/tokipe`. Nothing is published, so
  `go get` will not resolve it; work from a local checkout.
- Three nested modules are separate and not covered by a root-level
  `go test ./...`:

  ```bash
  cd stores/pgvector && go test -race ./...
  cd toolcache/redis && go test -race ./...
  cd metrics/otel    && go test -race ./...
  ```

- Tests requiring credentials or external services, all skipping by default:

  | Test | Gate | Cost |
  |---|---|---|
  | `TestPromptCachingAcrossTurns` | `ANTHROPIC_API_KEY` | two billed API calls |
  | `TestLiveCLIs` | `TOKIPE_CLI_LIVE=1` | subscription quota, ~15s |
  | pgvector integration | `TOKIPE_PGVECTOR_DSN` | needs a database |
  | `TestLiveCLIStreaming` | `TOKIPE_CLI_LIVE=1` | subscription quota, ~20s |

- `examples/cli-provider` and `examples/streaming` default to mocks or a dry
  run and exit 0 with no CLI installed. `-live` / `-cli` spend real quota.
- `examples/observability -break` deliberately fails every dependency. All
  turns must still succeed; that is the assertion, not a bug.

## 4. Reporting format

One finding per entry. Group by severity, highest first.

```
[Severity] Short title
  File:    path/to/file.go:123
  Claim:   what the code or docs assert
  Reality: what actually happens
  Repro:   the exact command or test that shows it
  Impact:  who is hurt and how
```

Close with a plain **go / no-go** for tagging v1.0.0, and say what would change
it. If your area was clean, say that explicitly — a silent area is
indistinguishable from an unexamined one.

## 5. Out of scope for this delivery

Not defects; do not report them:

- Phase 9 items: a learned router, traffic-derived preprocess rules,
  per-model compression tuning. ROADMAP defers these to v2 pending production
  data.
- Bedrock: dropped from Phase 8 with a documented reason (SigV4 needs the AWS
  SDK, which the stdlib-only core cannot take).
- Tool-result summarisation: deferred, because those results live in Metadata
  as arbitrary `any` and truncating them safely needs a contract that does not
  exist yet.
- The root package being `tokipe` at a path ending in `tokipe` — a known open
  decision in PLAN.md, not a defect.
- Nothing being pushed yet
- Style preferences with no behavioural consequence
