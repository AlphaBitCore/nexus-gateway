package specutil

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestDefaultHTTPConfig pins the historical hardcoded values so a future
// refactor doesn't silently halve the per-call budget.
func TestDefaultHTTPConfig(t *testing.T) {
	d := DefaultHTTPConfig()
	if d.Timeout != 120*time.Second {
		t.Errorf("Timeout: want 120s, got %s", d.Timeout)
	}
	if d.DialTimeout != 15*time.Second {
		t.Errorf("DialTimeout: want 15s, got %s", d.DialTimeout)
	}
	if d.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout: want 10s, got %s", d.TLSHandshakeTimeout)
	}
	if d.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout: want 90s, got %s", d.IdleConnTimeout)
	}
	if d.MaxIdleConns != 200 || d.MaxIdleConnsPerHost != 50 {
		t.Errorf("pool: want 200/50, got %d/%d", d.MaxIdleConns, d.MaxIdleConnsPerHost)
	}
}

// TestConfigure_OverridesActiveSnapshot is the core "YAML actually
// reaches the upstream client" guarantee.
func TestConfigure_OverridesActiveSnapshot(t *testing.T) {
	t.Cleanup(func() { Configure(DefaultHTTPConfig()) })

	Configure(HTTPConfig{
		Timeout:             240 * time.Second,
		DialTimeout:         5 * time.Second,
		TLSHandshakeTimeout: 7 * time.Second,
		KeepAlive:           20 * time.Second,
		IdleConnTimeout:     45 * time.Second,
		MaxIdleConns:        77,
		MaxIdleConnsPerHost: 11,
	})

	got := ActiveConfig()
	if got.Timeout != 240*time.Second {
		t.Errorf("Timeout: want 240s, got %s", got.Timeout)
	}
	if got.DialTimeout != 5*time.Second {
		t.Errorf("DialTimeout: want 5s, got %s", got.DialTimeout)
	}
	if got.MaxIdleConns != 77 || got.MaxIdleConnsPerHost != 11 {
		t.Errorf("pool: want 77/11, got %d/%d", got.MaxIdleConns, got.MaxIdleConnsPerHost)
	}
}

// TestConfigure_ZeroFieldsFallBackToDefaults guards the YAML-friendliness
// promise: writing `upstream: {timeoutSec: 240}` and omitting the rest
// must not collapse other knobs to zero.
func TestConfigure_ZeroFieldsFallBackToDefaults(t *testing.T) {
	t.Cleanup(func() { Configure(DefaultHTTPConfig()) })

	Configure(HTTPConfig{Timeout: 240 * time.Second})

	got := ActiveConfig()
	def := DefaultHTTPConfig()
	if got.Timeout != 240*time.Second {
		t.Errorf("Timeout: want 240s, got %s", got.Timeout)
	}
	if got.DialTimeout != def.DialTimeout {
		t.Errorf("DialTimeout: want %s, got %s", def.DialTimeout, got.DialTimeout)
	}
	if got.MaxIdleConns != def.MaxIdleConns {
		t.Errorf("MaxIdleConns: want %d, got %d", def.MaxIdleConns, got.MaxIdleConns)
	}
}

// TestNewHTTPClient_ReturnsStableSingleton pins the contract: NewHTTPClient
// must hand back the SAME *http.Client pointer across calls and across
// Configure, so adapters that captured the client at construction never
// become stale. The transport behind the singleton swaps atomically —
// verified by TestConfigure_SwapsActiveTransport.
func TestNewHTTPClient_ReturnsStableSingleton(t *testing.T) {
	t.Cleanup(func() { Configure(DefaultHTTPConfig()) })

	c1 := NewHTTPClient()
	Configure(HTTPConfig{Timeout: 60 * time.Second})
	c2 := NewHTTPClient()
	Configure(HTTPConfig{Timeout: 120 * time.Second})
	c3 := NewHTTPClient()

	if c1 != c2 || c2 != c3 {
		t.Fatalf("NewHTTPClient returned different pointers across Configure: %p, %p, %p", c1, c2, c3)
	}
	// Per-request budget is delivered via ActiveConfig + caller context,
	// not via http.Client.Timeout. Asserting Timeout==0 documents this.
	if c1.Timeout != 0 {
		t.Errorf("upstream singleton Timeout: want 0 (caller-context-driven), got %s", c1.Timeout)
	}
}

