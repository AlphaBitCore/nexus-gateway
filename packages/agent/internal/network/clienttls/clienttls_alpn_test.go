package clienttls

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sharedhttp "github.com/AlphaBitCore/nexus-gateway/packages/httpclient"
)

// WithTLSConfig replaced the transport's *tls.Config wholesale. The base client
// from shared/transport/http calls http2.ConfigureTransports, which writes "h2"
// into TLSClientConfig.NextProtos — so the replacement dropped the ALPN offer
// and both agent Hub clients silently negotiated HTTP/1.1. The h2 keep-alive
// probe (ReadIdleTimeout) configured next to it became dead configuration, and
// nothing logged the downgrade.
//
// The end-to-end arm is the one that matters: an h2-only test server tells us
// what was actually negotiated on the wire, which reasoning about NextProtos
// cannot.

func newBaseClient() *http.Client {
	return sharedhttp.New(sharedhttp.Config{
		Timeout:             10 * time.Second,
		DialTimeout:         3 * time.Second,
		KeepAlive:           30 * time.Second,
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	})
}

func mustTransport(t *testing.T, c *http.Client) *http.Transport {
	t.Helper()
	tr, err := UnderlyingTransport(c)
	if err != nil || tr == nil {
		t.Fatalf("UnderlyingTransport: %v", err)
	}
	return tr
}

// TestWithTLSConfig_KeepsHTTP2OnTheWire pins the observable outcome: after
// pinning a CA pool, the client still speaks HTTP/2.
func TestWithTLSConfig_KeepsHTTP2OnTheWire(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Proto", r.Proto)
		w.WriteHeader(http.StatusOK)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	c := newBaseClient()
	// Exactly what the Hub clients do: pin the CA, nothing about ALPN.
	if err := WithTLSConfig(c, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatalf("WithTLSConfig: %v", err)
	}

	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.Proto != "HTTP/2.0" {
		t.Errorf("negotiated %s, want HTTP/2.0 — pinning a CA pool silently downgraded the connection, and the h2 keep-alive probe configured on this transport is dead", resp.Proto)
	}
}

// The structural half, so a failure says WHY: the ALPN list the transport was
// configured with must survive the replacement.
func TestWithTLSConfig_PreservesALPNWhenCallerDoesNotSetIt(t *testing.T) {
	c := newBaseClient()
	tr, err := UnderlyingTransport(c)
	if err != nil || tr == nil {
		t.Fatalf("base client has no *http.Transport: %v", err)
	}
	before := append([]string(nil), tr.TLSClientConfig.NextProtos...)
	if len(before) == 0 {
		t.Fatal("the base transport advertises no ALPN protocols — this test cannot observe what it claims to")
	}

	if err := WithTLSConfig(c, &tls.Config{MinVersion: tls.VersionTLS13}); err != nil {
		t.Fatalf("WithTLSConfig: %v", err)
	}
	after := mustTransport(t, c).TLSClientConfig.NextProtos
	if len(after) != len(before) {
		t.Fatalf("NextProtos = %v, want the transport's original %v", after, before)
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("NextProtos = %v, want %v", after, before)
		}
	}
	// The caller's own fields must still be applied.
	if mustTransport(t, c).TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Error("the caller's MinVersion was lost")
	}
}

// A caller that DOES specify ALPN is deciding deliberately, and must win.
// Without this, "always keep the old list" would be indistinguishable from
// "ignore the caller".
func TestWithTLSConfig_CallerSuppliedALPNWins(t *testing.T) {
	c := newBaseClient()
	want := []string{"http/1.1"}
	if err := WithTLSConfig(c, &tls.Config{NextProtos: want, MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatalf("WithTLSConfig: %v", err)
	}
	got := mustTransport(t, c).TLSClientConfig.NextProtos
	if len(got) != 1 || got[0] != "http/1.1" {
		t.Errorf("NextProtos = %v, want %v — an explicit caller choice was overridden", got, want)
	}
}

// A nil cfg clears the config, which is the documented behaviour and must stay.
func TestWithTLSConfig_NilClears(t *testing.T) {
	c := newBaseClient()
	if err := WithTLSConfig(c, nil); err != nil {
		t.Fatalf("WithTLSConfig(nil): %v", err)
	}
	if mustTransport(t, c).TLSClientConfig != nil {
		t.Error("nil cfg must clear TLSClientConfig")
	}
}
