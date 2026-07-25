package toolcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// HashToolCall returns the SHA-256 hex digest of a canonical encoding of
// (toolName, args).
//
// Determinism is explicit rather than inherited: args are first round-tripped
// through encoding/json into generic values (with numbers preserved as
// json.Number so 1 and 1.0 keep their written form), and then re-encoded by
// canonicalize, which sorts object keys at *every* level of nesting. Relying
// on encoding/json's own key sorting alone is not enough — a value of a named
// struct type, a json.Marshaler, or a nested map reached through an `any`
// field can all serialize in ways whose stability is not part of the encoding
// contract we want to depend on for a cache key.
//
// An arg value that cannot be represented as JSON (a func, a channel, a NaN)
// makes HashToolCall return an error; callers treat that as "uncacheable" and
// proceed without the cache.
func HashToolCall(toolName string, args map[string]any) (string, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("toolcache: marshal args for %q: %w", toolName, err)
	}

	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return "", fmt.Errorf("toolcache: decode args for %q: %w", toolName, err)
	}

	var buf bytes.Buffer
	buf.WriteString(strconv.Quote(toolName))
	buf.WriteByte('\x00')
	if err := canonicalize(&buf, generic); err != nil {
		return "", fmt.Errorf("toolcache: canonicalize args for %q: %w", toolName, err)
	}

	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// canonicalize writes a deterministic textual encoding of v, sorting object
// keys at every nesting level. v must consist only of the types produced by
// json.Decoder with UseNumber: map[string]any, []any, string, bool,
// json.Number, and nil.
func canonicalize(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case string:
		buf.WriteString(strconv.Quote(t))
	case json.Number:
		buf.WriteString(t.String())
	case []any:
		buf.WriteByte('[')
		for i, elem := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := canonicalize(buf, elem); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(strconv.Quote(k))
			buf.WriteByte(':')
			if err := canonicalize(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical type %T", v)
	}
	return nil
}