// TestConfigure_SwapsActiveTransport asserts the swappable Transport
// behind the upstream singleton actually changes pointer on Configure.
// Without this, the singleton would be a stable wrapper around a fixed
// transport — defeating hot-swap entirely.
func TestConfigure_SwapsActiveTransport(t *testing.T) {
	t.Cleanup(func() { Configure(DefaultHTTPConfig()) })

	rt1 := *activeRT.Load()
	Configure(HTTPConfig{Timeout: 99 * time.Second})
	rt2 := *activeRT.Load()
	if rt1 == rt2 {
		t.Fatalf("activeRT did not swap on Configure")
	}
}

// TestNewProbeClient_FixedSingleton documents that the probe client
// does NOT participate in upstream hot-swap — Probe budgets stay fixed
// regardless of operator-tuned upstream timeouts.
func TestNewProbeClient_FixedSingleton(t *testing.T) {
	p1 := NewProbeClient()
	Configure(HTTPConfig{Timeout: 99 * time.Second})
	p2 := NewProbeClient()
	t.Cleanup(func() { Configure(DefaultHTTPConfig()) })

	if p1 != p2 {
		t.Fatalf("probe client must be a stable singleton; got %p then %p", p1, p2)
	}
}

// TestLiveTransport_RoundTripDelegatesToActiveRT exercises the
// liveTransport.RoundTrip path: a Configure swap must immediately
// route the next request through the NEW underlying transport. We
// stub activeRT to a counting RoundTripper, run a request via the
// singleton client, and confirm the count incremented — proving the
// delegation actually reaches the live pointer rather than caching a
// stale value at construction.
func TestLiveTransport_RoundTripDelegatesToActiveRT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	// Save + restore the live RT so the rest of the suite is unaffected.
	origRT := activeRT.Load()
	t.Cleanup(func() {
		activeRT.Store(origRT)
		Configure(DefaultHTTPConfig())
	})

	var calls atomic.Int32
	counter := &countingRT{base: http.DefaultTransport, calls: &calls}
	var asRT http.RoundTripper = counter
	activeRT.Store(&asRT)

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d, want 200", resp.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("countingRT.calls=%d, want 1 — singleton did not delegate to activeRT", got)
	}
}

// countingRT records every RoundTrip call so we can assert delegation.
type countingRT struct {
	base  http.RoundTripper
	calls *atomic.Int32
}

func (c *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.base.RoundTrip(req)
}

// idleClosingRT satisfies the idleCloser interface used by
// closeIdleConns and records the call so we can assert it fired.
type idleClosingRT struct {
	base   http.RoundTripper
	closed *atomic.Int32
}

func (i *idleClosingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	return i.base.RoundTrip(req)
}

func (i *idleClosingRT) CloseIdleConnections() {
	i.closed.Add(1)
}

// TestCloseIdleConns_InvokesClose asserts the helper invokes
// CloseIdleConnections when the wrapped RoundTripper exposes it. The
// real production wrapper (traffic.tracingTransport) does NOT expose
// the method today, which is why this branch was unreached by the
// indirect Configure→closeIdleConns path. Cover it directly so a
// future change that wraps a transport with CloseIdleConnections
// continues to actually drain the pool.
func TestCloseIdleConns_InvokesClose(t *testing.T) {
	var closed atomic.Int32
	rt := &idleClosingRT{base: http.DefaultTransport, closed: &closed}
	closeIdleConns(rt)
	if got := closed.Load(); got != 1 {
		t.Errorf("CloseIdleConnections call-count=%d, want 1", got)
	}
}

// TestCloseIdleConns_NoMethod_NoPanic asserts the helper is a safe
// no-op when the wrapped RoundTripper does NOT expose
// CloseIdleConnections (today's production case via tracingTransport).
// Without this, a future wrapper change could regress the helper into
// a panic via type assertion.
func TestCloseIdleConns_NoMethod_NoPanic(t *testing.T) {
	var calls atomic.Int32
	plain := &countingRT{base: http.DefaultTransport, calls: &calls}
	// Must not panic and must not call anything on plain — countingRT
	// has no CloseIdleConnections so the type-assertion ok-branch is
	// false. We also assert no RoundTrip slipped through.
	closeIdleConns(plain)
	if got := calls.Load(); got != 0 {
		t.Errorf("plain transport: unexpected RoundTrip call (calls=%d)", got)
	}
}
