package proxy

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/redact"
)

// A second copy of the extractor in this package keeps
// both defects the shared one fixes: the structured branches
// return the provider's string unbounded, and the raw-body fallback cuts on
// a byte boundary. Neither is caught here unless something in this package
// tests it — the tests live beside the OTHER copy.
//
// So this is not a test of a one-line delegation: it is what goes
// red if someone re-inlines the function, which is how the divergence
// happens.
func TestExtractProviderErrorMessage_InheritsTheSharedBound(t *testing.T) {
	long := strings.Repeat("x", 5000)

	cases := []struct {
		name string
		body string
	}{
		{"structured error.message", `{"error":{"message":"` + long + `"}}`},
		{"top-level message", `{"message":"` + long + `"}`},
		{"unstructured raw body", long},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractProviderErrorMessage([]byte(tc.body), 500, true)
			if len(got) > redact.MaxErrorReasonBytes+3 { // +3 for the ellipsis
				t.Errorf("%s produced %d bytes into traffic_event.error_reason; "+
					"the cap is %d", tc.name, len(got), redact.MaxErrorReasonBytes)
			}
		})
	}
}

// The raw-body fallback here is fed execResult.Body verbatim, so a provider
// can put any bytes in it. Invalid UTF-8 in a text column is rejected by
// PostgreSQL and the audit consumer treats that as permanent, dropping the
// event — a lost audit row is worse than a long one.
//
// The "x" is load-bearing: every multi-byte width divides 300 exactly, so a
// run of one rune type puts a naive cut ON a boundary and this test would
// pass against the very bug it exists to catch.
func TestExtractProviderErrorMessage_NeverReturnsInvalidUTF8(t *testing.T) {
	body := "x" + strings.Repeat("→", 400)
	got := extractProviderErrorMessage([]byte(body), 502, true)
	if !utf8.ValidString(got) {
		t.Errorf("returned invalid UTF-8 (%d bytes): %q", len(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("nothing was truncated, so this cannot show a boundary bug: %q", got)
	}
}
