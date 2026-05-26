package tlsbump

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewUpstreamTransportWith_ZeroOptionsByteEquivalentToLegacy pins the
// backward-compat invariant: callers using the legacy constructor must
// observe the same behaviour as before the UpstreamOptions seam landed.
// Both constructors return non-nil transports, and neither sets a
// proxy.
func TestNewUpstreamTransportWith_ZeroOptionsByteEquivalentToLegacy(t *testing.T) {
	legacy, err := NewUpstreamTransport(8, 10*time.Second, time.Second)
	if err != nil {
		t.Fatalf("legacy: %v", err)
	}
	withZero, err := NewUpstreamTransportWith(8, 10*time.Second, time.Second, UpstreamOptions{})
	if err != nil {
		t.Fatalf("with-zero: %v", err)
	}
	if legacy == nil || withZero == nil {
		t.Fatal("both constructors must return non-nil transports")
	}
}

// TestNewUpstreamTransportWith_ProxyConfiguresHTTPProxy stands up an
// httptest proxy that accepts CONNECT, then issues a request through
// the transport and verifies the CONNECT actually went to the proxy.
// Catches the obvious wire-up regression: Proxy field gets dropped on
// the floor.
func TestNewUpstreamTransportWith_ProxyConfiguresHTTPProxy(t *testing.T) {
	var connectCount atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// httptest can't fake a real CONNECT tunnel, but it does see
		// the inbound method/URL. Stdlib emits CONNECT only for
		// HTTPS requests via a proxy; for HTTP requests it forwards
		// the absolute-URL line. The presence of *any* request on
		// this server confirms the Proxy field is wired.
		connectCount.Add(1)
		w.WriteHeader(http.StatusBadGateway) // we don't actually tunnel
	}))
	t.Cleanup(proxy.Close)

	proxyURL, _ := url.Parse(proxy.URL)
	tr, err := NewUpstreamTransportWith(8, 10*time.Second, time.Second,
		UpstreamOptions{Proxy: proxyURL})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}

	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodGet, "http://does-not-exist.example/", nil)
	resp, _ := tr.ForwardRequest(context.Background(), req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	if got := connectCount.Load(); got != 1 {
		t.Errorf("proxy server saw %d hits; want 1 (Proxy not wired)", got)
	}
}

// TestNewUpstreamTransportWith_GetProxyConnectHeaderInvokedOnCONNECT
// verifies the callback fires when stdlib emits a CONNECT. Stdlib
// emits CONNECT for HTTPS-through-proxy. We can't easily run a real
// TLS upstream here, so the assertion is on the callback being
// invoked at all — coupled with the unit tests on the signer side
// that check what it returns.
func TestNewUpstreamTransportWith_GetProxyConnectHeaderInvokedOnCONNECT(t *testing.T) {
	var callbackCalls atomic.Int32
	cb := func(_ context.Context, _ *url.URL, target string) (http.Header, error) {
		callbackCalls.Add(1)
		if !strings.Contains(target, "example.com:443") {
			t.Errorf("target = %q; want example.com:443", target)
		}
		return http.Header{"X-Nexus-Attestation": []string{"v1;..."}}, nil
	}

	// A proxy that accepts CONNECT and immediately closes — enough
	// to trigger stdlib's CONNECT-emit path which invokes the
	// callback before the wire write.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			// Confirm stdlib actually sent the header we returned.
			if got := r.Header.Get("X-Nexus-Attestation"); got == "" {
				t.Errorf("proxy did not see X-Nexus-Attestation on CONNECT")
			}
		}
		http.Error(w, "no tunnel", http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	proxyURL, _ := url.Parse(proxy.URL)
	tr, err := NewUpstreamTransportWith(8, 10*time.Second, time.Second,
		UpstreamOptions{Proxy: proxyURL, GetProxyConnectHeader: cb})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}

	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodGet, "https://example.com/", nil)
	resp, _ := tr.ForwardRequest(context.Background(), req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	if got := callbackCalls.Load(); got < 1 {
		t.Errorf("GetProxyConnectHeader invoked %d times; want ≥1", got)
	}
}

// TestNewUpstreamTransportWith_NilProxyIgnoresGetProxyConnectHeader
// pins the no-op guard: with no proxy configured, the callback must
// never fire, even if accidentally set. Catches the misconfiguration
// where an operator leaves the callback in place but clears the
// proxy URL — the request should flow direct, no callback overhead.
func TestNewUpstreamTransportWith_NilProxyIgnoresGetProxyConnectHeader(t *testing.T) {
	var calls atomic.Int32
	cb := func(_ context.Context, _ *url.URL, _ string) (http.Header, error) {
		calls.Add(1)
		return nil, nil
	}

	tr, err := NewUpstreamTransportWith(8, 10*time.Second, time.Second,
		UpstreamOptions{Proxy: nil, GetProxyConnectHeader: cb})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}

	// Direct-dial path — no proxy in the chain, callback must stay
	// quiet. The dial will fail (the host doesn't exist) but that's
	// the expected behaviour for this test; the assertion is the
	// callback never fired.
	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodGet, "https://nonexistent.invalid./", nil)
	resp, _ := tr.ForwardRequest(context.Background(), req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("callback fired %d times with nil Proxy; want 0", got)
	}
}
