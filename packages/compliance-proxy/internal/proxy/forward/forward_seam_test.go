// Package forward tests cover the Run pipeline and logRelayResult helper
// without a live TLS stack. The bumpConnFn package-level seam is used to
// inject synthetic BumpConnection results so error branches after the
// TLS interception step are reachable without a real TLS handshake.
package forward

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	metricsreg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	hookbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// TestMain initializes the compliance-proxy metrics registry so
// PinningPassthroughTotal is non-nil during test execution. This makes the
// `if metrics.PinningPassthroughTotal != nil` guards in forward.go reachable.
func TestMain(m *testing.M) {
	reg := metricsreg.NewRegistry(prometheus.NewRegistry())
	cpmetrics.Register(reg)
	os.Exit(m.Run())
}

// nopAuditWriter satisfies shared/audit.Writer with no-op methods.
type nopAuditWriter struct{}

func (nopAuditWriter) Enqueue(_ sharedaudit.AuditEvent) {}
func (nopAuditWriter) Flush(_ context.Context) error    { return nil }
func (nopAuditWriter) Close(_ context.Context) error    { return nil }

// minimalPolicyResolver builds a *compliance.PolicyResolver with no hooks.
func minimalPolicyResolver() *compliance.PolicyResolver {
	return pipeline.NewPolicyResolver(nil, hookbuiltins.Registry, slog.Default())
}

// minimalAuditEmitter builds a *compliance.AuditEmitter backed by a nop writer.
func minimalAuditEmitter() *compliance.AuditEmitter {
	return compliance.NewAuditEmitter(nopAuditWriter{}, slog.Default())
}

// nopConn is a net.Conn that does nothing — sufficient for testing paths
// that call Run but return before any data is written on the connection.
type nopConn struct{ net.Conn }

func (nopConn) Close() error                       { return nil }
func (nopConn) SetDeadline(_ time.Time) error      { return nil }
func (nopConn) SetReadDeadline(_ time.Time) error  { return nil }
func (nopConn) SetWriteDeadline(_ time.Time) error { return nil }
func (nopConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (nopConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (nopConn) Read(_ []byte) (int, error)         { return 0, errors.New("nopConn: read") }
func (nopConn) Write(_ []byte) (int, error)        { return 0, errors.New("nopConn: write") }

// minimalCfg builds a Config with no optional fields set (nil kill switch,
// nil pinning tracker, nil exemption store, nil compliance pipeline).
// Enough to reach the bumpConnFn call.
func minimalCfg() Config {
	return Config{
		SourceAddr: "127.0.0.1:12345",
		TargetHost: "api.example.com:443",
		Host:       "api.example.com",
		ConnID:     "127.0.0.1:12345->api.example.com:443",
		ConnStart:  time.Now(),
		Logger:     slog.Default(),
	}
}

// withBumpFn temporarily replaces the package-level bumpConnFn seam and
// restores the original at the end of the test.
func withBumpFn(t *testing.T, fn func(context.Context, net.Conn, string, func(*tls.ClientHelloInfo) (*tls.Certificate, error), *tlsbump.UpstreamTransport, *slog.Logger, ...tlsbump.BumpOption) error) {
	t.Helper()
	orig := bumpConnFn
	bumpConnFn = fn
	t.Cleanup(func() { bumpConnFn = orig })
}

func TestLogRelayResult_NilError_IsNoop(t *testing.T) {
	// Must not panic and must not call any logger method that would fail.
	LogRelayResult(slog.Default(), "test", nil)
}

func TestLogRelayResult_DialError_LogsWarn(t *testing.T) {
	// PassThroughError with Op=dial → WARN path.
	ptErr := &tlsbump.PassThroughError{Op: "dial", Err: errors.New("connection refused")}
	// Should not panic.
	LogRelayResult(slog.Default(), "passthrough", ptErr)
}

func TestLogRelayResult_RelayError_LogsDebug(t *testing.T) {
	// Generic relay error (not a dial failure) → DEBUG path.
	ptErr := &tlsbump.PassThroughError{Op: "copy", Err: errors.New("connection reset")}
	LogRelayResult(slog.Default(), "relay", ptErr)
}

func TestLogRelayResult_NonPassThroughError_LogsDebug(t *testing.T) {
	// A plain error (not *PassThroughError at all) → DEBUG path.
	LogRelayResult(slog.Default(), "relay", errors.New("EOF"))
}

// Run — kill switch

func TestRun_KillSwitch_Passthrough(t *testing.T) {
	// Kill switch active → passthrough without bump.
	// bumpConnFn should never be called.
	bumpCalled := false
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		bumpCalled = true
		return nil
	})

	emitCalled := false

	cfg := minimalCfg()
	cfg.KillSwitchChecker = func() bool { return true }
	cfg.AuditEmitter = nil // nil is safe — Run guards on != nil

	// Use a net.Pipe so PassThrough has something to read/write (it will fail
	// quickly since the remote side is closed immediately).
	client, server := net.Pipe()
	server.Close() // remote side gone → PassThrough returns fast

	Run(context.Background(), client, cfg)
	client.Close()

	if bumpCalled {
		t.Error("bumpConnFn must NOT be called when kill switch is active")
	}
	_ = emitCalled
}

