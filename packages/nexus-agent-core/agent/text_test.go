package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCutText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short passes through untouched", "hello", 60, "hello"},
		{"ascii cuts exactly at the byte cap", "abcdef", 3, "abc"},
		// The incident shape: a 3-byte CJK char straddles the cap. A byte slice
		// would leave a torn lead byte (0xe7…) that Postgres rejects; the cut
		// must back up to the rune boundary instead.
		{"CJK straddling the cap backs up to the rune boundary", "网关网关", 7, "网关"},
		{"cap on a rune boundary keeps the full rune", "网关网关", 6, "网关"},
		{"cap smaller than one rune yields empty", "网", 2, ""},
		{"exact length passes through", "网关", 6, "网关"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CutText(tt.in, tt.max)
			if got != tt.want {
				t.Fatalf("CutText(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("CutText(%q, %d) = %q is not valid UTF-8", tt.in, tt.max, got)
			}
		})
	}
}

// The session-title derivation that produced the wedged sync slot: a Chinese
// first message longer than 60 bytes must yield a valid-UTF-8 title.
func TestSessionTitleCJKIsValidUTF8(t *testing.T) {
	msg := strings.Repeat("配", 30) // 90 bytes, cap is 60 — 60 is mid-rune-free here, so shift by an ascii prefix
	sess := &Session{Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "a" + msg}}}}}
	title := sessionTitle(sess)
	if !utf8.ValidString(title) {
		t.Fatalf("sessionTitle produced invalid UTF-8: %q", title)
	}
	if !strings.HasSuffix(title, "…") {
		t.Fatalf("long title must end with the ellipsis: %q", title)
	}
}
