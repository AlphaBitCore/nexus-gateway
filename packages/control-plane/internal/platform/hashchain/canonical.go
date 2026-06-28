// Package hashchain is the platform's tamper-evidence recipe: a canonical
// JSON encoder plus a SHA256 hash chain. Every chained audit log (the chat
// session revision chain today) folds links with ChainHash over a Canonicalize
// envelope and verifies with VerifyLinks, so there is one recipe and one set
// of tests.
package hashchain

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ErrNonCanonical is returned for values that have no stable canonical form:
// non-finite floats (NaN/±Inf, which JSON cannot represent) and structures too
// deep to bound. Callers treat it as a hard error, never a silent coercion.
var ErrNonCanonical = errors.New("value has no canonical JSON form")

// maxDepth bounds recursion so a pathological nested value cannot blow the Go
// stack while canonicalizing (defense-in-depth; sandbox value caps already
// bound size).
const maxDepth = 256

// Canonicalize returns the canonical JSON encoding of raw: object keys sorted
// lexicographically at every level, all insignificant whitespace removed, and
// numbers in a single stable form. Two values that are equal as JSON documents
// always produce byte-identical output — the property the hash chain relies on.
//
// Determinism notes:
//   - Go map iteration order is randomized; we sort keys at every object level.
//   - encoding/json renders floats with the shortest round-trip form, but to
//     pin it across Go versions we re-render numbers ourselves via
//     strconv.AppendFloat(g, -1, 64) on the parsed value, and emit integral
//     values without a trailing ".0".
//   - Non-finite floats cannot appear in valid JSON input, but a hand-built
//     value marshalled upstream could carry one; we reject rather than guess.
func Canonicalize(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		// goccy/go-json validates json.Number magnitude eagerly at decode time, so a
		// number with no finite float64 form (e.g. 1e400 → out of range) fails here
		// rather than in encodeNumber. goccy stringifies the strconv error into its
		// own SyntaxError (errors.As/Is don't see the strconv type), so match the
		// ParseFloat signature: such a value has no canonical JSON form, so classify
		// it as ErrNonCanonical (not a generic decode fault) — matching how
		// encodeNumber reports the same condition for numbers that reach it.
		if strings.Contains(err.Error(), "ParseFloat") {
			return nil, fmt.Errorf("%w: unrepresentable number: %w", ErrNonCanonical, err)
		}
		return nil, fmt.Errorf("canonicalize: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("canonicalize: trailing data after JSON value")
	}
	var buf bytes.Buffer
	if err := encode(&buf, v, 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encode(buf *bytes.Buffer, v any, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: nesting exceeds %d levels", ErrNonCanonical, maxDepth)
	}
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		encodeString(buf, t)
	case json.Number:
		return encodeNumber(buf, string(t))
	case float64:
		return encodeNumber(buf, strconv.FormatFloat(t, 'g', -1, 64))
	case map[string]any:
		return encodeObject(buf, t, depth)
	case []any:
		return encodeArray(buf, t, depth)
	default:
		return fmt.Errorf("%w: unsupported type %T", ErrNonCanonical, v)
	}
	return nil
}

func encodeObject(buf *bytes.Buffer, m map[string]any, depth int) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		encodeString(buf, k)
		buf.WriteByte(':')
		if err := encode(buf, m[k], depth+1); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func encodeArray(buf *bytes.Buffer, a []any, depth int) error {
	buf.WriteByte('[')
	for i, e := range a {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := encode(buf, e, depth+1); err != nil {
			return err
		}
	}
	buf.WriteByte(']')
	return nil
}

// encodeNumber renders a numeric token in one stable form: integral values as
// plain integers (no ".0", no exponent), everything else via the shortest
// round-trip float form. This keeps 1, 1.0, and 1e0 — all equal as numbers —
// canonicalizing identically.
func encodeNumber(buf *bytes.Buffer, tok string) error {
	f, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		return fmt.Errorf("%w: unparseable number %q", ErrNonCanonical, tok)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("%w: non-finite number %q", ErrNonCanonical, tok)
	}
	if f == 0 {
		f = 0 // normalize -0 to 0 so the sign never reaches canonical bytes
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		buf.WriteString(strconv.FormatFloat(f, 'f', -1, 64))
		return nil
	}
	buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	return nil
}

// encodeString writes a JSON string with Go's standard escaping but WITHOUT
// the HTML escaping json.Marshal applies by default (which would render <, >, &
// as < etc.), so canonical bytes are minimal and independent of that
// cosmetic choice.
func encodeString(buf *bytes.Buffer, s string) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // strings always encode
	out := tmp.Bytes()
	buf.Write(out[:len(out)-1]) // Encode appends a newline; trim it
}
