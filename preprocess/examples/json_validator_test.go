package examples_test

import (
	"context"
	"strings"
	"testing"

	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/preprocess"
	"github.com/JMjirapat/tokipe/preprocess/examples"
)

func TestJSONValidatorHandlesQuery(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		can     bool
		want    string
		nilResp bool
	}{
		{name: "object", query: `{"a":1,"b":2}`, can: true, want: "valid JSON: object with 2 key(s)"},
		{name: "array", query: `[1,2,3]`, can: true, want: "valid JSON: array with 3 element(s)"},
		{name: "string", query: `"hi"`, can: true, want: "valid JSON: string"},
		{name: "number", query: `3.5`, can: true, want: "valid JSON: number"},
		{name: "bool", query: `true`, can: true, want: "valid JSON: boolean"},
		{name: "null", query: `null`, can: true, want: "valid JSON: null"},
		{name: "negative", query: `-1`, can: true, want: "valid JSON: number"},
		{name: "broken", query: `{"a":}`, can: true, want: "invalid JSON:"},
		{name: "prose", query: `explain quantum tunnelling`, can: false},
		{name: "empty", query: ``, can: false},
	}

	rule := examples.JSONValidator{}
	var _ preprocess.Rule = rule

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &pipeline.Request{Query: tc.query}
			if got := rule.CanHandle(req); got != tc.can {
				t.Fatalf("CanHandle = %v, want %v", got, tc.can)
			}
			if !tc.can {
				return
			}
			resp, err := rule.Handle(req)
			if err != nil {
				t.Fatalf("Handle returned error: %v", err)
			}
			if !strings.HasPrefix(resp.Content, tc.want) {
				t.Errorf("Content = %q, want prefix %q", resp.Content, tc.want)
			}
			if resp.ModelUsed != "json_validator" {
				t.Errorf("ModelUsed = %q", resp.ModelUsed)
			}
		})
	}
}

func TestJSONValidatorPrefix(t *testing.T) {
	rule := examples.JSONValidator{Prefix: "validate json:", RuleName: "vj"}
	if rule.Name() != "vj" {
		t.Errorf("Name = %q", rule.Name())
	}

	if rule.CanHandle(&pipeline.Request{Query: `{"a":1}`}) {
		t.Error("payload without the prefix must not be claimed")
	}
	if rule.CanHandle(&pipeline.Request{Query: `validate`}) {
		t.Error("query shorter than the prefix must not be claimed")
	}

	req := &pipeline.Request{Query: `  Validate JSON: {"a":1} `}
	if !rule.CanHandle(req) {
		t.Fatal("prefixed payload should be claimed (case insensitive)")
	}
	resp, err := rule.Handle(req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.Content != "valid JSON: object with 1 key(s)" {
		t.Errorf("Content = %q", resp.Content)
	}
}

func TestJSONValidatorMetadataKey(t *testing.T) {
	rule := examples.JSONValidator{MetadataKey: "payload"}

	t.Run("string value", func(t *testing.T) {
		req := &pipeline.Request{Query: "ignored"}
		req.SetMeta("payload", `[1]`)
		if !rule.CanHandle(req) {
			t.Fatal("CanHandle = false")
		}
		resp, _ := rule.Handle(req)
		if resp.Content != "valid JSON: array with 1 element(s)" {
			t.Errorf("Content = %q", resp.Content)
		}
	})

	t.Run("bytes value", func(t *testing.T) {
		req := &pipeline.Request{}
		req.SetMeta("payload", []byte(`{}`))
		if !rule.CanHandle(req) {
			t.Fatal("CanHandle = false for []byte")
		}
	})

	t.Run("missing and wrong type", func(t *testing.T) {
		if rule.CanHandle(&pipeline.Request{Query: `{}`}) {
			t.Error("missing metadata key must not be claimed")
		}
		req := &pipeline.Request{}
		req.SetMeta("payload", 42)
		if rule.CanHandle(req) {
			t.Error("non string/[]byte metadata must not be claimed")
		}
		if resp, err := rule.Handle(req); resp != nil || err != nil {
			t.Errorf("Handle with no payload = %v, %v; want nil, nil", resp, err)
		}
	})

	if rule.CanHandle(nil) {
		t.Error("nil request must not be claimed")
	}
}

func TestJSONValidatorInStage(t *testing.T) {
	st := preprocess.NewStage(examples.JSONValidator{})
	req := &pipeline.Request{Query: `{"ok":true}`}

	got, err := st.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	resp, ok := got.Metadata[pipeline.MetaShortCircuit].(*pipeline.Response)
	if !ok {
		t.Fatal("expected a short circuit")
	}
	if !resp.ShortCircuited {
		t.Error("ShortCircuited = false")
	}
}
