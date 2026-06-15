package tlsbump

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	utls "github.com/refraction-networking/utls"
)

// loopbackUTLSDial returns a fingerprintDialFunc that performs a REAL uTLS
// (Chrome) handshake against a loopback TLS server, offering ALPN [h2,
// http/1.1] exactly like the production fingerprint dial. InsecureSkipVerify is
// test-only — it lets the handshake trust httptest's self-signed cert without
// the production cert-probe path. The returned counter records how many times
// this seam dialed, so a test can prove the dispatcher's per-authority cache
// skips the redundant h2 probe.
func loopbackUTLSDial() (fingerprintDialFunc, *int32) {
	var dials int32
	fn := func(ctx context.Context, network, addr string) (net.Conn, error) {
		atomic.AddInt32(&dials, 1)
		raw, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		host, _, _ := net.SplitHostPort(addr)
		u := utls.UClient(raw, &utls.Config{ServerName: host, InsecureSkipVerify: true}, utls.HelloChrome_Auto)
		if err := u.HandshakeContext(ctx); err != nil {
			_ = u.Close()
			return nil, err
		}
		return u, nil
	}
	return fn, &dials
}

// dispatchForServers wires a protocolDispatchRoundTripper whose h2 path and h1
// path each dial through their own counted uTLS seam, so a test can attribute a
// dial to the protocol that made it.
func dispatchForServers() (rt *protocolDispatchRoundTripper, h2Dials, h1Dials *int32) {
	h2Dial, h2c := loopbackUTLSDial()
	h1Dial, h1c := loopbackUTLSDial()
	base := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return h1Dial(ctx, network, addr)
		},
	}
	return newH2DispatchTransport(base, h2Dial), h2c, h1c
}

func mustGet(t *testing.T, rt http.RoundTripper, rawURL, method, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip %s %s: %v", method, rawURL, err)
	}
	return resp
}

// TestH2Dispatch_NegotiatesH2EndToEnd proves that when the upstream advertises
// h2, the dispatcher routes the request through golang.org/x/net/http2 — the
// upstream handler observes HTTP/2 — and a POST body round-trips intact. This is
// the capability the forced-http/1.1 downgrade used to deny connect-RPC
// streaming agents.
func TestH2Dispatch_NegotiatesH2EndToEnd(t *testing.T) {
	var sawProto atomic.Value
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawProto.Store(r.Proto)
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte("echo:" + string(b)))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	rt, h2Dials, h1Dials := dispatchForServers()
	resp := mustGet(t, rt, srv.URL+"/run", http.MethodPost, "hello-stream")
	defer resp.Body.Close()

	if resp.ProtoMajor != 2 {
		t.Fatalf("response ProtoMajor = %d, want 2 (HTTP/2)", resp.ProtoMajor)
	}
	if got := sawProto.Load(); got != "HTTP/2.0" {
		t.Fatalf("upstream saw %v, want HTTP/2.0", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "echo:hello-stream" {
		t.Fatalf("body = %q, want echo:hello-stream (body did not round-trip over h2)", body)
	}
	addr := authorityOf(t, srv.URL)
	if v, ok := rt.protos.Load(addr); !ok || v.(string) != protoH2 {
		t.Fatalf("protos[%s] = %v (ok=%v), want %q", addr, v, ok, protoH2)
	}
	if n := atomic.LoadInt32(h2Dials); n != 1 {
		t.Fatalf("h2 dials = %d, want 1", n)
	}
	if n := atomic.LoadInt32(h1Dials); n != 0 {
		t.Fatalf("h1 dials = %d, want 0 (h2 host must not touch the h1 transport)", n)
	}
}

// TestH2Dispatch_FallsBackToH1AndCaches proves that an http/1.1-only upstream is
// detected (the h2 probe negotiates http/1.1 -> errUpstreamNotH2), the request
// is transparently retried on the stdlib transport (handler sees HTTP/1.1), and
// a SECOND request to the same authority skips the h2 probe entirely — the
// cache routes straight to h1.
func TestH2Dispatch_FallsBackToH1AndCaches(t *testing.T) {
	var sawProto atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawProto.Store(r.Proto)
		_, _ = w.Write([]byte("h1-ok"))
	}))
	defer srv.Close()

	rt, h2Dials, h1Dials := dispatchForServers()

	// First request: probe h2, get http/1.1, fall back.
	resp := mustGet(t, rt, srv.URL+"/v1/chat", http.MethodGet, "")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "h1-ok" {
		t.Fatalf("body = %q, want h1-ok", body)
	}
	if resp.ProtoMajor != 1 {
		t.Fatalf("response ProtoMajor = %d, want 1", resp.ProtoMajor)
	}
	if got := sawProto.Load(); got != "HTTP/1.1" {
		t.Fatalf("upstream saw %v, want HTTP/1.1", got)
	}
	addr := authorityOf(t, srv.URL)
	if v, ok := rt.protos.Load(addr); !ok || v.(string) != protoH1 {
		t.Fatalf("protos[%s] = %v (ok=%v), want %q after fallback", addr, v, ok, protoH1)
	}
	if n := atomic.LoadInt32(h2Dials); n != 1 {
		t.Fatalf("after first request: h2 dials = %d, want 1 (one probe)", n)
	}
	if n := atomic.LoadInt32(h1Dials); n != 1 {
		t.Fatalf("after first request: h1 dials = %d, want 1 (fallback)", n)
	}

	// Second request: cached as http/1.1 -> must NOT probe h2 again.
	resp2 := mustGet(t, rt, srv.URL+"/v1/chat", http.MethodGet, "")
	_, _ = io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if n := atomic.LoadInt32(h2Dials); n != 1 {
		t.Fatalf("after second request: h2 dials = %d, want still 1 (cache must skip the h2 probe)", n)
	}
}

