package httpclient

import (
	"strings"
	"testing"
)

// TestRedactQueryString covers the inbound half of the redaction, whose absence
// meant every OIDC login wrote its authorization code to the access log.
func TestRedactQueryString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"nothing sensitive is returned verbatim", "page=2&limit=50", "page=2&limit=50"},
		{
			name: "the IdP callback's authorization code and login handle are both redacted",
			in:   "code=4%2F0AVG7fiQ_secret&state=authctx-abc123",
			want: "code=%2A%2A%2A&state=%2A%2A%2A",
		},
		{
			name: "an unparseable query is redacted whole, not passed through",
			in:   "code=%zz",
			want: "***",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactQueryString(tc.in); got != tc.want {
				t.Fatalf("RedactQueryString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactQueryString_KeepsNonSensitiveNeighbours pins that redaction is
// per-parameter: an operator debugging a failed callback must still see which
// IdP and which error the request carried.
func TestRedactQueryString_KeepsNonSensitiveNeighbours(t *testing.T) {
	got := RedactQueryString("idp=okta&code=SECRET&error=access_denied")
	for _, want := range []string{"idp=okta", "error=access_denied", "code=%2A%2A%2A"} {
		if !strings.Contains(got, want) {
			t.Fatalf("RedactQueryString dropped or failed to redact: got %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "SECRET") {
		t.Fatalf("the authorization code survived redaction: %q", got)
	}
}

// TestSensitiveQueryParams_CoversTheOAuthFamily pins the membership the outbound
// redactor and the inbound one now share. The list is the single place the two
// directions agree, and it is what the two directions disagreeing looked like.
func TestSensitiveQueryParams_CoversTheOAuthFamily(t *testing.T) {
	for _, name := range []string{"code", "state", "authctx", "id_token", "refresh_token", "client_secret", "session_state"} {
		if !isSensitiveParamName(name) {
			t.Errorf("%q is not treated as sensitive", name)
		}
	}
	// Matching is exact, so a longer name that merely contains one of these
	// must NOT be redacted — over-redaction of ordinary fields costs debugging.
	for _, name := range []string{"errorCode", "statusCode", "stateName", "nodeId"} {
		if isSensitiveParamName(name) {
			t.Errorf("%q was redacted; matching must be exact", name)
		}
	}
}
