package core

import "testing"

// TestIsAuditOnlySentinelAddress pins the denylist discriminator: only the known
// audit-only sentinel is recognised; every other address (a real resolvable address,
// the empty string, an unknown form) is reported applicable — the fail-safe direction
// for the CarriesRedaction predicate (unknown → applicable → over-block, never leak).
func TestIsAuditOnlySentinelAddress(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{AddressAuditOnlySentinel, true},
		{"webhook.flat", true}, // the sentinel's literal value, pinned for drift
		{"messages.0.content.0", false},
		{"messages.0.content.0.toolUse.input.1", false},
		{"http.bodyView", false},
		{"", false},
		{"webhook.flatx", false},
	}
	for _, c := range cases {
		if got := IsAuditOnlySentinelAddress(c.addr); got != c.want {
			t.Errorf("IsAuditOnlySentinelAddress(%q)=%v want %v", c.addr, got, c.want)
		}
	}
}
