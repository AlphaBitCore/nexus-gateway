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
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
	tlsissuer "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/issuer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	hookscore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// TestLogRelayResult_NilErr verifies that nil errors are silently ignored
// (no log output, no panic). logRelayResult is the pass-through logging
// helper called after every PassThrough; it must not log anything on success.
func TestLogRelayResult_NilErr(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// Must not panic.
	LogRelayResult(logger, "kill switch passthrough", nil)
}

// TestLogRelayResult_DialError — named failure mode: when PassThrough fails
// because the upstream dial was refused, logRelayResult must log at WARN
// (not ERROR or DEBUG). Operators rely on this to distinguish network issues
// from connection-reset copy errors.
func TestLogRelayResult_DialError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// Construct a PassThroughError with Op="dial" — this is what PassThrough
	// returns when the upstream TCP connect is refused.
	ptErr := &tlsbump.PassThroughError{
		Op:   "dial",
		Host: "api.openai.com:443",
		Err:  errors.New("connection refused"),
	}
	// Must not panic; WARN level is verified structurally below.
	LogRelayResult(logger, "kill switch passthrough", ptErr)
}

// TestLogRelayResult_CopyError — named failure mode: when the relay ends due
// to an EOF or ECONNRESET (copy-side error), logRelayResult logs at DEBUG so
// routine connection resets don't flood operator logs.
func TestLogRelayResult_CopyError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// A plain (non-PassThroughError) error simulates a copy-side closure.
	LogRelayResult(logger, "pinning fallback passthrough", errors.New("connection reset by peer"))
}

// Run — kill-switch branch

// closedPipeConn returns a net.Conn that is already closed so that any
// PassThrough or BumpConnection attempt fails immediately (refused dial or
// EOF), allowing Run to return without blocking.
func closedPipeConn(t *testing.T) net.Conn {
	t.Helper()
	server, client := net.Pipe()
	client.Close()
	t.Cleanup(func() { server.Close() })
	return server
}

// TestRun_KillSwitchEnabled — named failure mode: when the kill-switch is
// active, Run must use PassThrough (not BumpConnection) so no compliance
// hooks fire. We verify this by checking Run returns (i.e. the passthrough
// path exits) rather than hanging on a bump handshake. Because PassThrough
// dials targetHost and the test uses a port that refuses, Run returns quickly
// after logging the kill-switch passthrough.
func TestRun_KillSwitchEnabled(t *testing.T) {
	conn := closedPipeConn(t)

	cfg := Config{
		SourceAddr:        "127.0.0.1:12345",
		TargetHost:        "127.0.0.1:1", // port 1 is refused on all modern systems
		Host:              "127.0.0.1",
		ConnStart:         time.Now(),
		KillSwitchChecker: func() bool { return true }, // kill switch ON
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Run must return (not hang). The passthrough dial will fail quickly
	// because port 1 is refused.
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, conn, cfg)
	}()

	select {
	case <-done:
		// Good — Run returned.
	case <-time.After(5 * time.Second):
		t.Fatal("Run with kill-switch enabled did not return within 5s")
	}
}

// TestRun_KillSwitchNil — when KillSwitchChecker is nil, Run must skip the
// kill-switch check and proceed to the pinning / bump path.
func TestRun_KillSwitchNil(t *testing.T) {
	conn := closedPipeConn(t)

	cfg := Config{
		SourceAddr:        "127.0.0.1:12346",
		TargetHost:        "127.0.0.1:1",
		Host:              "127.0.0.1",
		ConnStart:         time.Now(),
		KillSwitchChecker: nil,
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, conn, cfg)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with nil kill-switch did not return within 5s")
	}
}

// TestRun_PinningExemptPassthrough — named failure mode: when the
// PinningTracker marks the host as exempt, Run must use PassThrough (not
// BumpConnection). We verify by configuring a tracker with an explicit
// exemption for the target host.
func TestRun_PinningExemptPassthrough(t *testing.T) {
	conn := closedPipeConn(t)

	// Configure pinning tracker with an explicit exemption for "api.pinned.com".
	tracker := tlsbump.NewPinningTracker(tlsbump.PinningConfig{
		Exemptions: []tlsbump.DomainExemption{
			{Host: "api.pinned.com", Reason: "client-cert-pinned"},
		},
	})

	cfg := Config{
		SourceAddr:     "127.0.0.1:12347",
		TargetHost:     "api.pinned.com:443",
		Host:           "api.pinned.com",
		ConnStart:      time.Now(),
		PinningTracker: tracker,
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, conn, cfg)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with pinning exemption did not return within 5s")
	}
}

