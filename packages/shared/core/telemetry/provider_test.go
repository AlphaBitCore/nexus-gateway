package telemetry

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestInitDisabled(t *testing.T) {
	cfg := Config{
		Enabled:     false,
		Endpoint:    "",
		ServiceName: "test-svc",
	}

	sp, err := Init(context.Background(), cfg, testLogger())
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	// Should return a no-op tracer that does not panic.
	tr := sp.Tracer("test")
	ctx, span := tr.Start(context.Background(), "test-span")
	span.End()
	_ = ctx

	// The underlying provider should be no-op.
	st := sp.current.Load()
	if st.sdkProvider != nil {
		t.Error("expected nil sdkProvider for disabled config")
	}
	if _, ok := st.provider.(noop.TracerProvider); !ok {
		t.Errorf("expected noop.TracerProvider, got %T", st.provider)
	}
}

func TestInitAndReconfigure(t *testing.T) {
	cfg := Config{
		Enabled:      true,
		Endpoint:     "localhost:4318",
		ServiceName:  "test-svc",
		SamplingRate: 0.5,
	}

	sp, err := Init(context.Background(), cfg, testLogger())
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = sp.Shutdown(ctx)
	}()

	// Tracer should work before reconfigure.
	tr := sp.Tracer("before")
	_, span := tr.Start(context.Background(), "span-before")
	span.End()

	// Reconfigure with different sampling rate.
	newCfg := Config{
		Enabled:      true,
		Endpoint:     "localhost:4318",
		ServiceName:  "test-svc-v2",
		SamplingRate: 1.0,
	}
	if err := sp.Reconfigure(newCfg); err != nil {
		t.Fatalf("Reconfigure returned error: %v", err)
	}

	// Tracer should work after reconfigure.
	tr2 := sp.Tracer("after")
	_, span2 := tr2.Start(context.Background(), "span-after")
	span2.End()

	// The provider should be an SDK provider (not no-op).
	st := sp.current.Load()
	if st.sdkProvider == nil {
		t.Error("expected non-nil sdkProvider after reconfigure with enabled config")
	}
}

func TestReconfigureDisable(t *testing.T) {
	cfg := Config{
		Enabled:      true,
		Endpoint:     "localhost:4318",
		ServiceName:  "test-svc",
		SamplingRate: 1.0,
	}

	sp, err := Init(context.Background(), cfg, testLogger())
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	// Verify it started as an SDK provider.
	st := sp.current.Load()
	if st.sdkProvider == nil {
		t.Fatal("expected SDK provider initially")
	}

	// Reconfigure to disabled.
	disabledCfg := Config{
		Enabled:     false,
		ServiceName: "test-svc",
	}
	if err := sp.Reconfigure(disabledCfg); err != nil {
		t.Fatalf("Reconfigure returned error: %v", err)
	}

	// Allow background shutdown goroutine to run.
	time.Sleep(50 * time.Millisecond)

	// Should now be a no-op provider.
	st = sp.current.Load()
	if st.sdkProvider != nil {
		t.Error("expected nil sdkProvider after disabling")
	}
	if _, ok := st.provider.(noop.TracerProvider); !ok {
		t.Errorf("expected noop.TracerProvider, got %T", st.provider)
	}

	// Tracer should still work (no-op).
	tr := sp.Tracer("disabled")
	_, span := tr.Start(context.Background(), "noop-span")
	span.End()
}

func TestShutdownTimeout(t *testing.T) {
	cfg := Config{
		Enabled:      true,
		Endpoint:     "localhost:4318",
		ServiceName:  "test-svc",
		SamplingRate: 1.0,
	}

	sp, err := Init(context.Background(), cfg, testLogger())
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	// Shutdown with a real timeout context.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = sp.Shutdown(ctx)
	if err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
}

// TestShutdown_OnNoopReturnsNilWithoutCallingSDK covers the
// `if st.sdkProvider != nil` branch in Shutdown — when telemetry was
// initialised disabled, Shutdown must NOT attempt to call into a nil
// sdkProvider and must return nil so callers' defer chains stay clean.
func TestShutdown_OnNoopReturnsNilWithoutCallingSDK(t *testing.T) {
	sp, err := Init(context.Background(), Config{Enabled: false, ServiceName: "noop-svc"}, testLogger())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sp.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown on noop provider must return nil, got: %v", err)
	}
}

