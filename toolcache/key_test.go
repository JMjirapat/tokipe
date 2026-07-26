package toolcache_test

import (
	"context"
	"errors"
	"testing"

	"agentkit/pipeline"
	"agentkit/toolcache"
)

// Regression for QA-REPORT.md finding 3: encoding/json replaces each invalid
// UTF-8 byte with U+FFFD, so distinct byte strings marshalled to identical
// JSON and hashed identically. The cache would then serve one tool call's
// result for a different call.
func TestInvalidUTF8IsRejectedRatherThanCollapsed(t *testing.T) {
	cases := map[string]map[string]any{
		"invalid value":            {"path": string([]byte{0xff})},
		"invalid key":              {string([]byte{0xff}): "v"},
		"invalid nested value":     {"a": map[string]any{"b": string([]byte{0xfe})}},
		"invalid inside a slice":   {"a": []any{"ok", string([]byte{0xff})}},
		"invalid deep in a struct": {"a": struct{ S string }{S: string([]byte{0xff})}},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := toolcache.HashToolCall("read", args); !errors.Is(err, toolcache.ErrUncacheable) {
				t.Fatalf("err = %v, want ErrUncacheable", err)
			}
		})
	}

	if _, err := toolcache.HashToolCall(string([]byte{0xff}), map[string]any{}); !errors.Is(err, toolcache.ErrUncacheable) {
		t.Errorf("an invalid tool name should also be rejected, got %v", err)
	}
}

// The exact pair from the QA report, which previously produced one digest.
func TestDistinctInvalidBytesDoNotShareADigest(t *testing.T) {
	a, errA := toolcache.HashToolCall("read", map[string]any{"path": string([]byte{0xff})})
	b, errB := toolcache.HashToolCall("read", map[string]any{"path": string([]byte{0xfe})})

	if errA == nil || errB == nil {
		t.Fatalf("both must be rejected; got %q/%v and %q/%v", a, errA, b, errB)
	}
	if a != "" || b != "" {
		t.Errorf("a rejected call must return no digest, got %q and %q", a, b)
	}
}

// Valid multi-byte UTF-8 must keep working, and []byte stays cacheable because
// JSON encodes it as base64, which is lossless.
func TestValidUTF8AndBytesRemainCacheable(t *testing.T) {
	cases := map[string]map[string]any{
		"thai":       {"q": "สวัสดีชาวโลก"},
		"emoji":      {"q": "🔑🚀"},
		"cjk":        {"q": "日本語のテキスト"},
		"raw bytes":  {"blob": []byte{0xff, 0xfe, 0x00}},
		"nil inside": {"a": nil},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := toolcache.HashToolCall("t", args); err != nil {
				t.Fatalf("should hash cleanly, got %v", err)
			}
		})
	}

	x, _ := toolcache.HashToolCall("t", map[string]any{"q": "café"})
	y, _ := toolcache.HashToolCall("t", map[string]any{"q": "cafe"})
	if x == y {
		t.Error("distinct valid strings must not collide")
	}
}

// Rejection means "do not cache", never "do not run". An uncacheable call must
// still reach the tool — the fail-open contract seen from the key layer.
func TestUncacheableArgsStillExecuteTheTool(t *testing.T) {
	execs := 0
	st := toolcache.NewStage(toolcache.NewMemoryCache(),
		func(context.Context, pipeline.ToolCall) (any, error) {
			execs++
			return "ran", nil
		})

	req := &pipeline.Request{ToolCalls: []pipeline.ToolCall{
		{Name: "read", Args: map[string]any{"path": string([]byte{0xff})}},
	}}
	out, err := st.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if execs != 1 {
		t.Fatalf("executor ran %d times, want 1 — uncacheable must not mean unexecuted", execs)
	}
	results, ok := out.Metadata[toolcache.MetaResults].(map[string]any)
	if !ok || len(results) != 1 {
		t.Fatalf("the result should still be published, got %v", out.Metadata[toolcache.MetaResults])
	}
}