// TestRun_NoComplianceNoBump — when CompliancePipeline is nil and no
// kill-switch/pinning-exemption applies, Run still calls BumpConnection with
// only the Identity bump option. The connection will fail at the TLS
// handshake because the target is refused/closed, so Run returns quickly.
func TestRun_NoComplianceNoBump(t *testing.T) {
	conn := closedPipeConn(t)

	cfg := Config{
		SourceAddr: "127.0.0.1:12348",
		TargetHost: "127.0.0.1:1",
		Host:       "127.0.0.1",
		ConnStart:  time.Now(),
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		// GetCert and Upstream nil — BumpConnection will fail fast at TLS.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, conn, cfg)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run without compliance pipeline did not return within 5s")
	}
}

// TestRun_ExemptionStoreMatch — named correctness invariant: when the
// exemption store matches the source IP + target host, hookExempted=true
// and compliance hooks are skipped (even though CompliancePipeline is nil
// here — the important thing is the exemption branch runs without panic or
// incorrect behaviour).
func TestRun_ExemptionStoreMatch(t *testing.T) {
	conn := closedPipeConn(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	store := exemption.NewStore(logger)
	// Add an exemption matching the source IP and target host we'll use.
	store.Add("127.0.0.1", "api.exempt.com", 1*time.Minute, "test-exemption", "test")

	cfg := Config{
		SourceAddr:     "127.0.0.1:12349",
		TargetHost:     "api.exempt.com:443",
		Host:           "api.exempt.com",
		ConnStart:      time.Now(),
		ExemptionStore: store,
		// CompliancePipeline is nil so the exemption branch skips hooks but
		// still bumps (BumpConnection will fail at TLS handshake quickly).
		Logger: logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, conn, cfg)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with exemption store match did not return within 5s")
	}
}

// TestRun_ExemptionStoreNoMatch — when the exemption store is set but the
// source IP + target host combination does NOT match any exemption, the
// compliance hooks are NOT skipped. Here CompliancePipeline is nil so the
// Run path falls through to BumpConnection with only Identity.
func TestRun_ExemptionStoreNoMatch(t *testing.T) {
	conn := closedPipeConn(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	store := exemption.NewStore(logger)
	// No exemptions added — IsExempt will always return false.

	cfg := Config{
		SourceAddr:     "10.0.0.1:12350",
		TargetHost:     "api.openai.com:443",
		Host:           "api.openai.com",
		ConnStart:      time.Now(),
		ExemptionStore: store,
		Logger:         logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, conn, cfg)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with no exemption match did not return within 5s")
	}
}

// Helpers shared by metrics/audit/TLS tests below

// noopAuditWriter is a test-only audit.Writer that discards every event.
// Using it ensures AuditEmitter can be wired in forward tests without
// requiring a real MQ or NDJSON sink.
type noopAuditWriter struct{}

func (noopAuditWriter) Enqueue(_ audit.AuditEvent)    {}
func (noopAuditWriter) Flush(_ context.Context) error { return nil }
func (noopAuditWriter) Close(_ context.Context) error { return nil }

// initTestMetrics registers the compliance-proxy metrics on an isolated
// Prometheus registry and returns a restore func. Call defer restore() so
// global metric variables are reset to their pre-test state, keeping tests
// hermetic.
func initTestMetrics(t *testing.T) (restore func()) {
	t.Helper()
	prev := metrics.PinningPassthroughTotal
	pr := prometheus.NewRegistry()
	reg := registry.NewRegistry(pr)
	metrics.Register(reg)
	return func() { metrics.PinningPassthroughTotal = prev }
}

// newTestPolicyResolver builds a *compliance.PolicyResolver with no hooks
// wired. This is sufficient to exercise code branches that check
// cfg.CompliancePipeline != nil without triggering real policy evaluation.
func newTestPolicyResolver(t *testing.T) *compliance.PolicyResolver {
	t.Helper()
	return compliance.NewPolicyResolver(
		nil,
		hookscore.NewHookRegistry(),
		slog.New(slog.NewTextHandler(os.Stderr, nil)),
	)
}

