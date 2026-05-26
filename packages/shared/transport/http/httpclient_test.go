package http

import (
	"crypto/tls"
	"net/http"
	"syscall"
	"testing"
	"time"
)

func TestNew_AppliesDefaults(t *testing.T) {
	c := New(Config{})
	if c.Timeout != 30*time.Second {
		t.Errorf("Timeout default: got %v, want 30s", c.Timeout)
	}
	lt, ok := c.Transport.(*loggingTransport)
	if !ok {
		t.Fatalf("Transport: got %T, want *loggingTransport", c.Transport)
	}
	tr, ok := lt.base.(*http.Transport)
	if !ok {
		t.Fatalf("base: got %T, want *http.Transport", lt.base)
	}
	if tr.MaxIdleConns != 200 {
		t.Errorf("MaxIdleConns: got %d, want 200", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != 50 {
		t.Errorf("MaxIdleConnsPerHost: got %d, want 50", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout: got %v, want 90s", tr.IdleConnTimeout)
	}
	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout: got %v, want 10s", tr.TLSHandshakeTimeout)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2: got false, want true (default)")
	}
}

func TestNew_RespectsExplicitConfig(t *testing.T) {
	c := New(Config{
		Timeout:             5 * time.Second,
		MaxIdleConnsPerHost: 7,
		ForceHTTP2:          Off(),
	})
	if c.Timeout != 5*time.Second {
		t.Errorf("Timeout: got %v, want 5s", c.Timeout)
	}
	lt, ok := c.Transport.(*loggingTransport)
	if !ok {
		t.Fatalf("Transport: got %T, want *loggingTransport", c.Transport)
	}
	tr, ok := lt.base.(*http.Transport)
	if !ok {
		t.Fatalf("base: got %T, want *http.Transport", lt.base)
	}
	if tr.MaxIdleConnsPerHost != 7 {
		t.Errorf("MaxIdleConnsPerHost: got %d, want 7", tr.MaxIdleConnsPerHost)
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2: got true, want false (explicit Off())")
	}
}

func TestNew_NegativeMaxConnsPerHostMeansUnlimited(t *testing.T) {
	c := New(Config{MaxConnsPerHost: -1})
	lt, ok := c.Transport.(*loggingTransport)
	if !ok {
		t.Fatalf("Transport: got %T, want *loggingTransport", c.Transport)
	}
	tr, ok := lt.base.(*http.Transport)
	if !ok {
		t.Fatalf("base: got %T, want *http.Transport", lt.base)
	}
	if tr.MaxConnsPerHost != 0 {
		t.Errorf("MaxConnsPerHost: got %d, want 0 (unlimited)", tr.MaxConnsPerHost)
	}
}

func TestNew_TLSConfigStartsClean(t *testing.T) {
	c := New(Config{})
	lt, ok := c.Transport.(*loggingTransport)
	if !ok {
		t.Fatalf("Transport: got %T, want *loggingTransport", c.Transport)
	}
	tr, ok := lt.base.(*http.Transport)
	if !ok {
		t.Fatalf("base: got %T, want *http.Transport", lt.base)
	}
	if tr.TLSClientConfig == nil {
		// nil is fine; net/http will instantiate as needed.
		return
	}
	// If anything ever sets a default TLSConfig, make sure it doesn't
	// disable verification or pin to weird ciphers.
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must default to false")
	}
	_ = tls.Certificate{} // keeps tls import meaningful even if assertion path skipped
}

func TestNewProbe_ShortTimeouts(t *testing.T) {
	c := NewProbe()
	if c.Timeout != 5*time.Second {
		t.Errorf("Probe Timeout: got %v, want 5s", c.Timeout)
	}
	lt, ok := c.Transport.(*loggingTransport)
	if !ok {
		t.Fatalf("Transport: got %T, want *loggingTransport", c.Transport)
	}
	tr, ok := lt.base.(*http.Transport)
	if !ok {
		t.Fatalf("base: got %T, want *http.Transport", lt.base)
	}
	if tr.MaxIdleConnsPerHost != 5 {
		t.Errorf("Probe MaxIdleConnsPerHost: got %d, want 5", tr.MaxIdleConnsPerHost)
	}
}

func TestOnOff(t *testing.T) {
	if !*On() {
		t.Error("On() must return *true")
	}
	if *Off() {
		t.Error("Off() must return *false")
	}
}

func TestNew_WrapsLoggingTransport(t *testing.T) {
	c := New(Config{Caller: "test"})
	if _, ok := c.Transport.(*loggingTransport); !ok {
		t.Fatalf("expected *loggingTransport, got %T", c.Transport)
	}
}

func TestNew_PropagateReqIDFlowsThrough(t *testing.T) {
	c := New(Config{Caller: "test", PropagateReqID: true})
	lt := c.Transport.(*loggingTransport)
	if !lt.opts.PropagateReqID {
		t.Error("PropagateReqID not propagated into loggingTransport opts")
	}
	if lt.opts.Caller != "test" {
		t.Errorf("Caller: got %q want test", lt.opts.Caller)
	}
}

func TestGlobalDialControl_SetGetClear(t *testing.T) {
	// Process-wide install + read + clear cycle. Used by macOS NE +
	// Linux iptables to mark agent egress so it bypasses the redirect
	// chain (see SO_MARK 0x4E58 comment in production code).
	prev := GlobalDialControl()
	t.Cleanup(func() { SetGlobalDialControl(prev) })

	SetGlobalDialControl(nil)
	if got := GlobalDialControl(); got != nil {
		t.Fatal("after clear: GlobalDialControl should be nil, got non-nil")
	}

	called := 0
	fn := func(network, address string, c syscall.RawConn) error {
		called++
		return nil
	}
	SetGlobalDialControl(fn)
	got := GlobalDialControl()
	if got == nil {
		t.Fatal("after set: GlobalDialControl should be non-nil")
	}
	_ = got("tcp", "1.2.3.4:443", nil)
	if called != 1 {
		t.Errorf("callback invoked %d times, want 1", called)
	}

	// SetGlobalDialControl(nil) must clear the slot — not store a nil
	// pointer that GlobalDialControl then dereferences.
	SetGlobalDialControl(nil)
	if got := GlobalDialControl(); got != nil {
		t.Error("after final clear: got non-nil")
	}
}

func TestResolveDialControl_PerCallOverridesGlobal(t *testing.T) {
	// resolveDialControl prefers Config.DialControl over the global.
	// Without this, services that set a per-call mark (e.g. a one-shot
	// probe) would silently inherit the unrelated global.
	prev := GlobalDialControl()
	t.Cleanup(func() { SetGlobalDialControl(prev) })

	globalCalled := 0
	localCalled := 0
	SetGlobalDialControl(func(string, string, syscall.RawConn) error {
		globalCalled++
		return nil
	})

	resolved := resolveDialControl(Config{
		DialControl: func(string, string, syscall.RawConn) error {
			localCalled++
			return nil
		},
	})
	_ = resolved("tcp", "x:1", nil)
	if localCalled != 1 || globalCalled != 0 {
		t.Errorf("per-call DialControl should win: local=%d global=%d", localCalled, globalCalled)
	}

	// And nil per-call falls through to the global.
	resolved2 := resolveDialControl(Config{})
	_ = resolved2("tcp", "x:1", nil)
	if globalCalled != 1 {
		t.Errorf("global should fire when per-call nil: %d", globalCalled)
	}
}
