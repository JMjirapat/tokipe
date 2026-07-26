# Roadmap — after v1.0.0

v1 delivers the spec. This is what the spec did not cover, ordered by value.
Each phase keeps the v1 interface freeze intact: **everything here is additive.**
If a phase cannot be built additively, that is itself the finding, and it
belongs in a v2 discussion rather than a quiet breaking change.

Two of these come from gaps found by reading the shipped code, not from the
spec. They are called out where they appear.

---

## Phase 5 — Streaming ✅ shipped

**Why first:** it is the only item whose *API shape* interacts with the freeze.
Every other phase adds a stage; this one adds a way to call. Deciding it late
risks discovering that `ModelClient` needed a different shape after it was
frozen — which costs a major version for a library nobody has used yet.

The pipeline architecture already contains the problem, which is the good news:
stages all run *before* the model call, so streaming touches only the final
step. No stage needs to change.

- [x] `StreamingClient` as an **optional** interface a client may also
      implement — `ModelClient` itself stays frozen:
      ```go
      type Delta struct { Text string; Usage *Usage }
      type StreamingClient interface {
          ModelClient
          SendStream(ctx context.Context, req *Request) (iter.Seq2[Delta, error], error)
      }
      ```
      (`iter.Seq2` is stdlib as of Go 1.23, so this costs no dependency.)
- [x] `Pipeline.RunStream` — runs every stage as usual, then streams the final
      call. A preprocess short-circuit yields exactly one `Delta` and closes,
      so callers need no special case.
- [x] Adapter so a non-streaming `ModelClient` still satisfies `RunStream`,
      yielding its whole response as one `Delta`. Callers should never have to
      ask which kind of client they hold.
- [x] `providers/anthropic`: SSE parsing, with `Usage` arriving on the final
      event.
- [x] `providers/cli`: stream stdout line by line. `claude -p --output-format
      stream-json` and `codex exec --json` already emit incrementally; opencode
      does not, so it uses the adapter.

**DoD met.** `examples/streaming` streams through the full stage set; a
short-circuited turn and a non-streaming backend both work through the same call
path; `go test -race` green.

**What the build taught us, beyond the plan:**

- `Run` and `RunStream` now share one `prepare` helper holding the stage loop,
  short-circuit handling and routing. Two copies would have drifted, and that
  is a bug class better designed out than tested for.
- **CLI streaming is not token-level, and cannot be made so.** Measured live:
  `claude --output-format stream-json` emits an assistant message as one block
  (1 delta), `codex exec --json` likewise (1 delta), `opencode` streams a line
  at a time (5 deltas for a five-line answer). The granularity limit is in the
  CLIs' output. A UI that needs per-token updates needs the API backend.
- `providers/cli` requires an explicit `StreamParse`; it is deliberately not
  defaulted, because guessing which lines are answer text and which are
  protocol chatter would show users a CLI's internals as if they were the
  answer. Without it, `SendStream` buffers and yields one delta.
- `Delta.Usage` is non-nil only on the final delta. Anthropic splits usage
  across `message_start` and `message_delta`, so the adapter merges rather than
  overwrites — a later frame must not zero an earlier one.
- The non-streaming request body is unchanged: `stream` is `omitempty`, because
  a changed body would change the prefix the provider hashes for its cache.

## Phase 6 — Context budget enforcement ✅ shipped

**Gap found in the shipped code, not the spec.** `budget.Policy.BudgetFor`
returns a number and *nothing consumes it* — the only caller in the tree is an
example that prints it. Meanwhile `Request.Messages` grows without limit, and
`compress` only ever touches `RetrievedChunks`.

For the spec's own first target use case — "long-running, multi-turn, heavy
tool use, growing context" (§1.5.1) — unbounded history is the dominant cost.
This is the largest remaining token win in the library, larger than anything
v1 shipped.

- [x] `history.Stage` that fits a request to `Policy.BudgetFor(TurnType)`:
      drop or summarise oldest turns, always preserving the static prefix.
- [x] **Must not break cache alignment.** Trimming from the front changes the
      prefix and invalidates the provider cache on the very turn it trims.
      Trim from the middle-out, keeping the static prefix byte-identical, and
      test that property explicitly — this is the same non-negotiable rule as
      §2.4.6 seen from the other side.
- [x] Ordering: after `toolcache`, before `cache.Aligner`. `config.Stages()`
      owns it as usual.
- [x] A real token counter behind an interface (`TokenCounter`), with a
      char-estimate default and a provider-backed implementation
      (`/v1/messages/count_tokens`) in the anthropic package. The benchmark's
      4-chars-per-token estimate becomes a fallback rather than the only option.
- [ ] *Deferred:* tool results should be summarisable too — verbose tool
      output is a large share of an agent loop's context.

**DoD met.** Measured by `go run ./benchmarks`, long-loop section:

```
100 turns, growing context
  no budget      : 195691 billed tokens, peak request 4039 tokens
  history budget :  88930 billed tokens, peak request 1192 tokens (limit 1200)
  reduction      : 54.6%, 185 messages dropped
```

The static prefix is byte-identical across all 100 turns
(`TestHundredTurnLoopStaysUnderBudgetWithAStablePrefix`).

**What the build taught us, beyond the plan:**

- **The roadmap's "middle-out" instinct was right for a reason it did not
  state.** Trimming the oldest turns is the obvious move and it destroys
  automatic prefix caching: providers match the longest common prefix, so
  changing the first non-static bytes means nothing is ever reused. Head and
  tail are kept; the middle goes.
- **Phase 6 adds nothing to the v1 headline workload, and the benchmark says
  so.** Twelve turns with short replies never reach the budget: `+0.0 pp`. The
  long-loop section was added rather than tuning the budget down to manufacture
  a number on a workload the optimization was not built for.
