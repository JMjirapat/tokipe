// Package examples holds reference preprocess.Rule implementations. It is a
// demonstration of the extension point and is deliberately NOT imported by any
// core agentkit package.
package examples

import (
	"encoding/json"
	"fmt"
	"strings"

	"agentkit/pipeline"
)

// JSONValidator is a preprocess.Rule that answers "is this valid JSON?"
// without an LLM. It reads the payload from Request.Query, or from a
// designated Request.Metadata field when MetadataKey is set.
//
// It is a plain struct so it can be constructed as a literal:
//
//	rule := examples.JSONValidator{Prefix: "validate json:"}
type JSONValidator struct {
	// Prefix, when non-empty, is required at the start of the query (case
	// insensitive, leading space trimmed) for the rule to claim the request.
	// The prefix is stripped before validation. Ignored when MetadataKey is
	// set.
	Prefix string

	// MetadataKey, when non-empty, names the Request.Metadata entry holding
	// the payload instead of Query. The value must be a string or []byte.
	MetadataKey string

	// RuleName overrides the reported name. Defaults to "json_validator".
	RuleName string
}

func (v JSONValidator) Name() string {
	if v.RuleName != "" {
		return v.RuleName
	}
	return "json_validator"
}

// payload extracts the bytes to validate and reports whether a payload was
// found at all.
func (v JSONValidator) payload(req *pipeline.Request) (string, bool) {
	if req == nil {
		return "", false
	}
	if v.MetadataKey != "" {
		raw, ok := req.Metadata[v.MetadataKey]
		if !ok {
			return "", false
		}
		switch s := raw.(type) {
		case string:
			return s, true
		case []byte:
			return string(s), true
		default:
			return "", false
		}
	}

	q := strings.TrimSpace(req.Query)
	if v.Prefix != "" {
		if len(q) < len(v.Prefix) || !strings.EqualFold(q[:len(v.Prefix)], v.Prefix) {
			return "", false
		}
		q = strings.TrimSpace(q[len(v.Prefix):])
	}
	return q, true
}

// CanHandle reports whether a payload is present and looks JSON-shaped, i.e.
// it starts with a JSON structural character. Anything else is left to the LLM.
func (v JSONValidator) CanHandle(req *pipeline.Request) bool {
	p, ok := v.payload(req)
	if !ok || p == "" {
		return false
	}
	switch p[0] {
	case '{', '[', '"', 't', 'f', 'n', '-':
		return true
	default:
		return p[0] >= '0' && p[0] <= '9'
	}
}

// Handle validates the payload and returns the verdict as a Response. It never
// returns an error: an invalid document is a successful answer, not a failure.
func (v JSONValidator) Handle(req *pipeline.Request) (*pipeline.Response, error) {
	p, ok := v.payload(req)
	if !ok {
		// Nothing to validate — report "did not match" per the Rule contract.
		return nil, nil
	}

	var out any
	if err := json.Unmarshal([]byte(p), &out); err != nil {
		return &pipeline.Response{
			Content:   fmt.Sprintf("invalid JSON: %v", err),
			ModelUsed: v.Name(),
		}, nil
	}
	return &pipeline.Response{
		Content:   "valid JSON: " + describe(out),
		ModelUsed: v.Name(),
	}, nil
}

// describe names the top-level JSON kind of the decoded value.
func describe(v any) string {
	switch t := v.(type) {
	case map[string]any:
		return fmt.Sprintf("object with %d key(s)", len(t))
	case []any:
		return fmt.Sprintf("array with %d element(s)", len(t))
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return "value"
	}
}
