package responses

import "testing"

// An escaped spelling is the same part type, and the downgrade has to see it.
//
// The pre-scan is a cost bound rather than a filter — that is the property the
// walk depends on — and a literal scan alone broke it: gjson resolves \uXXXX
// when it matches, so `{"type":"\u0069nput_audio"}` reaches the same wire the
// plain spelling would, and the caller gets a 400 from a wire that cannot carry
// audio instead of being routed to one that can.
func TestExceedsInputVocabulary_SeesEscapedPartTypes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantHit bool
	}{
		{"plain", `{"input":[{"role":"user","content":[{"type":"input_audio"}]}]}`, true},
		{"first letter escaped", `{"input":[{"role":"user","content":[{"type":"\u0069nput_audio"}]}]}`, true},
		{"underscore escaped", `{"input":[{"role":"user","content":[{"type":"input\u005faudio"}]}]}`, true},
		// A served part whose TEXT carries escapes must still take the cheap
		// path's answer, not be dragged to the walk for nothing... and must
		// certainly not be reported as exceeding the vocabulary.
		{"served part with escapes in the text", `{"input":[{"role":"user","content":[{"type":"input_text","text":"a \n b"}]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			part, exceeds := ExceedsInputVocabulary([]byte(tc.body))
			if exceeds != tc.wantHit {
				t.Fatalf("exceeds = %v (part %q), want %v — an escaped part type is the same "+
					"part type, and missing it sends audio to a wire that cannot carry it",
					exceeds, part, tc.wantHit)
			}
			if tc.wantHit && part != "input_audio" {
				t.Errorf("part = %q, want input_audio — the name must be the resolved one so "+
					"the downgrade can act on it", part)
			}
		})
	}
}
