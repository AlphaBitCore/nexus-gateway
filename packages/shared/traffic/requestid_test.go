package traffic

import (
	"net/http"
	"testing"
)

// TestResolveRequestID_AcceptsEitherSpelling pins the compatibility contract a
// caller relies on: the canonical name and the industry-conventional alias are
// two spellings of one id, the canonical one wins when both arrive, and an
// absent id is reported as absent rather than invented here.
//
// The "both spellings, different values" case is the one that matters. A caller
// whose framework stamps x-request-id automatically AND who sets the Nexus name
// deliberately has said which one they mean; reading the alias there would file
// their traffic under an id they did not choose.
func TestResolveRequestID_AcceptsEitherSpelling(t *testing.T) {
	tests := []struct {
		name  string
		hdrs  map[string]string
		want  string
		leads string
	}{
		{
			name:  "canonical only",
			hdrs:  map[string]string{"X-Nexus-Request-Id": "nexus-1"},
			want:  "nexus-1",
			leads: "the Nexus spelling is honoured as sent",
		},
		{
			name:  "alias only",
			hdrs:  map[string]string{"X-Request-Id": "conventional-1"},
			want:  "conventional-1",
			leads: "a caller who never heard of Nexus is still understood",
		},
		{
			name: "both present, canonical wins",
			hdrs: map[string]string{
				"X-Nexus-Request-Id": "nexus-1",
				"X-Request-Id":       "conventional-1",
			},
			want:  "nexus-1",
			leads: "the deliberate spelling beats the one a framework stamped",
		},
		{
			name:  "neither present",
			hdrs:  map[string]string{},
			want:  "",
			leads: "absence is reported, not filled in — only a service that owns the response may mint",
		},
		{
			name:  "canonical blank falls through to the alias",
			hdrs:  map[string]string{"X-Nexus-Request-Id": "   ", "X-Request-Id": "conventional-1"},
			want:  "conventional-1",
			leads: "a whitespace-only header is not a value",
		},
		{
			name:  "surrounding whitespace is trimmed",
			hdrs:  map[string]string{"X-Nexus-Request-Id": "  nexus-1  "},
			want:  "nexus-1",
			leads: "the stored id must match the one echoed on the response",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.hdrs {
				h.Set(k, v)
			}
			if got := ResolveRequestID(h); got != tc.want {
				t.Errorf("ResolveRequestID = %q, want %q — %s", got, tc.want, tc.leads)
			}
		})
	}
}

// TestResolveRequestID_IsCaseInsensitiveOnTheWire guards the one thing a
// hand-written header read gets wrong: HTTP header names are case-insensitive,
// and a caller's SDK may send any casing. http.Header.Get canonicalises, so
// this passes for free — the test exists so a future rewrite to a map lookup
// or a raw scan fails loudly instead of dropping every lowercase caller.
func TestResolveRequestID_IsCaseInsensitiveOnTheWire(t *testing.T) {
	h := http.Header{}
	h.Set("x-nexus-request-id", "lowercase-sender")
	if got := ResolveRequestID(h); got != "lowercase-sender" {
		t.Errorf("ResolveRequestID = %q, want the value regardless of header casing", got)
	}
}