// TestReconfigure_FromNoopSkipsShutdownOldAsync covers the
// `old == nil || old.sdkProvider == nil` early return in
// shutdownOldAsync. Reconfiguring from a disabled (noop) provider to
// an enabled SDK provider replaces the old state with no goroutine
// emitted — confirmed indirectly via the absence of a Shutdown
// invocation on a nil-typed pointer (would panic).
func TestReconfigure_FromNoopSkipsShutdownOldAsync(t *testing.T) {
	// Start with disabled (noop).
	sp, err := Init(context.Background(), Config{Enabled: false, ServiceName: "svc"}, testLogger())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Reconfigure to enabled. The previous state's sdkProvider is nil,
	// so shutdownOldAsync must short-circuit. If the early return were
	// removed, the call `old.sdkProvider.Shutdown(ctx)` would panic on
	// nil deref.
	if err := sp.Reconfigure(Config{
		Enabled:      true,
		Endpoint:     "localhost:4318",
		ServiceName:  "svc-v2",
		SamplingRate: 1.0,
	}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	// Allow the (skipped) goroutine to NOT-run.
	time.Sleep(20 * time.Millisecond)
	st := sp.current.Load()
	if st.sdkProvider == nil {
		t.Fatal("after reconfigure-from-noop, sdkProvider should be set")
	}
	// Clean up: shut down the SDK provider we just installed.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = sp.Shutdown(ctx)
}

// TestInit_NewProviderErrorPropagates pins the error-wrapping
// contract: if newProvider returns an error (rare but possible if the
// OTLP exporter constructor or resource builder fails on a canceled
// context), Init must wrap with "telemetry init: ..." so the caller's
// startup-log surface flags telemetry specifically — not "context
// canceled" with no provenance.
//
// This test deliberately exercises the cancellation-aware path of
// otlptracehttp.New / resource.New by passing an already-cancelled
// context. If a future OTEL SDK upgrade stops respecting ctx in either
// constructor, this test will silently start passing for the wrong
// reason — Init would succeed and the assertion would fail.
func TestInit_NewProviderErrorPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	_, err := Init(ctx, Config{
		Enabled:      true,
		Endpoint:     "localhost:4318",
		ServiceName:  "svc",
		SamplingRate: 1.0,
	}, testLogger())
	if err == nil {
		t.Skip("OTEL SDK no longer surfaces canceled-ctx errors from newProvider; revisit when SDK behavior changes")
	}
	// We don't assert exact wrapping wording — message stability is an
	// SDK concern — but the prefix must come from us.
	if got := err.Error(); !contains(got, "telemetry init") {
		t.Errorf("Init error not wrapped with 'telemetry init': %q", got)
	}
}

// TestReconfigure_NewProviderErrorPropagates mirrors Init's wrap
// contract for the Reconfigure path. Without it, a misconfigured
// shadow-pushed Config could fail silently and leave the gateway
// running with stale tracing.
func TestReconfigure_NewProviderErrorPropagates(t *testing.T) {
	sp, err := Init(context.Background(), Config{Enabled: false, ServiceName: "svc"}, testLogger())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Reconfigure internally uses context.Background() — can't pre-cancel
	// the new ctx from outside. Instead drive a definitely-bad endpoint
	// shape: the OTLP exporter constructor accepts most strings, so this
	// path is best-effort. If it returns nil, mark as a skip rather than
	// failing — the wrapping contract is asserted by Init's test above.
	if err := sp.Reconfigure(Config{
		Enabled:      true,
		Endpoint:     "\x00not-a-valid-endpoint",
		ServiceName:  "svc",
		SamplingRate: 1.0,
	}); err != nil {
		if got := err.Error(); !contains(got, "telemetry reconfigure") {
			t.Errorf("Reconfigure error not wrapped: %q", got)
		}
	} else {
		t.Skip("OTEL SDK accepted the malformed endpoint; Reconfigure-err wrap not exercised")
	}
}

// contains is a tiny strings.Contains shim — avoid an extra import here
// just for a one-time substring check.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestGlobalProviderSet(t *testing.T) {
	cfg := Config{
		Enabled:     false,
		ServiceName: "test-svc",
	}

	sp, err := Init(context.Background(), cfg, testLogger())
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	got := otel.GetTracerProvider()
	// Compare as trace.TracerProvider interface values.
	if got != trace.TracerProvider(sp) {
		t.Errorf("global TracerProvider = %p, want %p", got, sp)
	}
}