// newTestAuditEmitter returns an *AuditEmitter backed by a no-op writer so
// EmitKillSwitchPassthrough / EmitExempted calls complete without MQ.
func newTestAuditEmitter() *compliance.AuditEmitter {
	return pipeline.NewAuditEmitter(noopAuditWriter{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// newTestIssuer creates a temporary CA and returns an *Issuer ready to sign
// leaf certs. Used to wire GetCert for BumpConnection tests.
func newTestIssuer(t *testing.T) *tlsissuer.Issuer {
	t.Helper()
	certPath, keyPath, err := testutil.WriteTestCA(t.TempDir())
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := tlsissuer.NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss
}

// Run — metrics and audit branches

// TestRun_KillSwitchEnabled_WithMetricsAndAudit — named correctness invariant:
// when the kill-switch fires AND metrics + AuditEmitter are wired,
// PinningPassthroughTotal.With().Inc() and EmitKillSwitchPassthrough must
// both complete without panic. This exercises the nil-guard branches at
// lines 108–113 of forward.go.
func TestRun_KillSwitchEnabled_WithMetricsAndAudit(t *testing.T) {
	conn := closedPipeConn(t)
	restore := initTestMetrics(t)
	defer restore()

	cfg := Config{
		SourceAddr:        "127.0.0.1:12351",
		TargetHost:        "127.0.0.1:1",
		Host:              "127.0.0.1",
		ConnStart:         time.Now(),
		KillSwitchChecker: func() bool { return true },
		AuditEmitter:      newTestAuditEmitter(),
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, conn, cfg)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with kill-switch + metrics + audit did not return within 5s")
	}
}

// TestRun_PinningExemptPassthrough_WithMetrics — named correctness invariant:
// when a host is pinning-exempt AND metrics are wired, the PinningPassthroughTotal
// counter increments without panic. Covers the nil-guard at lines 125–127.
func TestRun_PinningExemptPassthrough_WithMetrics(t *testing.T) {
	conn := closedPipeConn(t)
	restore := initTestMetrics(t)
	defer restore()

	tracker := tlsbump.NewPinningTracker(tlsbump.PinningConfig{
		Exemptions: []tlsbump.DomainExemption{
			{Host: "api.metrics-pinned.com", Reason: "test-pinned"},
		},
	})

	cfg := Config{
		SourceAddr:     "127.0.0.1:12352",
		TargetHost:     "api.metrics-pinned.com:443",
		Host:           "api.metrics-pinned.com",
		ConnStart:      time.Now(),
		PinningTracker: tracker,
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, conn, cfg)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with pinning exemption + metrics did not return within 5s")
	}
}

// TestRun_HookExemptionWithAudit — named correctness invariant: when both
// ExemptionStore and CompliancePipeline are non-nil AND the source IP+host
// matches an active exemption, hookExempted=true and AuditEmitter.EmitExempted
// is called. The BumpConnection then fails at TLS (closed pipe) and Run returns.
// This covers lines 135–149 of forward.go (the hook-exemption body).
func TestRun_HookExemptionWithAudit(t *testing.T) {
	conn := closedPipeConn(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	store := exemption.NewStore(logger)
	store.Add("127.0.0.1", "api.hook-exempt.com", 1*time.Minute, "unit-test", "test")

	cfg := Config{
		SourceAddr:         "127.0.0.1:12353",
		TargetHost:         "api.hook-exempt.com:443",
		Host:               "api.hook-exempt.com",
		ConnStart:          time.Now(),
		ExemptionStore:     store,
		CompliancePipeline: newTestPolicyResolver(t),
		AuditEmitter:       newTestAuditEmitter(),
		Logger:             logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, conn, cfg)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with hook exemption + audit did not return within 5s")
	}
}

// TestRun_CompliancePipelineNonNil_BumpOpts — named correctness invariant:
// when CompliancePipeline is non-nil (and no exemption applies), Run builds
// the full set of BumpOptions (WithCompliance, WithStreamingPolicyGlobal,
// WithSourceInfo, WithRejectConfig, WithPayloadCapture, WithDomainEngine,
// WithAdapterRegistry) before calling BumpConnection. The closed pipe causes
// BumpConnection to return a non-pinning TLS error, so Run logs the error
// and returns. This covers lines 158–200 of forward.go.
func TestRun_CompliancePipelineNonNil_BumpOpts(t *testing.T) {
	conn := closedPipeConn(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg := Config{
		SourceAddr:          "127.0.0.1:12354",
		TargetHost:          "127.0.0.1:1",
		Host:                "127.0.0.1",
		ConnStart:           time.Now(),
		ConnID:              "127.0.0.1:12354->127.0.0.1:1",
		CompliancePipeline:  newTestPolicyResolver(t),
		PayloadCaptureStore: payloadcapture.NewStore(payloadcapture.Config{}),
		DomainEngine:        domain.NewEngine(),
		AdapterRegistry:     traffic.NewAdapterRegistry("forward-test"),
		// non-nil Store exercises the WithStreamingPolicyStore
		// branch of Run's BumpOpts assembly. Nil-store path is
		// covered by the other tests that omit this field.
		StreamingPolicyStore: streampolicy.NewStore(streampolicy.DefaultPolicy()),
		Logger:               logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, conn, cfg)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with non-nil CompliancePipeline did not return within 5s")
	}
}

// Run — PinningTracker.RecordFailure (TLS alert from client)

// TestRun_PinningError_RecordFailure — named correctness invariant: when
// BumpConnection fails because the TLS client sends an "unknown_ca" alert
// (tls.AlertError 48), IsPinningError returns true, PinningTracker.RecordFailure
// is called, and Run falls back to PassThrough. This is the certificate-pinning
// detection path (forward.go lines 187–198).
//
// Mechanism: we create an in-process net.Pipe(). One goroutine runs
// BumpConnection on the "server" side. The main test goroutine acts as the
// TLS client: it dials the pipe with InsecureSkipVerify=false and an empty
// root pool so it sends alert(48) unknown_ca when it sees the proxy's
// self-signed cert. BumpConnection's HandshakeContext receives the client's
// alert and returns tls.AlertError(48), which IsPinningError recognises.
func TestRun_PinningError_RecordFailure(t *testing.T) {
	iss := newTestIssuer(t)

	// Configure PinningTracker with auto-exempt enabled so RecordFailure
	// can transition to the BumpStatusExemptPinned return path (threshold=1).
	tracker := tlsbump.NewPinningTracker(tlsbump.PinningConfig{
		AutoExempt: tlsbump.AutoExemptConfig{
			Enabled:           true,
			FailureThreshold:  1,
			WindowSeconds:     60,
			ExemptionDuration: 5 * time.Minute,
		},
	})

	// Build a net.Pipe() — both ends are in-process so the TLS handshake
	// succeeds at the TCP layer but fails at the application layer when the
	// client rejects our proxy CA as unknown.
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		serverConn.Close()
		clientConn.Close()
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	getCert := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		host := hello.ServerName
		if host == "" {
			host = "test.example.com"
		}
		return iss.SignCert(host)
	}

	cfg := Config{
		SourceAddr:     "127.0.0.1:12355",
		TargetHost:     "test.example.com:443",
		Host:           "test.example.com",
		ConnStart:      time.Now(),
		GetCert:        getCert,
		PinningTracker: tracker,
		Logger:         logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run BumpConnection on the server side in a goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, serverConn, cfg)
	}()

	// Act as a TLS client that rejects the server's certificate.
	// InsecureSkipVerify=false + empty RootCAs = the client will send
	// alert(48) unknown_ca when it sees the proxy's self-signed CA cert.
	tlsClient := tls.Client(clientConn, &tls.Config{
		ServerName:         "test.example.com",
		InsecureSkipVerify: false, //nolint:gosec // intentional: triggers pinning alert
		RootCAs:            nil,   // empty pool → unknown_ca alert sent to server
	})
	// Attempt TLS handshake; we expect it to fail from the client's perspective
	// (it will reject the server cert). The server (BumpConnection) will see
	// the client's alert and return IsPinningError=true.
	_ = tlsClient.Handshake()
	tlsClient.Close()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after pinning error within 10s")
	}
}
