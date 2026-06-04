package core

import (
	"net/http"
	"testing"
	"time"
)

// TestNewClient_StreamingUncappedAndHandshakeWidened locks in the transport fix:
// the streaming client carries no overall timeout (an SSE turn is bounded by its
// request context, not a 30s wall clock that would cut a long agent turn), and the
// admin transport widens the TLS handshake budget past the 10s default that flaked
// against a slow prod TLS termination.
func TestNewClient_StreamingUncappedAndHandshakeWidened(t *testing.T) {
	c := NewClient(Env{Name: "local"}, fixedTokenSource{}, nil)
	if c.httpc.Timeout != 60*time.Second {
		t.Fatalf("admin client timeout = %v, want 60s", c.httpc.Timeout)
	}
	if c.streamc.Timeout != 0 {
		t.Fatalf("streaming client must have no overall timeout (ctx-bound), got %v", c.streamc.Timeout)
	}
	tr, ok := c.httpc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("admin transport should be *http.Transport, got %T", c.httpc.Transport)
	}
	if tr.TLSHandshakeTimeout != 30*time.Second {
		t.Fatalf("TLS handshake budget = %v, want 30s", tr.TLSHandshakeTimeout)
	}
	// Idle pooled connections must be evicted before nginx/NAT silently drops them
	// (90s default outlived the middlebox → stale-connection-reuse hangs).
	if tr.IdleConnTimeout != 30*time.Second {
		t.Fatalf("IdleConnTimeout = %v, want 30s (recycle idle conns before a middlebox drops them)", tr.IdleConnTimeout)
	}
	if c.streamc.Transport != c.httpc.Transport {
		t.Fatal("the streaming client should share the admin transport (pooling + same TLS/proxy settings)")
	}
}

// TestNewClient_StreamingReusesInjectedTransport ensures an injected client's
// transport (a test server's, or a mock round-tripper) backs the streaming client,
// so streaming is testable without real network. The web assistant's in-process
// self-call transport relies on this: it is injected as the admin client's transport
// and must also back the streaming (inference) client.
func TestNewClient_StreamingReusesInjectedTransport(t *testing.T) {
	inj := &http.Client{Transport: http.DefaultTransport}
	c := NewClient(Env{Name: "local"}, fixedTokenSource{}, inj)
	if c.streamc.Transport != inj.Transport {
		t.Fatal("an injected client's transport must back the streaming client")
	}
}