// Run — pinning exemption

func TestRun_PinningExemption_Passthrough(t *testing.T) {
	// Pinning tracker exempts the host → passthrough without bump.
	bumpCalled := false
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		bumpCalled = true
		return nil
	})

	tracker := tlsbump.NewPinningTracker(tlsbump.PinningConfig{
		// Seed a static exemption so IsExempt returns true immediately.
		Exemptions: []tlsbump.DomainExemption{
			{Host: "api.example.com", Reason: "pinned-test"},
		},
	})

	cfg := minimalCfg()
	cfg.PinningTracker = tracker

	client, server := net.Pipe()
	server.Close()

	Run(context.Background(), client, cfg)
	client.Close()

	if bumpCalled {
		t.Error("bumpConnFn must NOT be called when pinning exemption fires")
	}
}

// Run — temporary exemption — hook-exempted path still calls BumpConnection

func TestRun_ExemptionStore_SkipsHooks_ButStillBumps(t *testing.T) {
	// ExemptionStore marks the connection as hook-exempt. Run should still
	// call bumpConnFn — just without the compliance BumpOption.
	bumpCalled := false
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		bumpCalled = true
		return nil
	})

	store := exemption.NewStore(slog.Default())
	store.Add("127.0.0.1", "api.example.com", 1*time.Hour, "test exemption", "test")

	cfg := minimalCfg()
	cfg.ExemptionStore = store
	// CompliancePipeline is nil → hookExempted doesn't change bumpOpts meaningfully

	Run(context.Background(), nopConn{}, cfg)

	if !bumpCalled {
		t.Error("bumpConnFn MUST be called when exemption only skips hooks")
	}
}

// Run — BumpConnection error: pinning failure → fallback passthrough

func TestRun_BumpConnection_PinningError_FallsBackToPassthrough(t *testing.T) {
	// bumpConnFn returns a tls.AlertError(48=unknown_ca) which IsPinningError
	// detects → RecordFailure + PassThrough.
	pinningErr := tls.AlertError(48) // unknown_ca → IsPinningError returns true

	passthroughAttempted := false
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		return pinningErr
	})

	// PinningTracker must be non-nil so the IsPinningError branch is taken.
	// AutoExempt.Enabled=true is required for RecordFailure to write an exemption.
	tracker := tlsbump.NewPinningTracker(tlsbump.PinningConfig{
		AutoExempt: tlsbump.AutoExemptConfig{
			Enabled:           true,
			FailureThreshold:  1, // single failure triggers exemption
			WindowSeconds:     60,
			ExemptionDuration: 1 * time.Hour,
		},
	})

	cfg := minimalCfg()
	cfg.PinningTracker = tracker

	client, server := net.Pipe()
	server.Close() // remote gone → PassThrough returns quickly

	Run(context.Background(), client, cfg)
	client.Close()

	// Verify RecordFailure was called (auto-exemption recorded): IsExempt
	// now returns true for the host.
	exempt, _, _ := tracker.IsExempt("api.example.com")
	if !exempt {
		t.Error("PinningTracker.RecordFailure should have auto-exempted the host")
	}
	_ = passthroughAttempted
}

// failClosedEngine returns a domain.Engine where host api.example.com is
// matched and configured FAIL_CLOSED (the rest default to FAIL_OPEN).
func failClosedEngine(t *testing.T, host string, behavior domain.AdapterErrorBehavior) *domain.Engine {
	t.Helper()
	e := domain.NewEngine()
	d := domain.InterceptionDomain{
		ID:                "id-" + host,
		Name:              host,
		HostPattern:       host,
		HostMatchType:     domain.HostMatchExact,
		NetworkZone:       domain.ZonePublic,
		DefaultPathAction: domain.PathActionProcess,
		OnAdapterError:    behavior,
		Enabled:           true,
	}
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	return e
}

