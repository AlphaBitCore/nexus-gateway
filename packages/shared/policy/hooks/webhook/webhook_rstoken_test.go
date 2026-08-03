package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureTokenServer returns an httptest server that records the X-RS-Token
// header of the single request it serves and replies with an approve decision.
func captureTokenServer(t *testing.T, seen *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Get("X-RS-Token")
		_, _ = w.Write([]byte(`{"decision":"approve"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// staticBases wraps a fixed base list in the TrustedAIGuardBases provider shape.
func staticBases(bases ...string) func(context.Context) []string {
	return func(context.Context) []string { return bases }
}

// optionsHook builds a webhook-forward hook via NewWebhookForwardWithOptions
// with the unguarded DefaultClient so Execute can dial the 127.0.0.1 test
// server (the SSRF guard is covered elsewhere).
func optionsHook(t *testing.T, endpoint string, opts Options) Hook {
	t.Helper()
	opts.Client = http.DefaultClient
	h, err := NewWebhookForwardWithOptions(&HookConfig{
		ID:               "wh-rs",
		ImplementationID: "webhook-forward",
		Name:             "test-webhook",
		Config:           map[string]any{"endpoint": endpoint},
	}, opts)
	if err != nil {
		t.Fatalf("NewWebhookForwardWithOptions: %v", err)
	}
	return h
}

// The trusted AI-Guard compliance-webhook endpoint receives the internal
// X-RS-Token so AI-Guard's rstokenauth gate accepts the call instead of 401ing.
func TestWebhookForward_Execute_InjectsRSTokenForTrustedAIGuard(t *testing.T) {
	var seen string
	srv := captureTokenServer(t, &seen)

	h := optionsHook(t, srv.URL+AIGuardComplianceWebhookPath, Options{
		InternalToken:       "internal-secret-123",
		TrustedAIGuardBases: staticBases(srv.URL),
	})
	if _, err := h.Execute(context.Background(), &HookInput{Stage: "request"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen != "internal-secret-123" {
		t.Fatalf("X-RS-Token = %q; want the injected internal token", seen)
	}
}

// A trailing slash on the endpoint path still resolves to the compliance-
// webhook and authenticates — path comparison is slash-insensitive.
func TestWebhookForward_Execute_InjectsRSTokenTrailingSlash(t *testing.T) {
	var seen string
	srv := captureTokenServer(t, &seen)

	h := optionsHook(t, srv.URL+AIGuardComplianceWebhookPath+"/", Options{
		InternalToken:       "tok",
		TrustedAIGuardBases: staticBases(srv.URL),
	})
	if _, err := h.Execute(context.Background(), &HookInput{Stage: "request"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen != "tok" {
		t.Fatalf("X-RS-Token = %q; want token on trailing-slash compliance path", seen)
	}
}

// When several bases are trusted (public + private URL of the gateway), a
// match against any one of them injects the token.
func TestWebhookForward_Execute_InjectsRSTokenForSecondBase(t *testing.T) {
	var seen string
	srv := captureTokenServer(t, &seen)

	h := optionsHook(t, srv.URL+AIGuardComplianceWebhookPath, Options{
		InternalToken:       "tok",
		TrustedAIGuardBases: staticBases("https://public.example.com", srv.URL),
	})
	if _, err := h.Execute(context.Background(), &HookInput{Stage: "request"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen != "tok" {
		t.Fatalf("X-RS-Token = %q; want token when the second trusted base matches", seen)
	}
}

// Security-critical: an endpoint that carries the compliance-webhook PATH but
// on a host other than the trusted AI-Gateway must NOT receive the [MUST MATCH]
// internal token — otherwise an admin typo / malicious webhook URL would
// exfiltrate the secret.
func TestWebhookForward_Execute_NoRSTokenForUntrustedHost(t *testing.T) {
	var seen string
	srv := captureTokenServer(t, &seen) // the request actually lands here...

	h := optionsHook(t, srv.URL+AIGuardComplianceWebhookPath, Options{
		InternalToken:       "internal-secret-123",
		TrustedAIGuardBases: staticBases("https://real-gateway.example.com"), // ...but trust is a DIFFERENT host
	})
	if _, err := h.Execute(context.Background(), &HookInput{Stage: "request"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen != "" {
		t.Fatalf("X-RS-Token = %q; token leaked to an untrusted host", seen)
	}
}

// A trusted host but a NON-compliance path must not receive the token — the
// token is scoped to the specific AI-Guard endpoint, not the whole gateway.
func TestWebhookForward_Execute_NoRSTokenForOtherPath(t *testing.T) {
	var seen string
	srv := captureTokenServer(t, &seen)

	h := optionsHook(t, srv.URL+"/v1/chat/completions", Options{
		InternalToken:       "tok",
		TrustedAIGuardBases: staticBases(srv.URL),
	})
	if _, err := h.Execute(context.Background(), &HookInput{Stage: "request"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen != "" {
		t.Fatalf("X-RS-Token = %q; token sent to a non-AI-Guard path", seen)
	}
}

// With no internal token (or no trusted-bases provider) the hook posts
// unauthenticated, exactly as before this feature — the compliance-webhook
// simply is not wired.
func TestWebhookForward_Execute_NoTokenWhenUnconfigured(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"no token", Options{TrustedAIGuardBases: nil /* filled with srv.URL below */}},
		{"no trusted bases provider", Options{InternalToken: "tok"}},
		{"neither", Options{}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			srv := captureTokenServer(t, &seen)
			opts := tc.opts
			if i == 0 {
				opts.TrustedAIGuardBases = staticBases(srv.URL)
			}
			h := optionsHook(t, srv.URL+AIGuardComplianceWebhookPath, opts)
			if _, err := h.Execute(context.Background(), &HookInput{Stage: "request"}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if seen != "" {
				t.Fatalf("X-RS-Token = %q; want no token when unconfigured", seen)
			}
		})
	}
}

// The trusted-base decision is PER REQUEST: a provider that has nothing
// resolved yet (nil) means no token on that request — fail-safe, no error —
// and once the provider starts returning the matching base (the peer's URL
// arrived from the Hub) the very next Execute on the SAME hook instance
// injects the token. Proves the injection decision moved from construction
// time to request time.
func TestWebhookForward_Execute_PerRequestBaseResolution(t *testing.T) {
	var seen string
	srv := captureTokenServer(t, &seen)

	var resolved []string // nil = "not resolved yet"
	h := optionsHook(t, srv.URL+AIGuardComplianceWebhookPath, Options{
		InternalToken:       "tok",
		TrustedAIGuardBases: func(context.Context) []string { return resolved },
	})

	// First request: provider returns nil → unauthenticated, no failure.
	seen = "unset"
	if _, err := h.Execute(context.Background(), &HookInput{Stage: "request"}); err != nil {
		t.Fatalf("Execute (unresolved): %v", err)
	}
	if seen != "" {
		t.Fatalf("X-RS-Token = %q before the base resolved; want no token", seen)
	}

	// The Hub-resolved base arrives → the same hook now authenticates.
	resolved = []string{srv.URL}
	seen = "unset"
	if _, err := h.Execute(context.Background(), &HookInput{Stage: "request"}); err != nil {
		t.Fatalf("Execute (resolved): %v", err)
	}
	if seen != "tok" {
		t.Fatalf("X-RS-Token = %q after the base resolved; want the token", seen)
	}
}

// A malformed or empty trusted base is peer-reported data, not admin intent:
// it must never match (no token), and must not error the request.
func TestWebhookForward_Execute_MalformedTrustedBaseNeverMatches(t *testing.T) {
	for _, bad := range []string{"not a url", "no-scheme.example.com", "https://", ""} {
		t.Run(bad, func(t *testing.T) {
			var seen string
			srv := captureTokenServer(t, &seen)
			h := optionsHook(t, srv.URL+AIGuardComplianceWebhookPath, Options{
				InternalToken:       "tok",
				TrustedAIGuardBases: staticBases(bad),
			})
			if _, err := h.Execute(context.Background(), &HookInput{Stage: "request"}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if seen != "" {
				t.Fatalf("X-RS-Token = %q; malformed base %q must never authenticate", seen, bad)
			}
		})
	}
}

func TestEndpointMatchesTrustedBase(t *testing.T) {
	const base = "https://api.taskforce10x.com"
	// Endpoint side is pre-parsed at construction; mirror that here.
	cases := []struct {
		name        string
		epScheme    string
		epHost      string
		trustedBase string
		want        bool
	}{
		{"exact match", "https", "api.taskforce10x.com", base, true},
		{"scheme case-insensitive", "HTTPS", "api.taskforce10x.com", base, true},
		{"host case-insensitive", "https", "API.TaskForce10x.com", base, true},
		{"different host", "https", "evil.example.com", base, false},
		{"different scheme", "http", "api.taskforce10x.com", base, false},
		{"host with port differs", "https", "api.taskforce10x.com:8443", base, false},
		{"base with matching port", "https", "api.taskforce10x.com:8443", base + ":8443", true},
		{"malformed base", "https", "api.taskforce10x.com", "not a url", false},
		{"scheme-less base", "https", "api.taskforce10x.com", "api.taskforce10x.com", false},
		{"empty base", "https", "api.taskforce10x.com", "", false},
		{"path on base is ignored for host compare", "https", "api.taskforce10x.com", base + "/some/path", true},
		// Default-port normalization: the base is a reported/derived value the
		// operator no longer hand-matches to the endpoint, so an explicit
		// default port on either side must not break the trust match.
		{"endpoint explicit :443 matches portless https base", "https", "api.taskforce10x.com:443", base, true},
		{"base explicit :443 matches portless https endpoint", "https", "api.taskforce10x.com", base + ":443", true},
		{"endpoint explicit :80 matches portless http base", "http", "gw.internal:80", "http://gw.internal", true},
		{"http :443 is NOT a default port (no normalization)", "http", "gw.internal:443", "http://gw.internal", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := endpointMatchesTrustedBase(tc.epScheme, tc.epHost, tc.trustedBase)
			if got != tc.want {
				t.Fatalf("endpointMatchesTrustedBase(%q, %q, %q) = %v; want %v",
					tc.epScheme, tc.epHost, tc.trustedBase, got, tc.want)
			}
		})
	}
}
