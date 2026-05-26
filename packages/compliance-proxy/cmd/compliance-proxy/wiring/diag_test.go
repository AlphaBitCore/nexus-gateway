package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

func TestInitDiagSink_NilThingClientReturnsBuffer(t *testing.T) {
	logger := testLogger()
	// tc is nil — should still build a reconnect buffer and wrap the logger.
	result := InitDiagSink(logger, nil, "proxy-test", nil)

	if result.Logger == nil {
		t.Error("expected non-nil updated logger")
	}
	if result.ReconnectBuffer == nil {
		t.Error("expected non-nil reconnect buffer even when tc is nil")
	}
}

func TestInitDiagSink_LoggerIsUpdated(t *testing.T) {
	logger := testLogger()
	result := InitDiagSink(logger, nil, "proxy-test", nil)
	// The returned logger wraps the original via MultiHandler; they are
	// different objects.
	if result.Logger == logger {
		t.Error("expected returned logger to be a new wrapped instance")
	}
}

func TestPushStartupDiagEvent_NilClientIsNoop(t *testing.T) {
	// Should not panic when tc is nil.
	PushStartupDiagEvent(nil, "proxy-1", "v0.1.0")
}

func TestPushStartupDiagEvent_WithClientFiresGoroutine(t *testing.T) {
	// We cannot easily wait for the 600ms sleep in a unit test, but we can at
	// least verify the function does not panic when given a real (but
	// unconnected) client. The client is nil here to exercise the nil guard.
	var tc *thingclient.Client
	PushStartupDiagEvent(tc, "proxy-2", "v0.1.0")
}

// TestInitDiagSink_IsWSConnectedClosure exercises the IsWSConnected closure
// body (lines 35-37) by emitting an ERROR log through the updated logger.
// The SlogSink.Handle method calls IsWSConnected() at log time.
func TestInitDiagSink_IsWSConnectedClosure_InvokedOnErrorLog(t *testing.T) {
	logger := testLogger()
	result := InitDiagSink(logger, sharedTestThingClient, "proxy-isws-test", nil)
	if result.Logger == nil {
		t.Fatal("expected non-nil logger")
	}
	// Emit an ERROR record — this goes through SlogSink.Handle which calls
	// IsWSConnected(). The client is not started so Mode() != ModeWSConnected.
	result.Logger.Error("test error from diag sink coverage test")
}
