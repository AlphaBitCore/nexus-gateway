package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/access"
)

// stubPublicResolver is a hermetic DNS stub that always returns a single
// public IP (1.2.3.4). Used by every server-package test driving
// ProxyServer.ServeHTTP, because the production checker.CheckConnect path
// calls net.DefaultResolver.LookupIPAddr on the CONNECT target (typically
// "example.com" in tests). On a sandboxed CI runner without outbound DNS,
// that cgoLookupHostIP call blocks ~15s and the test fails on timeout.
// The stub returns a non-private IP so the private-IP gate passes.
type stubPublicResolver struct{}

func (stubPublicResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.IPv4(1, 2, 3, 4)}}, nil
}

// newCheckerForTest builds an access.Checker for the unlisted-passthrough
// tests. ipAllow is the source-IP allowlist; domainAllow is the domain
// allowlist (empty means everything fails the domain gate, which is the
// scenario the flag targets). The returned checker has its DNS resolver
// stubbed so ServeHTTP-driven tests never touch real outbound DNS.
func newCheckerForTest(t *testing.T, ipAllow, domainAllow []string) *access.Checker {
	t.Helper()
	c, err := access.NewChecker(ipAllow, domainAllow, nil)
	if err != nil {
		t.Fatalf("access.NewChecker: %v", err)
	}
	c.SetResolverForTest(stubPublicResolver{})
	return c
}

// TestServeHTTP_UnlistedPassthrough_Disabled_Returns403 asserts that with the
// flag off, an unlisted CONNECT still returns 403 (production behavior).
func TestServeHTTP_UnlistedPassthrough_Disabled_Returns403(t *testing.T) {
	checker := newCheckerForTest(t, []string{"10.0.0.0/8"}, nil)

	p := &ProxyServer{
		logger:                   discardLogger(),
		checker:                  checker,
		allowUnlistedPassthrough: false,
	}

	req := newConnectRequest("www.openai.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (flag off must keep reject behavior)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "rejected_domain") {
		t.Fatalf("body = %q, want it to mention rejected_domain", w.Body.String())
	}
}

// TestServeHTTP_UnlistedPassthrough_Enabled_TakesTunnelPath asserts that with
// the flag on, an unlisted CONNECT is routed through the tunnel path instead
// of being rejected. httptest.NewRecorder is not an http.Hijacker, so
// establishTunnel responds with 500 — that 500 is the marker that the
// passthrough branch ran (any 4xx would mean we took the reject branch).
func TestServeHTTP_UnlistedPassthrough_Enabled_TakesTunnelPath(t *testing.T) {
	checker := newCheckerForTest(t, []string{"10.0.0.0/8"}, nil)

	p := &ProxyServer{
		logger:                   discardLogger(),
		checker:                  checker,
		allowUnlistedPassthrough: true,
	}

	req := newConnectRequest("www.openai.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		t.Fatalf("status = 403, want non-reject path (flag on must downgrade to passthrough)")
	}
	// The recorder cannot hijack, so establishTunnel returns 500.
	// That confirms we entered the passthrough branch instead of returning 403.
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (recorder-not-hijacker marker for tunnel branch)", w.Code)
	}
}

// TestServeHTTP_UnlistedPassthrough_IPDenied_StillRejected asserts that the
// flag does not bypass the IP-allowlist gate. IP-denial is a security check,
// not an allowlist miss, and must remain enforced even in unlisted-passthrough
// mode.
func TestServeHTTP_UnlistedPassthrough_IPDenied_StillRejected(t *testing.T) {
	// 10.0.0.1 (the request's RemoteAddr) is NOT in this allowlist.
	checker := newCheckerForTest(t, []string{"192.168.0.0/16"}, nil)

	p := &ProxyServer{
		logger:                   discardLogger(),
		checker:                  checker,
		allowUnlistedPassthrough: true,
	}

	req := newConnectRequest("www.openai.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (IP gate must remain enforced)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "rejected_ip") {
		t.Fatalf("body = %q, want it to mention rejected_ip", w.Body.String())
	}
}

// TestServeHTTP_UnlistedPassthrough_ListedHost_TakesNormalPath asserts that
// when the flag is on but the target IS in the domain allowlist, the request
// takes the standard accepted path — i.e. the flag does not change behavior
// for listed hosts.
func TestServeHTTP_UnlistedPassthrough_ListedHost_TakesNormalPath(t *testing.T) {
	checker := newCheckerForTest(t, []string{"10.0.0.0/8"}, []string{"api.openai.com"})

	p := &ProxyServer{
		logger:                   discardLogger(),
		checker:                  checker,
		allowUnlistedPassthrough: true,
	}

	req := newConnectRequest("api.openai.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	// Listed host passes access control; reaches establishTunnel which 500s
	// against the recorder. A 403 here would mean the access check rejected
	// it spuriously.
	if w.Code == http.StatusForbidden {
		t.Fatalf("status = 403, want non-reject path for listed host")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (recorder-not-hijacker marker)", w.Code)
	}
}