// TestH2Dispatch_DialErrorSurfaces proves a genuine dial failure (not an ALPN
// fallback) is returned to the caller rather than silently swallowed.
func TestH2Dispatch_DialErrorSurfaces(t *testing.T) {
	rt, _, _ := dispatchForServers()
	// 127.0.0.1:1 is reserved/closed — the dial fails before any TLS.
	req, _ := http.NewRequest(http.MethodGet, "https://127.0.0.1:1/", nil)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("expected dial error, got nil")
	}
}

// TestH2Dispatch_PlainHTTPRoutesToH1 proves a plain http:// request is routed
// to the stdlib transport, not the h2 transport (which rejects unencrypted
// HTTP/2). This is the path a forward through a non-TLS upstream takes.
func TestH2Dispatch_PlainHTTPRoutesToH1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain-ok"))
	}))
	defer srv.Close()

	// h1 transport that dials plainly (http:// has no TLS); h2 seam must stay
	// untouched for a non-https request.
	h2Dial, h2c := loopbackUTLSDial()
	base := &http.Transport{}
	rt := newH2DispatchTransport(base, h2Dial)

	resp := mustGet(t, rt, srv.URL+"/x", http.MethodGet, "")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "plain-ok" {
		t.Fatalf("body = %q, want plain-ok", body)
	}
	if n := atomic.LoadInt32(h2c); n != 0 {
		t.Fatalf("h2 dials = %d, want 0 (http:// must not touch the h2 transport)", n)
	}
}

// TestCanonicalAuthority pins the authority-key derivation used by the
// dispatcher cache against the host:port form golang.org/x/net/http2 dials.
func TestCanonicalAuthority(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://api.cursor.sh/path", "api.cursor.sh:443"},
		{"https://api.cursor.sh:8443/path", "api.cursor.sh:8443"},
		{"http://example.com/x", "example.com:80"},
		{"http://example.com:3000/x", "example.com:3000"},
		{"https://[::1]/x", "[::1]:443"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("parse %s: %v", c.raw, err)
		}
		if got := canonicalAuthority(u); got != c.want {
			t.Errorf("canonicalAuthority(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func authorityOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %s: %v", rawURL, err)
	}
	return canonicalAuthority(u)
}