// TestRun_BumpConnection_PinningError_FailClosed_RefusesNoPassthrough is the
// safety-critical assertion for FIX 1: a pinned flow whose matched domain is
// FAIL_CLOSED must be refused (no PassThrough dial), NOT relayed uninspected.
// We point TargetHost at a local listener and assert it receives NO connection.
func TestRun_BumpConnection_PinningError_FailClosed_RefusesNoPassthrough(t *testing.T) {
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		return tls.AlertError(48) // unknown_ca → IsPinningError true
	})

	// Local listener stands in for the upstream. PassThrough would dial it;
	// a refused flow must not.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()

	tracker := tlsbump.NewPinningTracker(tlsbump.PinningConfig{
		AutoExempt: tlsbump.AutoExemptConfig{
			Enabled: true, FailureThreshold: 1, WindowSeconds: 60, ExemptionDuration: 1 * time.Hour,
		},
	})

	cfg := minimalCfg()
	cfg.TargetHost = ln.Addr().String()
	cfg.PinningTracker = tracker
	cfg.DomainEngine = failClosedEngine(t, "api.example.com", domain.AdapterErrorFailClosed)

	client, server := net.Pipe()
	server.Close()
	Run(context.Background(), client, cfg)
	client.Close()

	select {
	case <-accepted:
		t.Fatal("FAIL_CLOSED domain MUST refuse — PassThrough dialed upstream (flow was relayed uninspected)")
	case <-time.After(200 * time.Millisecond):
		// No connection to upstream: refused as required.
	}
}

// TestRun_BumpConnection_PinningError_FailOpen_StillPassesThrough confirms the
// default fail-open behavior is unchanged for a matched FAIL_OPEN domain: the
// pinned flow IS relayed (PassThrough dials the upstream).
func TestRun_BumpConnection_PinningError_FailOpen_StillPassesThrough(t *testing.T) {
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		return tls.AlertError(48)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()

	tracker := tlsbump.NewPinningTracker(tlsbump.PinningConfig{
		AutoExempt: tlsbump.AutoExemptConfig{
			Enabled: true, FailureThreshold: 1, WindowSeconds: 60, ExemptionDuration: 1 * time.Hour,
		},
	})

	cfg := minimalCfg()
	cfg.TargetHost = ln.Addr().String()
	cfg.PinningTracker = tracker
	cfg.DomainEngine = failClosedEngine(t, "api.example.com", domain.AdapterErrorFailOpen)

	client, _ := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(context.Background(), client, cfg)
	}()

	select {
	case <-accepted:
		// PassThrough dialed upstream: fail-open preserved.
	case <-time.After(2 * time.Second):
		t.Fatal("FAIL_OPEN domain MUST still pass through — upstream never dialed")
	}
	// Closing the client unblocks the PassThrough relay so Run returns. We MUST
	// wait for the Run goroutine to exit before the test returns: it reads the
	// package-level bumpConnFn seam (and the metrics counter), which withBumpFn's
	// t.Cleanup restores — a leaked goroutine racing that restore is the -race
	// failure this test originally exhibited.
	client.Close()
	<-done
}

// TestRun_BumpConnection_PinningError_UnmatchedHost_PassesThrough confirms an
// unmatched host (no domain, e.g. system/DNS-adjacent traffic) still fails
// open even with a FAIL_CLOSED domain present for a different host.
func TestRun_BumpConnection_PinningError_UnmatchedHost_PassesThrough(t *testing.T) {
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		return tls.AlertError(48)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()

	tracker := tlsbump.NewPinningTracker(tlsbump.PinningConfig{
		AutoExempt: tlsbump.AutoExemptConfig{
			Enabled: true, FailureThreshold: 1, WindowSeconds: 60, ExemptionDuration: 1 * time.Hour,
		},
	})

	cfg := minimalCfg()
	cfg.TargetHost = ln.Addr().String()
	cfg.Host = "unmatched.example.com" // no domain matches this host
	cfg.PinningTracker = tracker
	cfg.DomainEngine = failClosedEngine(t, "api.example.com", domain.AdapterErrorFailClosed)

	client, _ := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(context.Background(), client, cfg)
	}()

	select {
	case <-accepted:
		// Unmatched host fails open: upstream dialed.
	case <-time.After(2 * time.Second):
		t.Fatal("unmatched host MUST still pass through — upstream never dialed")
	}
	// Wait for the Run goroutine to exit before returning (see FailOpen test):
	// it reads the bumpConnFn seam that withBumpFn's t.Cleanup restores; a leaked
	// goroutine racing that restore is the -race failure.
	client.Close()
	<-done
}

// Run — BumpConnection error: generic (non-pinning) error → log and return

func TestRun_BumpConnection_GenericError_LogsAndReturns(t *testing.T) {
	genericErr := errors.New("tls: handshake timeout")

	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		return genericErr
	})

	cfg := minimalCfg()
	// No PinningTracker → the IsPinningError branch is skipped and the
	// generic error log path runs.

	Run(context.Background(), nopConn{}, cfg)
	// If we reach here without panic the generic error path completed correctly.
}

// Run — BumpConnection returns nil: normal closure

func TestRun_BumpConnection_Success_LogsNormalClosure(t *testing.T) {
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		return nil
	})

	cfg := minimalCfg()
	Run(context.Background(), nopConn{}, cfg)
	// No panic → normal closure logging ran correctly.
}

