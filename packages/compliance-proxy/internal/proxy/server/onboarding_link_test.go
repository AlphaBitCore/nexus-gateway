// Onboarding 407 setup-link degradation: the CP-UI base is a per-render
// provider (Hub-resolved default); while it is nil or resolves to "", the 407
// page must still render with a relative link — never panic, never block.
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServeHTTP_OnboardingIntercept_NilCPUIProvider_RendersRelativeLink(t *testing.T) {
	t.Parallel()
	checker := newCheckerForTest(t, []string{"10.0.0.0/8"}, []string{"api.openai.com"})
	p := &ProxyServer{
		logger:                discardLogger(),
		checker:               checker,
		onboardingCPUIBaseURL: nil, // resolver never wired
	}
	p.onboardingEnabled.Store(true)

	req := newConnectRequest("api.openai.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want 407 even without a CP-UI base", w.Code)
	}
	if !strings.Contains(w.Body.String(), `href="/setup/proxy"`) {
		t.Fatalf("body must degrade to the relative setup link, got: %q", w.Body.String())
	}
}

func TestServeHTTP_OnboardingIntercept_EmptyResolvedBase_RendersRelativeLink(t *testing.T) {
	t.Parallel()
	checker := newCheckerForTest(t, []string{"10.0.0.0/8"}, []string{"api.openai.com"})
	p := &ProxyServer{
		logger:                discardLogger(),
		checker:               checker,
		onboardingCPUIBaseURL: func() string { return "" }, // CP URL not resolved yet
	}
	p.onboardingEnabled.Store(true)

	req := newConnectRequest("api.openai.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want 407 while the CP URL is unresolved", w.Code)
	}
	if !strings.Contains(w.Body.String(), `href="/setup/proxy"`) {
		t.Fatalf("body must degrade to the relative setup link, got: %q", w.Body.String())
	}
}
