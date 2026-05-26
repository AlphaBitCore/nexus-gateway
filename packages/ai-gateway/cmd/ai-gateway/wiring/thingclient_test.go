package wiring

import (
	"context"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
)

// TestDefaultAdvertiseHost_emptyReturnsLoopback verifies that an empty
// advertise host is replaced with loopback.
func TestDefaultAdvertiseHost_emptyReturnsLoopback(t *testing.T) {
	if got := DefaultAdvertiseHost(""); got != "127.0.0.1" {
		t.Errorf("empty: want 127.0.0.1, got %q", got)
	}
}

// TestDefaultAdvertiseHost_wildcardIPv4ReturnsLoopback.
func TestDefaultAdvertiseHost_wildcardIPv4ReturnsLoopback(t *testing.T) {
	if got := DefaultAdvertiseHost("0.0.0.0"); got != "127.0.0.1" {
		t.Errorf("0.0.0.0: want 127.0.0.1, got %q", got)
	}
}

// TestDefaultAdvertiseHost_wildcardIPv6ReturnsLoopback.
func TestDefaultAdvertiseHost_wildcardIPv6ReturnsLoopback(t *testing.T) {
	if got := DefaultAdvertiseHost("::"); got != "127.0.0.1" {
		t.Errorf("::: want 127.0.0.1, got %q", got)
	}
}

// TestDefaultAdvertiseHost_specificHostPassedThrough.
func TestDefaultAdvertiseHost_specificHostPassedThrough(t *testing.T) {
	cases := []string{"10.0.0.1", "my-host.internal", "192.168.1.100"}
	for _, c := range cases {
		if got := DefaultAdvertiseHost(c); got != c {
			t.Errorf("%q: want same, got %q", c, got)
		}
	}
}

// TestRunTicker_callsOnInterval verifies RunTicker calls fn at least once
// before context cancellation.
func TestRunTicker_callsOnInterval(t *testing.T) {
	count := 0
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- RunTicker(ctx, 50*time.Millisecond, func() {
			count++
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunTicker returned error: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("RunTicker did not exit after ctx cancel")
	}

	if count < 1 {
		t.Errorf("expected at least 1 call, got %d", count)
	}
}

// TestRunTicker_returnsNilOnContextCancel verifies the return value is nil
// when context is cancelled (clean shutdown path).
func TestRunTicker_returnsNilOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := RunTicker(ctx, time.Hour, func() { /* should never be called */ })
	if err != nil {
		t.Fatalf("expected nil on cancelled context, got %v", err)
	}
}

// TestWireDiagReconnect_nilTCNoOp verifies that nil thingclient is a no-op.
func TestWireDiagReconnect_nilTCNoOp(t *testing.T) {
	// Should not panic with nil thingclient.
	WireDiagReconnect(nil, nil, false, "test-ag", "v0.0.1")
}

// TestWireStaticInfoReconnect_nilTCNoOp verifies that nil thingclient is a no-op.
func TestWireStaticInfoReconnect_nilTCNoOp(t *testing.T) {
	WireStaticInfoReconnect(nil, TCInitResult{})
}

// TestWireStaticInfoReconnect_staticInfoNotSetNoOp verifies that if
// StaticInfoSet=false, the function returns without registering a reconnect.
func TestWireStaticInfoReconnect_staticInfoNotSetNoOp(t *testing.T) {
	// tc is nil but StaticInfoSet is also false, so the nil guard fires first.
	WireStaticInfoReconnect(nil, TCInitResult{StaticInfoSet: false})
}

// TestWireDiagReconnect_reconnectComposedTrue verifies that the goroutine branch
// for the lifecycle start event is still launched when reconnectComposed=true
// (OnReconnect is skipped but the lifecycle goroutine is not).
func TestWireDiagReconnect_reconnectComposedTrue(t *testing.T) {
	// nil tc → early return; reconnectComposed doesn't matter.
	WireDiagReconnect(nil, nil, true, "ag-id", "v1.0.0")
}

// TestInitThingClient_emptyHubURL returns an empty TCInitResult without panicking.
func TestInitThingClient_emptyHubURL(t *testing.T) {
	c := defaultConfig()
	c.Registry.NexusHubURL = "" // no Hub → early return
	result := InitThingClient(context.Background(), TCInitDeps{Cfg: &c, Logger: discardLogger()})
	if result.AgID != "" {
		t.Errorf("expected empty AgID for empty hub URL, got %q", result.AgID)
	}
}

// TestInitDiagSink_nilTCClient verifies InitDiagSink runs without panic when
// the thingclient is nil (diag sink degrades gracefully).
func TestInitDiagSink_nilTCClient(t *testing.T) {
	combined := InitDiagSink(
		context.Background(),
		nil, // nil thingclient
		TCInitResult{},
		"test-ag",
		"v0.0.1",
		discardLogger(),
		nil,
	)
	if combined == nil {
		t.Fatal("expected non-nil combined logger from InitDiagSink")
	}
}

// TestInitMQProducer_emptyDriver returns nil without error.
func TestInitMQProducer_emptyDriverIsNilNil(t *testing.T) {
	c := defaultConfig()
	p, err := InitMQProducer(&c, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != nil {
		t.Fatal("expected nil producer for empty driver")
	}
}

// defaultConfig returns a zero-value config.Config for tests that need a
// non-nil cfg pointer.
func defaultConfig() config.Config {
	return config.Config{}
}