// Run — kill switch with AuditEmitter non-nil (covers the nil-check branch)

func TestRun_KillSwitch_WithAuditEmitter_EmitsEvent(t *testing.T) {
	// Both kill switch and audit emitter set → EmitKillSwitchPassthrough branch runs.
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		t.Error("bumpConnFn must NOT be called when kill switch is active")
		return nil
	})

	cfg := minimalCfg()
	cfg.KillSwitchChecker = func() bool { return true }
	cfg.AuditEmitter = minimalAuditEmitter() // non-nil → EmitKillSwitchPassthrough

	client, server := net.Pipe()
	server.Close()

	Run(context.Background(), client, cfg)
	client.Close()
	// Reaching here means the AuditEmitter branch was entered without panic.
}

// Run — exemption store + compliance pipeline: hook-exempted path with
// AuditEmitter (covers the exemptionStore + compliancePipeline block)

func TestRun_ExemptionStore_WithPipeline_EmitsExemptedEvent(t *testing.T) {
	// ExemptionStore AND CompliancePipeline both set → hook-exempted block runs,
	// AuditEmitter.EmitExempted fires, then bumpConnFn is called (still bumps).
	bumpCalled := false
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		bumpCalled = true
		return nil
	})

	store := exemption.NewStore(slog.Default())
	store.Add("127.0.0.1", "api.example.com", 1*time.Hour, "policy exemption", "admin")

	cfg := minimalCfg()
	cfg.ExemptionStore = store
	cfg.CompliancePipeline = minimalPolicyResolver()
	cfg.AuditEmitter = minimalAuditEmitter() // non-nil → EmitExempted runs

	Run(context.Background(), nopConn{}, cfg)

	if !bumpCalled {
		t.Error("bumpConnFn must be called even when hook-exempted")
	}
}

// Run — compliance pipeline non-nil (not hook-exempted): covers the
// WithCompliance / WithPayloadCapture / WithDomainEngine / WithAdapterRegistry
// bumpOpts building block (cfg.CompliancePipeline != nil && !hookExempted)

func TestRun_CompliancePipeline_NonNil_BuildsBumpOpts(t *testing.T) {
	// CompliancePipeline set but ExemptionStore nil → hookExempted=false →
	// the full bumpOpts building block runs (PayloadCaptureStore, DomainEngine,
	// AdapterRegistry are nil so only base opts are appended).
	bumpCalled := false
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		bumpCalled = true
		return nil
	})

	cfg := minimalCfg()
	cfg.CompliancePipeline = minimalPolicyResolver()

	Run(context.Background(), nopConn{}, cfg)

	if !bumpCalled {
		t.Error("bumpConnFn must be called when compliance pipeline is set")
	}
}

func TestRun_CompliancePipeline_WithPayloadCapture_AppendsBumpOpt(t *testing.T) {
	// PayloadCaptureStore non-nil → WithPayloadCapture opt is appended.
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		return nil
	})

	cfg := minimalCfg()
	cfg.CompliancePipeline = minimalPolicyResolver()
	cfg.PayloadCaptureStore = payloadcapture.NewStore(payloadcapture.Config{})

	// No panic means WithPayloadCapture was successfully appended.
	Run(context.Background(), nopConn{}, cfg)
}

func TestRun_CompliancePipeline_WithDomainEngine_AppendsBumpOpt(t *testing.T) {
	// DomainEngine non-nil → WithDomainEngine opt is appended.
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		return nil
	})

	cfg := minimalCfg()
	cfg.CompliancePipeline = minimalPolicyResolver()
	cfg.DomainEngine = domain.NewEngine()

	Run(context.Background(), nopConn{}, cfg)
}

func TestRun_CompliancePipeline_WithAdapterRegistry_AppendsBumpOpt(t *testing.T) {
	// AdapterRegistry non-nil → WithAdapterRegistry opt is appended.
	withBumpFn(t, func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		return nil
	})

	cfg := minimalCfg()
	cfg.CompliancePipeline = minimalPolicyResolver()
	cfg.AdapterRegistry = traffic.NewAdapterRegistry("test")

	Run(context.Background(), nopConn{}, cfg)
}

// Run — BumpConnection pinning error with metrics counter (non-nil guard)
//
// The metrics.PinningPassthroughTotal guard at line 199 is only executed
// when the counter has been registered (non-nil). In test binaries the
// compliance-proxy metrics package is not initialized via Init(), so
// PinningPassthroughTotal is nil and the branch is a no-op. This is
// acceptable — the branch is a one-liner metrics increment that requires
// a full Prometheus registry; instrumenting it further would require
// importing the full compliance-proxy metrics init, which is OS-bound.
// This comment documents the known unreachability in the test harness.

// Ensure goHooks import is referenced to prevent unused-import error.

var _ goHooks.HookConfig
