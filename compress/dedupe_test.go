package compress_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/JMjirapat/tokipe/compress"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
)

func chunks(contents ...string) []pipeline.Chunk {
	out := make([]pipeline.Chunk, len(contents))
	for i, c := range contents {
		out[i] = pipeline.Chunk{Content: c, SourceURL: fmt.Sprintf("src/%d", i)}
	}
	return out
}

func run(t *testing.T, st *compress.DedupeStage, cs []pipeline.Chunk) *pipeline.Request {
	t.Helper()
	req := &pipeline.Request{RetrievedChunks: cs}
	out, err := st.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return out
}

const passage = "Prompt caching anchors a breakpoint after static content only. " +
	"Dynamic content such as retrieved chunks must never sit before a breakpoint, " +
	"because it changes every turn and invalidates the cached prefix entirely."

func TestDropsExactDuplicates(t *testing.T) {
	rec := metrics.NewInMemory()
	st := compress.NewDedupeStage(compress.WithDedupeMetrics(rec))

	out := run(t, st, chunks(passage, passage, passage))
	if len(out.RetrievedChunks) != 1 {
		t.Fatalf("kept %d chunks, want 1", len(out.RetrievedChunks))
	}
	if got := out.Metadata[compress.MetaDeduped]; got != 2 {
		t.Errorf("MetaDeduped = %v, want 2", got)
	}
	if rec.Count(compress.MetricChunksDeduped) != 2 {
		t.Errorf("counter = %d, want 2", rec.Count(compress.MetricChunksDeduped))
	}
}

// The realistic case: the same passage reformatted, requoted, lightly edited.
func TestDropsNearDuplicates(t *testing.T) {
	reformatted := strings.ReplaceAll(passage, " ", "\n")
	requoted := "> " + strings.ReplaceAll(passage, ". ", ".\n> ")

	out := run(t, compress.NewDedupeStage(), chunks(passage, reformatted, requoted))
	if len(out.RetrievedChunks) != 1 {
		t.Fatalf("kept %d chunks, want 1:\n%+v", len(out.RetrievedChunks), out.RetrievedChunks)
	}
}

func TestKeepsGenuinelyDifferentChunks(t *testing.T) {
	other := "Tool results are keyed by a SHA-256 hash of the tool name and its " +
		"arguments. Argument maps are canonicalised first, so key order never " +
		"changes the resulting hash value at all."
	third := "Every optimization fails open. A compression error, a dead cache " +
		"backend, or an embedding timeout leaves the turn intact and simply " +
		"forfeits that particular optimization entirely."

	out := run(t, compress.NewDedupeStage(), chunks(passage, other, third))
	if len(out.RetrievedChunks) != 3 {
		t.Fatalf("kept %d chunks, want all 3 — different content was treated as duplicate", len(out.RetrievedChunks))
	}
}

// Word-order changes must not read as duplicates. This is why shingles are used
// rather than bare word sets, which would score these two identically.
func TestWordOrderMatters(t *testing.T) {
	a := "cache the prefix and never the suffix because the prefix is what the provider matches on every turn"
	b := "cache the suffix and never the prefix because the suffix is what the provider matches on every turn"

	out := run(t, compress.NewDedupeStage(), chunks(a, b))
	if len(out.RetrievedChunks) != 2 {
		t.Errorf("kept %d, want 2: reordered words must not count as duplicates", len(out.RetrievedChunks))
	}
}

// RAGStage orders chunks by relevance, so the first copy is the most relevant.
// Keeping the first preserves that ordering rather than silently re-ranking.
func TestKeepsTheFirstOccurrence(t *testing.T) {
	first := pipeline.Chunk{Content: passage, SourceURL: "canonical", Similarity: 0.95}
	second := pipeline.Chunk{Content: passage, SourceURL: "copy", Similarity: 0.40}

	out := run(t, compress.NewDedupeStage(), []pipeline.Chunk{first, second})
	if len(out.RetrievedChunks) != 1 {
		t.Fatalf("kept %d, want 1", len(out.RetrievedChunks))
	}
	if out.RetrievedChunks[0].SourceURL != "canonical" {
		t.Errorf("kept %q, want the first (most relevant) copy", out.RetrievedChunks[0].SourceURL)
	}
}

// A chunk too short to judge is kept. Dropping content the model needed is a
// worse failure than sending a duplicate.
func TestShortChunksAreAlwaysKept(t *testing.T) {
	out := run(t, compress.NewDedupeStage(), chunks("yes", "yes", "yes"))
	if len(out.RetrievedChunks) != 3 {
		t.Errorf("kept %d, want all 3 — chunks below the word floor must not be judged", len(out.RetrievedChunks))
	}
}

func TestThresholdIsConfigurable(t *testing.T) {
	a := passage
	b := passage + " An extra sentence that makes this chunk somewhat different from the first one."

	strict := run(t, compress.NewDedupeStage(compress.WithDedupeThreshold(0.99)), chunks(a, b))
	if len(strict.RetrievedChunks) != 2 {
		t.Errorf("a strict threshold kept %d, want 2", len(strict.RetrievedChunks))
	}
	loose := run(t, compress.NewDedupeStage(compress.WithDedupeThreshold(0.4)), chunks(a, b))
	if len(loose.RetrievedChunks) != 1 {
		t.Errorf("a loose threshold kept %d, want 1", len(loose.RetrievedChunks))
	}
}

