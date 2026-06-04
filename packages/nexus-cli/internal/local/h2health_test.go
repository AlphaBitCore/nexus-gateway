package local

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEnableH2Health_TransportServesOverH2 verifies EnableH2Health configures the
// transport so it (a) still works and (b) actually negotiates HTTP/2 — the protocol
// whose dead-connection reuse the PING health-check is there to fix.
func TestEnableH2Health_TransportServesOverH2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	EnableH2Health(tr)

	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatalf("request after EnableH2Health failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("expected HTTP/2 after EnableH2Health, got HTTP/%d.%d", resp.ProtoMajor, resp.ProtoMinor)
	}
}

// TestEnableH2Health_DoubleCallNoPanic guards that a second call (ConfigureTransports
// returns an error the helper swallows) is a harmless no-op rather than a panic.
func TestEnableH2Health_DoubleCallNoPanic(t *testing.T) {
	tr := &http.Transport{}
	EnableH2Health(tr)
	EnableH2Health(tr) // must not panic
}

// freezeConn wraps a net.Conn so the server side of a specific connection can be
// "frozen" — Reads block and Writes are discarded while the TCP stays open, with no
// FIN/RST. That reproduces a NAT/firewall silently blackholing an established HTTP/2
// connection: the peer is gone but the local socket still looks alive.
type freezeConn struct {
	net.Conn
	frozen *atomic.Bool
}

func (c *freezeConn) Read(b []byte) (int, error) {
	for c.frozen.Load() {
		// Don't deliver inbound frames (e.g. the client's PING) to the h2 server, so it
		// never answers — but keep polling the socket with a short deadline so that when
		// the peer finally closes the connection this goroutine unblocks and exits
		// (otherwise the test server's graceful Close would hang forever).
		_ = c.Conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		if _, err := c.Conn.Read(b); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return 0, err // underlying closed → unblock the serve goroutine
		}
		// bytes read are intentionally discarded (blackholed)
	}
	_ = c.Conn.SetReadDeadline(time.Time{})
	return c.Conn.Read(b)
}

func (c *freezeConn) Write(b []byte) (int, error) {
	if c.frozen.Load() {
		return len(b), nil // blackhole: pretend the bytes left, so no error surfaces
	}
	return c.Conn.Write(b)
}

// freezeListener hands out freezeConns and can freeze the first accepted connection
// while every later connection works normally — i.e. the existing pooled h2 conn is
// dead but a fresh dial still succeeds (NAT made a new mapping).
type freezeListener struct {
	net.Listener
	mu     sync.Mutex
	frozen []*atomic.Bool
}

func (l *freezeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	f := &atomic.Bool{}
	l.mu.Lock()
	l.frozen = append(l.frozen, f)
	l.mu.Unlock()
	return &freezeConn{Conn: c, frozen: f}, nil
}

func (l *freezeListener) freezeFirst() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.frozen) > 0 {
		l.frozen[0].Store(true)
	}
}

func newFrozenFirstH2Server(t *testing.T) (*httptest.Server, *freezeListener) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.EnableHTTP2 = true
	fl := &freezeListener{Listener: srv.Listener}
	srv.Listener = fl
	srv.StartTLS()
	return srv, fl
}

// TestEnableH2Health_RecoversFromBlackholedConn is the load-bearing test: it reproduces
// the production failure (an idle HTTP/2 connection silently blackholed by a middlebox,
// then reused) and proves the PING health-check recovers by detecting the dead conn and
// dialing a fresh one — where the unconfigured transport just hangs on the dead conn.
func TestEnableH2Health_RecoversFromBlackholedConn(t *testing.T) {
	srv, fl := newFrozenFirstH2Server(t)
	defer srv.Close()

	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	// Short timeouts so the idle PING fires fast in the test.
	configureH2Health(tr, 150*time.Millisecond, 150*time.Millisecond)
	client := &http.Client{Transport: tr}

	// First request establishes the (soon-to-be-blackholed) h2 connection.
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("test must run over HTTP/2, got HTTP/%d", resp.ProtoMajor)
	}

	// The middlebox silently drops that connection: TCP stays open, peer is gone.
	fl.freezeFirst()

	// Let the connection sit idle long enough for the health-check PING to fire and fail
	// (no PONG), so the dead conn is evicted before the next request.
	time.Sleep(500 * time.Millisecond)

	// Second request must RECOVER on a fresh connection within a bounded time, not hang.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("second request did not recover after the connection was blackholed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("recovered request status = %d, want 200", resp2.StatusCode)
	}
}

// TestEnableH2Health_WithoutHealthCheckHangs is the control: the SAME blackhole, on a
// transport WITHOUT the health-check, hangs on the dead pooled connection — proving the
// fix is what makes the difference, not the test setup.
func TestEnableH2Health_WithoutHealthCheckHangs(t *testing.T) {
	srv, fl := newFrozenFirstH2Server(t)
	defer srv.Close()

	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2: true,
	}
	// NO configureH2Health → no PING health-check.
	client := &http.Client{Transport: tr}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("control must also run over HTTP/2 so the hang is on an h2 conn, got HTTP/%d", resp.ProtoMajor)
	}

	fl.freezeFirst()
	time.Sleep(500 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("without the health-check the request should hang on the dead conn and be cancelled, but it succeeded")
	} else if ctx.Err() == nil {
		t.Fatalf("expected a context-deadline cancellation (hung on dead conn), got: %v", err)
	}
}