- **A summariser needs reserved headroom or it can never fire.** Trimming stops
  the instant the request fits, leaving no room for the summary, which is then
  rejected for pushing back over budget — every time. Trimming now targets
  `limit - reserve` while the summary is checked against the real limit.
- **The stage mutates the Request in place, like every other stage.** The first
  version returned a copy, which looked safer and was worse: the caller's own
  pointer never saw the trim, so the metadata it wrote was invisible and the
  benchmark measured the untrimmed request while billing the trimmed one.
  Safety comes from building new slices, not from copying the Request.
- **Fixing the benchmark exposed a near-miss of my own.** Rewriting the arm's
  loop briefly stopped recording the user query in the agentkit arm while the
  baseline still did, inflating the headline from 57.1% to 67.4% for no real
  reason. Both arms must record identical content or the comparison is
  worthless — the exact failure QA had flagged as worth scrutinising.
- Tool-result summarisation is deferred rather than done badly: the results live
  in `Metadata` as arbitrary `any`, and truncating them safely needs a contract
  about their shape that does not exist yet.

## Phase 7 — Production observability ✅ shipped

**Why it follows Phase 6 rather than leading:** fail-open means a broken
optimization is silent by design. That is right for availability and wrong for
operations — today a dead cache backend degrades every request and nothing
says so. Phase 6's claims also need measurement in production to be believed.

- [x] A degradation channel: when a stage fails open, it reports *why*, without
      failing the turn. A callback or an event on the `Recorder`, not a log
      line — the library must not choose a logger for its host.
- [x] Extend `metrics` beyond counters: histograms for stage latency and token
      counts, a gauge for cache hit rate. Additive to `Recorder`; the no-op
      default stays the default.
- [x] OpenTelemetry adapter in its own nested module, so the core stays
      stdlib-only.
- [x] Per-stage timing, to answer "is this pipeline paying for itself?" with
      data rather than a synthetic benchmark.

**DoD met.** `go run ./examples/observability` renders all four. Run it with
`-break` to make every optional dependency fail:

```
 RATES
   short-circuit rate    0%   (0/40 turns answered with no model call)
   tool cache hit rate   0%   (0 hits, 10 misses)

 STAGE LATENCY (ms)
   stage           calls    mean    peak
   toolcache          40   0.007   0.249
   ...

 DEGRADATIONS  (80)
   history / counter_failed          ×40
   preprocess / rule_failed          ×10
   rag / embed_failed                ×10
   toolcache / cache_get_panicked    ×10
   toolcache / tool_failed           ×10

 All 40 turns still succeeded.
```

That output is the whole argument for the phase: fail-open kept every turn
alive while the pipeline stopped optimizing entirely, and nothing except the
degradation log would have told you.

**What the build taught us, beyond the plan:**

- **The safety wrapper was hiding the new capabilities.** `metrics.Or` wraps a
  recorder to contain panics, and the wrapper implemented only `Recorder` — so
  every type assertion for the new optional interfaces failed and every
  histogram, gauge and degradation was silently discarded. Caught by a test
  written specifically to check it. Wrapping for safety must not cost
  capability, and that is now pinned by `TestOrPreservesOptionalCapabilities`.
- **Optional interfaces, not a wider `Recorder`.** `Recorder` is the interface
  callers implement; adding methods would have broken every existing
  implementation, including this repository's own examples. Same reasoning that
  kept `ModelClient` frozen when streaming arrived.
- **The degradation sink lives on the `Recorder`.** One object, one wiring
  point. The library still chooses nothing — no logger, no format, no
  destination; it hands over a struct.
- **`Degradation.Err` never becomes a metric attribute.** Error strings are
  unbounded, and that is how a backend gets a cardinality explosion. It reaches
  the handler, where a logger can take it. Asserted in the OTel adapter's tests.
- **A metric nothing can display is not shipped.** Per-stage latency needed the
  in-memory recorder to index observations by label; without that, agentkit
  emitted the metric and no in-tree recorder could show the breakdown the DoD
  asked for.
- Instruments are memoised in the OTel adapter: agentkit asks for the same
  handful of names on every turn, and OTel instrument creation is not free.

## Phase 8 — The deferred v1 items

Cheapest to close now that the surrounding machinery exists.

- [ ] `compress.CodeCompressor`, AST-aware — Go via `go/ast` (stdlib, so it can
      live in core). Other languages need tree-sitter, which is cgo, so they go
      in a nested module.
- [ ] `stores/pgvector` integration tests against a real database in CI.
- [ ] Semantic deduplication of retrieved chunks: near-identical chunks from
      different sources are common in RAG and currently all get sent.
- [ ] More `providers`: OpenAI-compatible endpoints, Bedrock, Ollama. Note that
      `CacheBreakpoints` is advisory for all of them — most providers cache
      automatically, so only the aligner's *reordering* carries over.

## Phase 9 — Adaptive behaviour (v2 territory)

Spec §1.3 rules learned models out of v1. These need production data from
Phase 7 before they are worth attempting, and at least one may need an
interface change — hence v2.

- [ ] A router that learns from outcomes instead of a fixed heuristic:
      escalate when the cheap tier's answer was rejected or retried.
- [ ] Preprocess rules derived from observed traffic rather than hand-written.
- [ ] Compression tuned per-model, since token boundaries differ by tokenizer.

---

## Sequencing note

Phases 5 and 6 are independent and can run in parallel. 7 depends on 6 being
worth measuring. 8 is independent of everything and is good filler work. 9
should not start before 7 has produced real data.

The one hard ordering constraint is that **Phase 5's interface decision should
be made before v1.0.0 is tagged, even if the implementation lands later.**
Adding `StreamingClient` after the freeze is fine because it is additive;
discovering that `ModelClient` itself was wrong is not.
