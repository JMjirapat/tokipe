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

## Phase 6 — Context budget enforcement

**Gap found in the shipped code, not the spec.** `budget.Policy.BudgetFor`
returns a number and *nothing consumes it* — the only caller in the tree is an
example that prints it. Meanwhile `Request.Messages` grows without limit, and
`compress` only ever touches `RetrievedChunks`.

For the spec's own first target use case — "long-running, multi-turn, heavy
tool use, growing context" (§1.5.1) — unbounded history is the dominant cost.
This is the largest remaining token win in the library, larger than anything
v1 shipped.

- [ ] `history.Stage` that fits a request to `Policy.BudgetFor(TurnType)`:
      drop or summarise oldest turns, always preserving the static prefix.
- [ ] **Must not break cache alignment.** Trimming from the front changes the
      prefix and invalidates the provider cache on the very turn it trims.
      Trim from the middle-out, keeping the static prefix byte-identical, and
      test that property explicitly — this is the same non-negotiable rule as
      §2.4.6 seen from the other side.
- [ ] Ordering: after `toolcache`, before `cache.Aligner`. `config.Stages()`
      owns it as usual.
- [ ] A real token counter behind an interface (`TokenCounter`), with a
      char-estimate default and a provider-backed implementation
      (`/v1/messages/count_tokens`) in the anthropic package. The benchmark's
      4-chars-per-token estimate becomes a fallback rather than the only option.
- [ ] Tool results already resolved should be summarisable too — verbose tool
      output is a large share of an agent loop's context.

**DoD:** a 100-turn synthetic agent loop stays under a fixed budget; the static
prefix is byte-identical across every turn; the benchmark reports the
additional saving separately from v1's.

## Phase 7 — Production observability

**Why it follows Phase 6 rather than leading:** fail-open means a broken
optimization is silent by design. That is right for availability and wrong for
operations — today a dead cache backend degrades every request and nothing
says so. Phase 6's claims also need measurement in production to be believed.

- [ ] A degradation channel: when a stage fails open, it reports *why*, without
      failing the turn. A callback or an event on the `Recorder`, not a log
      line — the library must not choose a logger for its host.
- [ ] Extend `metrics` beyond counters: histograms for stage latency and token
      counts, a gauge for cache hit rate. Additive to `Recorder`; the no-op
      default stays the default.
- [ ] OpenTelemetry adapter in its own nested module, so the core stays
      stdlib-only.
- [ ] Per-stage timing, to answer "is this pipeline paying for itself?" with
      data rather than a synthetic benchmark.

**DoD:** an example dashboard-in-a-terminal shows hit rate, short-circuit rate,
per-stage latency, and degradation events for a running workload.

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
