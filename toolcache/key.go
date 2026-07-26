package toolcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"unicode/utf8"
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
	// Invalid UTF-8 must be rejected BEFORE marshalling. encoding/json replaces
	// each invalid byte with U+FFFD, so "\xff" and "\xfe" marshal to the same
	// JSON and would hash identically — the cache would then serve one tool
	// call's result for a different call. Lossy encoding is worse than no
	// encoding, so this joins the existing "uncacheable" contract above.
	if !validUTF8(toolName) {
		return "", fmt.Errorf("toolcache: tool name for %q is not valid UTF-8: %w", toolName, ErrUncacheable)
	}
	if err := checkUTF8(reflect.ValueOf(args), 0); err != nil {
		return "", fmt.Errorf("toolcache: args for %q: %w", toolName, err)
	}

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

// ErrUncacheable marks arguments that cannot be hashed faithfully. Callers
// treat it as "do not cache this call" and proceed without the cache, which is
// the fail-open behaviour — never a reason to fail the turn.
var ErrUncacheable = errors.New("uncacheable arguments")

// maxUTF8Depth bounds the validation walk. Argument maps are JSON-shaped and
// shallow in practice; the bound exists so a self-referential value cannot
// spin here before encoding/json would have rejected it anyway.
const maxUTF8Depth = 64

func validUTF8(s string) bool { return utf8.ValidString(s) }

// checkUTF8 walks v and reports the first string, anywhere inside it, that is
// not valid UTF-8. It covers map keys as well as values, because a key with an
// invalid byte collides exactly the same way.
func checkUTF8(v reflect.Value, depth int) error {
	if depth > maxUTF8Depth {
		return fmt.Errorf("nested deeper than %d levels: %w", maxUTF8Depth, ErrUncacheable)
	}
	if !v.IsValid() {
		return nil
	}

	switch v.Kind() {
	case reflect.String:
		if !validUTF8(v.String()) {
			return fmt.Errorf("string value is not valid UTF-8: %w", ErrUncacheable)
		}
	case reflect.Interface, reflect.Pointer:
		if !v.IsNil() {
			return checkUTF8(v.Elem(), depth+1)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if err := checkUTF8(k, depth+1); err != nil {
				return err
			}
			if err := checkUTF8(v.MapIndex(k), depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		// []byte is JSON-encoded as base64, which is lossless, so it is safe.
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := range v.Len() {
			if err := checkUTF8(v.Index(i), depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				if err := checkUTF8(v.Field(i), depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