func TestInvalidOptionsFallBackToDefaults(t *testing.T) {
	// A threshold of 0 or 2 would make the stage drop everything or nothing;
	// neither is a reasonable reading of the caller's intent.
	for _, bad := range []float64{0, -1, 1.5} {
		st := compress.NewDedupeStage(compress.WithDedupeThreshold(bad))
		out := run(t, st, chunks(passage, passage))
		if len(out.RetrievedChunks) != 1 {
			t.Errorf("threshold %v: kept %d, want default behaviour (1)", bad, len(out.RetrievedChunks))
		}
	}
	if st := compress.NewDedupeStage(compress.WithShingleSize(0)); st == nil {
		t.Error("an invalid shingle size should not produce a nil stage")
	}
}

func TestNoDuplicatesLeavesTheRequestUntouched(t *testing.T) {
	req := &pipeline.Request{RetrievedChunks: chunks(passage, "totally different content here that shares no shingles with the other one at all")}
	out, err := compress.NewDedupeStage().Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if out != req {
		t.Error("with nothing to drop the request should be returned as-is")
	}
	if _, ok := out.Metadata[compress.MetaDeduped]; ok {
		t.Error("MetaDeduped should not be set when nothing was dropped")
	}
}

func TestEdgeCasesDoNotPanic(t *testing.T) {
	st := compress.NewDedupeStage()
	cases := map[string]*pipeline.Request{
		"nil request":  nil,
		"no chunks":    {},
		"one chunk":    {RetrievedChunks: chunks(passage)},
		"empty chunks": {RetrievedChunks: chunks("", "", "")},
		"unicode":      {RetrievedChunks: chunks(strings.Repeat("สวัสดีชาวโลก ", 20), strings.Repeat("สวัสดีชาวโลก ", 20))},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := st.Process(context.Background(), req); err != nil {
				t.Errorf("Process: %v", err)
			}
		})
	}
}

func TestCanceledContextLeavesChunksAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := &pipeline.Request{RetrievedChunks: chunks(passage, passage)}
	out, err := compress.NewDedupeStage().Process(ctx, req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(out.RetrievedChunks) != 2 {
		t.Errorf("kept %d; a canceled context should skip the work, not do it", len(out.RetrievedChunks))
	}
}

func TestImplementsPipelineStage(t *testing.T) {
	var s pipeline.Stage = compress.NewDedupeStage()
	if s.Name() != "dedupe" {
		t.Errorf("Name = %q", s.Name())
	}
}

// QA-REPORT-2 M6: two long, otherwise identical passages differing only in a
// decisive value scored ~0.82 and the second was discarded, so the model
// answered from whichever ranked first. A threshold alone cannot see this: a
// decisive value is a few tokens in a long passage.
func TestKeepsChunksThatDisagreeOnAValue(t *testing.T) {
	base := "The production ingress timeout policy applies to all upstream services in the cluster " +
		"and is enforced by the gateway for every request path without exception. The configured value is "

	cases := map[string][2]string{
		"different duration": {base + "30 seconds.", base + "60 seconds."},
		"different version":  {base + "applied from v2 onward.", base + "applied from v3 onward."},
		"different limit":    {base + "capped at 1024 requests.", base + "capped at 4096 requests."},
	}
	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			out := run(t, compress.NewDedupeStage(), chunks(pair[0], pair[1]))
			if len(out.RetrievedChunks) != 2 {
				t.Errorf("kept %d, want both — they disagree on a value", len(out.RetrievedChunks))
			}
		})
	}
}

// The fact guard must not stop genuine duplicates being dropped: identical
// values on both sides is agreement, not disagreement.
func TestIdenticalChunksWithNumbersAreStillDeduped(t *testing.T) {
	withNumbers := "The timeout is 30 seconds and the retry budget is 3 attempts across all 12 " +
		"upstream services configured in the production cluster today without exception."
	out := run(t, compress.NewDedupeStage(), chunks(withNumbers, withNumbers, withNumbers))
	if len(out.RetrievedChunks) != 1 {
		t.Errorf("kept %d, want 1 — these are identical", len(out.RetrievedChunks))
	}
}

func TestDefaultKeepsCriticalNonnumericDifference(t *testing.T) {
	words := make([]string, 300)
	for i := range words {
		words[i] = alphaWord(i)
	}
	common := strings.Join(words, " ") + " final decision "
	out := run(t, compress.NewDedupeStage(), chunks(common+"allow", common+"deny"))
	if len(out.RetrievedChunks) != 2 {
		t.Fatalf("kept %d, want both — opposite policies are not duplicates",
			len(out.RetrievedChunks))
	}
}

// QA Round 3: Jaccard 1.0 over a set of shingles is not exact sequence
// equality. Periodic sequences can have the same shingle set after rotation,
// even though their normalized content and leading policy decision differ.
func TestDefaultRequiresExactNormalizedSequence(t *testing.T) {
	a := strings.TrimSpace(strings.Repeat("allow users deny admins ", 4))
	b := strings.TrimSpace(strings.Repeat("deny admins allow users ", 4))
	out := run(t, compress.NewDedupeStage(), chunks(a, b))
	if len(out.RetrievedChunks) != 2 {
		t.Fatalf("kept %d, want both — normalized sequences differ",
			len(out.RetrievedChunks))
	}
}

func alphaWord(n int) string {
	b := []byte{'w', 'o', 'r', 'd', 'a', 'a', 'a'}
	for i := len(b) - 1; i >= 4; i-- {
		b[i] = byte('a' + n%26)
		n /= 26
	}
	return string(b)
}

// The default threshold is conservative by design: discarding needed evidence
// is worse than sending a duplicate.
func TestDefaultThresholdIsConservative(t *testing.T) {
	if compress.DefaultDedupeThreshold != 1 {
		t.Errorf("DefaultDedupeThreshold = %v, want 1; lossy matching must be opt-in",
			compress.DefaultDedupeThreshold)
	}
}
