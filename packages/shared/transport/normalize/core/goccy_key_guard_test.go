package core

import (
	"context"
	"testing"
)

// The guard's condition is DERIVED from goccy's key decoder, so these pin the derivation's
// two non-obvious consequences rather than a list of sampled crashers — sampling is what
// produced C-31a's insufficient guard, which fuzz later bypassed.

func TestMayOverrunGoccyKeyDecoder_Condition(t *testing.T) {
	bs := `\`
	cases := []struct {
		name string
		in   string
		want bool
		why  string
	}{
		{"backslash-last", `{"` + bs, true,
			"the backslash's escapee IS goccy's terminator"},
		{"invalid-escapee-last", `{"mo` + bs + `x`, true,
			"escapee is the final payload byte, so the cursor steps past the terminator"},
		{"c31a-fuzz-finding", `{"` + bs + `00` + bs + `.`, true,
			"the input that bypassed C-31a's trailing-backslash-only guard"},
		{"VALID-escapee-last", `{"a` + bs + `n`, true,
			"a RECOGNIZED escape over-advances identically — gating on invalid escapes only " +
				"would miss this, which is the derivation's first non-obvious consequence"},
		{"escape-then-more-input", `{"mo` + bs + `xdel":1}`, false,
			"one byte after the escape is enough: the cursor lands on real input, not the terminator"},
		{"complete-body-with-escapes", `{"model":"a` + bs + `"b","messages":[]}`, false,
			"well-formed bodies are never refused, however many escapes they carry"},
		{"truncated-unicode-escape", `{"` + bs + `u00`, false,
			"decodeKeyCharByUnicodeRune handles a short \\u without the double increment"},
		{"empty", "", false, "no bytes, no escape"},
		{"single-backslash", bs, true, "degenerate but still the condition"},
	}
	for _, c := range cases {
		if got := mayOverrunGoccyKeyDecoder([]byte(c.in)); got != c.want {
			t.Errorf("%s: mayOverrunGoccyKeyDecoder(%q) = %v, want %v — %s",
				c.name, c.in, got, c.want, c.why)
		}
	}
}

// TestNormalize_RefusesOverrunBodiesFailOpen pins the POSTURE, not just the predicate: a
// refused body must come back as ErrUnsupported so the caller keeps its verbatim fallback
// (B9). Returning a nil payload with a nil error would silently blank the record instead.
func TestNormalize_RefusesOverrunBodiesFailOpen(t *testing.T) {
	reg := NewRegistry()
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}` + `\`)

	payload, err := reg.Normalize(context.Background(), body, Meta{Direction: DirectionRequest})
	if err == nil {
		t.Fatal("a body that can overrun goccy's key decoder must return an error so the " +
			"caller falls back, not a silently empty payload")
	}
	if payload.Kind != "" || payload.Protocol != "" {
		t.Fatalf("payload = %+v, want the zero value alongside the error", payload)
	}
}
