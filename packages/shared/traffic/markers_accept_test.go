package traffic

import (
	"strings"
	"testing"
)

// TestAcceptHeaders_CarriesEveryGatewayReadHeader pins the business contract
// of the request-direction registry: every header the gateway's own read
// sites depend on is present, so composing the CORS allowlist from this
// slice can never preflight-reject a header the gateway itself needs.
// A name disappearing from this list silently breaks browser callers of
// the matching read site — auth first of all.
func TestAcceptHeaders_CarriesEveryGatewayReadHeader(t *testing.T) {
	want := []string{
		// VK carriers (vkauth.extractVKToken).
		"Authorization",
		"X-Nexus-Virtual-Key",
		"x-api-key",
		"x-goog-api-key",
		"api-key",
		// Correlation.
		"X-Nexus-Request-Id",
		"X-Request-Id",
		"X-Nexus-End-User-Id",
		"X-Nexus-Session-Id",
		"X-Nexus-Client-Tags",
		// Cache opt-out, canonical + deprecated alias (dual-read window).
		"X-Nexus-No-Cache",
		"X-Nexus-Aigw-No-Cache",
		// Body negotiation.
		"Content-Type",
	}
	have := map[string]bool{}
	for _, h := range AcceptHeaders {
		have[strings.ToLower(h)] = true
	}
	for _, w := range want {
		if !have[strings.ToLower(w)] {
			t.Errorf("AcceptHeaders missing %q — a browser client could no longer send it past preflight", w)
		}
	}
	if len(AcceptHeaders) != len(want) {
		t.Errorf("AcceptHeaders has %d entries, want %d — additions belong to a read site; remove-with-the-read-site only", len(AcceptHeaders), len(want))
	}
}

// TestAcceptHeaders_NoDuplicateNames guards the registry against the
// case-variant duplicates that hand-edited header lists accumulate.
func TestAcceptHeaders_NoDuplicateNames(t *testing.T) {
	seen := map[string]string{}
	for _, h := range AcceptHeaders {
		k := strings.ToLower(h)
		if prev, dup := seen[k]; dup {
			t.Errorf("AcceptHeaders lists %q and %q — same header twice", prev, h)
		}
		seen[k] = h
	}
}
