package tlsbump

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestCouldCarryContent pins which uninspected passthroughs get an audit
// row.
//
// Background: a path-policy PASSTHROUGH sets complianceEnabled=false and
// runResponseStage returns immediately, so the flow left NO audit trail at all —
// measured live, a bumped GET to an intercepted host produced zero traffic_event
// rows across 64s of polling while the proxy log recorded the decision. Its two
// siblings both emit (an exemption grant, an emergency bypass) precisely so the
// absence of inspection is auditable; this was the only one that recorded
// nothing.
//
// The condition is "could this have carried content", not "was the rule explicit
// or the domain default". The seeded explicit PASSTHROUGH rules ARE asset
// patterns (/_next/, /static/, /assets/, /fonts/ on bolt.new), so keying on
// explicitness would emit per asset fetch on the very hosts it was meant to
// spare.
func TestRequestCouldCarryContent(t *testing.T) {
	body := func(method, b string) *http.Request {
		r := httptest.NewRequest(method, "https://api.openai.com/v1/chat/completions", strings.NewReader(b))
		return r
	}
	bodyless := func(method string) *http.Request {
		return httptest.NewRequest(method, "https://api.openai.com/assets/app.js", nil)
	}

	cases := []struct {
		name string
		req  *http.Request
		want bool
		why  string
	}{
		{"POST with a body", body(http.MethodPost, `{"messages":[]}`), true,
			"a decrypted POST body may be a prompt; relaying it uninspected is the event worth recording"},
		{"PUT with a body", body(http.MethodPut, `{"x":1}`), true, "same as POST"},
		{"GET asset", bodyless(http.MethodGet), false,
			"an asset fetch carries nothing a compliance auditor needs, and there are many"},
		{"HEAD", bodyless(http.MethodHead), false, "never carries a body"},
		{"OPTIONS preflight", bodyless(http.MethodOptions), false, "never carries a prompt"},
		{"POST with an empty body", body(http.MethodPost, ""), false,
			"ContentLength 0 — nothing was carried, so nothing was missed"},
		{"nil request", nil, false, "defensive: no request, no claim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestCouldCarryContent(tc.req); got != tc.want {
				t.Errorf("requestCouldCarryContent = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestRequestCouldCarryContent_ChunkedCountsAsContent is separate because the
// value that encodes "unknown length" is -1, and treating it as "no body" would
// silently drop every streamed upload from the audit trail.
func TestRequestCouldCarryContent_ChunkedCountsAsContent(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", strings.NewReader("x"))
	r.ContentLength = -1 // chunked / unknown
	if !requestCouldCarryContent(r) {
		t.Error("a chunked request body must count as content — an unknown length is still a body, " +
			"and a streamed upload is exactly the case where inspection matters most")
	}
}
