package compress

import (
	"bytes"
	"encoding/json"
	"strings"
)

// JSONCompressor re-marshals JSON-shaped content in its most compact form
// (all insignificant whitespace removed) and optionally drops caller-nominated
// low-value fields.
//
// Guarantees:
//   - It never invents content. Every retained key and every scalar value
//     survives byte-for-byte; numbers are decoded with json.Decoder.UseNumber
//     so that large integers and trailing zeros are not silently reformatted
//     through float64.
//   - It never emits invalid JSON: output comes from encoding/json.Marshal of
//     a value that was decoded from valid JSON.
//   - CanHandle returns false for anything that does not parse, so malformed
//     content falls through to the next compressor untouched.
type JSONCompressor struct {
	// drop is the set of object keys removed at any nesting depth.
	drop map[string]struct{}
}

// NewJSONCompressor builds a JSONCompressor. dropFields names object keys to
// remove at any depth (e.g. "_links", "etag", "raw_html"). Passing none makes
// the compressor purely a whitespace stripper.
func NewJSONCompressor(dropFields ...string) *JSONCompressor {
	drop := make(map[string]struct{}, len(dropFields))
	for _, f := range dropFields {
		if f != "" {
			drop[f] = struct{}{}
		}
	}
	return &JSONCompressor{drop: drop}
}

// CanHandle reports whether content is a valid JSON object or array.
//
// Bare scalars ("42", `"hi"`, "null") are valid JSON but there is nothing to
// compress and treating stray prose numerals as JSON would steal chunks from
// the text compressor, so only objects and arrays are claimed.
func (c *JSONCompressor) CanHandle(content string) bool {
	t := strings.TrimSpace(content)
	if len(t) < 2 {
		return false
	}
	switch t[0] {
	case '{', '[':
	default:
		return false
	}
	return json.Valid([]byte(t))
}

// Compress re-marshals content compactly, dropping configured fields.
func (c *JSONCompressor) Compress(content string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(content))
	dec.UseNumber() // preserve numeric literals exactly

	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}

	if len(c.drop) > 0 {
		v = c.prune(v)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // fewer bytes, identical semantics
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	// json.Encoder.Encode appends a newline.
	return strings.TrimRight(buf.String(), "\n"), nil
}

// prune removes dropped keys recursively. It only ever deletes; it never adds
// or rewrites a value.
func (c *JSONCompressor) prune(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k := range t {
			if _, drop := c.drop[k]; drop {
				delete(t, k)
				continue
			}
			t[k] = c.prune(t[k])
		}
		return t
	case []any:
		for i := range t {
			t[i] = c.prune(t[i])
		}
		return t
	default:
		return v
	}
}

var _ Compressor = (*JSONCompressor)(nil)
