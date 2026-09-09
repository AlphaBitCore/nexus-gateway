package codec

import (
	"strings"
	"testing"
)

// Turning on stream_options.include_usage is not an internal detail: it makes
// the upstream emit an extra terminal chunk that reaches the caller's stream. A
// caller who did not ask for it receives a frame they did not send for, and
// until this was recorded the coercion ledger reported every other field this
// codec fills while staying silent about these — which makes the entries it does
// carry read as the complete list.
func TestEnsureStreamUsage_RecordsWhatItFilled(t *testing.T) {
	t.Run("caller sent neither flag", func(t *testing.T) {
		payload := map[string]any{"model": "gpt-4o"}
		coerced := ensureStreamUsage(payload)

		if payload["stream"] != true {
			t.Fatalf("stream = %v, want true", payload["stream"])
		}
		opts, _ := payload["stream_options"].(map[string]any)
		if opts == nil || opts["include_usage"] != true {
			t.Fatalf("stream_options = %v, want include_usage true", payload["stream_options"])
		}
		assertHas(t, coerced, "stream_options.include_usage→")
		// `stream` is filled here too and deliberately not reported: the caller
		// asked to stream out of band, so writing it on this wire tells them
		// nothing they did not decide. Reporting it would attach a coercion
		// header to nearly every streaming request and bury the entry that
		// matters.
		for _, c := range coerced {
			if strings.HasPrefix(c, "stream→") {
				t.Fatalf("recorded %q: the caller's own streaming intent is not a coercion, "+
					"and reporting it drowns the one that is", c)
			}
		}
	})

	t.Run("caller already asked for both — nothing filled, nothing recorded", func(t *testing.T) {
		payload := map[string]any{
			"model":          "gpt-4o",
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
		}
		coerced := ensureStreamUsage(payload)
		if len(coerced) != 0 {
			t.Fatalf("recorded %v for a caller who set both — a ledger that reports "+
				"changes it did not make is as misleading as one that omits changes it did",
				coerced)
		}
	})

	t.Run("caller opted OUT of usage — the override is recorded", func(t *testing.T) {
		payload := map[string]any{
			"model":          "gpt-4o",
			"stream":         true,
			"stream_options": map[string]any{"include_usage": false},
		}
		coerced := ensureStreamUsage(payload)
		opts, _ := payload["stream_options"].(map[string]any)
		if opts["include_usage"] != false {
			t.Fatalf("include_usage = %v: an explicit false was overwritten, which changes "+
				"the caller's stream without them asking", opts["include_usage"])
		}
		if len(coerced) != 0 {
			t.Fatalf("recorded %v though nothing was changed", coerced)
		}
	})

	t.Run("a non-object stream_options is discarded, and the discard is recorded", func(t *testing.T) {
		payload := map[string]any{
			"model":          "gpt-4o",
			"stream":         true,
			"stream_options": "not-an-object",
		}
		coerced := ensureStreamUsage(payload)
		if _, ok := payload["stream_options"].(map[string]any); !ok {
			t.Fatalf("stream_options = %T, want it replaced with an object", payload["stream_options"])
		}
		// Losing what the caller sent is the part that must not be silent.
		assertHas(t, coerced, "stream_options→replaced_non_object")
	})
}

func assertHas(t *testing.T, got []string, prefix string) {
	t.Helper()
	for _, g := range got {
		if strings.HasPrefix(g, prefix) {
			return
		}
	}
	t.Fatalf("no coercion recorded starting %q; got %v — the field was filled on the "+
		"caller's behalf and the ledger did not say so", prefix, got)
}
