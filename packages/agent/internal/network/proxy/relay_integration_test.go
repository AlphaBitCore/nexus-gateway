package proxy_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/relay"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// startCountingTLSServer returns an httptest TLS server that increments
// a counter on every TLS handshake (the server side runs
// GetConfigForClient before handshake completes, which is the
// canonical hook for this signal in net/http).
func startCountingTLSServer(t *testing.T, h http.Handler) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			count.Add(1)
			return nil, nil
		},
	}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &count
}

func newClient(t *testing.T) *relay.Client {
	t.Helper()
	c, err := relay.New(relay.Config{
		UserAgent:       "nexus-agent/test",
		OpsRegistry:     opsmetrics.NewRegistry(prometheus.NewRegistry()),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatalf("relay.New: %v", err)
	}
	return c
}

// TestRelay_HandshakeCount_Sequential verifies that 100 sequential
// flows produced by relay.Client.Do against the same host result in
// a small fixed handshake count on the server (HTTP/2 multiplex).
func TestRelay_HandshakeCount_Sequential(t *testing.T) {
	srv, count := startCountingTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))

	c := newClient(t)
	for i := range 100 {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	got := count.Load()
	if got > 3 {
		t.Errorf("handshake count = %d, want ≤ 3 (HTTP/2 multiplex over a single connection)", got)
	}
	t.Logf("100 sequential flows produced %d TLS handshakes", got)
}

// (No concurrent-burst handshake assertion: stdlib http.Transport's
// concurrent first-dial behaviour does not dedupe — H2 multiplex
// capability is only known after the handshake, so N concurrent cold
// dials can each open their own connection. The chatty-keep-alive
// shape the rewrite actually targets is covered by Sequential above.
// The dial counter in mode={reused,new} is asserted in the relay
// package's own TestClient_Do_DialAccounting.)

// TestRelay_HandshakeCount_DifferentHosts verifies that two different
// destination hosts each get their own TLS connection (no sharing).
func TestRelay_HandshakeCount_DifferentHosts(t *testing.T) {
	srvA, countA := startCountingTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srvB, countB := startCountingTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	c := newClient(t)
	for range 5 {
		for _, url := range []string{srvA.URL, srvB.URL} {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("Do %s: %v", url, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}

	if got := countA.Load(); got != 1 {
		t.Errorf("server A handshake count = %d, want 1", got)
	}
	if got := countB.Load(); got != 1 {
		t.Errorf("server B handshake count = %d, want 1", got)
	}
}
