package core

// Benchmark to pick the lowest-alloc, byte-identical replacement for
// topLevelKeys' map[string]json.RawMessage decode.
// Candidates: (current) map decode; (token) Decoder.Token walk; (valid) json.Valid
// validation + map decode only for the key set. The Token walk risks allocating
// MORE than the map decode because it boxes every nested token into interface{};
// this benchmark settles it on a realistic ~50KB OpenAI chat body.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func bigChatBody() []byte {
	var msgs []any
	for range 40 {
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": strings.Repeat("the quick brown fox ", 60), // ~1.2KB each → ~48KB
		})
	}
	b, _ := json.Marshal(map[string]any{
		"model": "gpt-4o-mini", "stream": false, "temperature": 0.7,
		"max_tokens": 1024, "messages": msgs,
	})
	return b
}

// tokenWalkKeys: candidate (b) — full-document validation via Token walk, dedupe
// via map, trailing-garbage rejection via final EOF check.
func tokenWalkKeys(raw []byte) map[string]struct{} {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	t, err := dec.Token()
	if err != nil {
		return nil
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil
	}
	out := make(map[string]struct{})
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := kt.(string)
		if !ok {
			return nil
		}
		out[key] = struct{}{}
		if err := skipOneValue(dec); err != nil {
			return nil
		}
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) { // reject trailing data (match Unmarshal)
		return nil
	}
	return out
}

func skipOneValue(dec *json.Decoder) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); ok && (d == '{' || d == '[') {
		depth := 1
		for depth > 0 {
			tt, err := dec.Token()
			if err != nil {
				return err
			}
			if dd, ok := tt.(json.Delim); ok {
				if dd == '{' || dd == '[' {
					depth++
				} else {
					depth--
				}
			}
		}
	}
	return nil
}

// validThenMapKeys: candidate (c) — json.Valid first (zero-alloc scan), then the
// existing map decode for keys. (Validation is redundant with the map decode, so
// this is only here to measure json.Valid's own cost.)
func validThenMapKeys(raw []byte) map[string]struct{} {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(trimmed, &m) != nil {
		return nil
	}
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

func TestTokenWalkParity(t *testing.T) {
	cases := [][]byte{
		bigChatBody(),
		[]byte(`{"a":1,"b":2,"a":3}`),   // duplicate key
		[]byte(`{"a":1}trailing`),       // trailing garbage
		[]byte(`{"a":{"b":1.}}`),        // malformed nested
		[]byte(`[1,2,3]`),               // array root
		[]byte(`{"a":1}`),               // simple
		append([]byte(`{"a":1}`), 0x00), // NUL padded
	}
	for i, c := range cases {
		want := topLevelKeys(c)
		got := tokenWalkKeys(c)
		if !sameKeySet(want, got) {
			t.Errorf("case %d: token-walk diverged from map decode: want=%v got=%v body=%q", i, want, got, c)
		}
	}
}

func sameKeySet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func BenchmarkTopLevelKeys(b *testing.B) {
	body := bigChatBody()
	b.Run(fmt.Sprintf("current_map_%dB", len(body)), func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = topLevelKeys(body)
		}
	})
	b.Run("token_walk", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = tokenWalkKeys(body)
		}
	})
	b.Run("valid_then_map", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = validThenMapKeys(body)
		}
	})
}
