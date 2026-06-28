package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// stubConnHook is a connection-stage-compatible test hook that records whether
// it ran and the input it received. Its decision and reason are configurable
// so tests can drive RejectHard vs Approve flows.
type stubConnHook struct {
	core.AnyEndpointAnyModality
	decision core.Decision
	reason   string
	called   bool
	lastIn   *core.HookInput
}

func (h *stubConnHook) Execute(_ context.Context, in *core.HookInput) (*core.HookResult, error) {
	h.called = true
	h.lastIn = in
	return &core.HookResult{Decision: h.decision, Reason: h.reason}, nil
}

// ConnectionStageOK opts this hook into stage="connection" so the resolver
// will admit it to the connection-stage pipeline.
func (*stubConnHook) ConnectionStageOK() {}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildConnResolverWithHook creates a PolicyResolver with a single
// connection-stage hook bound to the given stub. ApplicableIngress=["ALL"]
// matches the COMPLIANCE_PROXY ingress the listener passes.
func buildConnResolverWithHook(t *testing.T, h *stubConnHook) *compliance.PolicyResolver {
	t.Helper()
	registry := core.NewHookRegistry()
	registry.Register("stub-conn", func(cfg *core.HookConfig) (core.Hook, error) {
		return h, nil
	})
	return compliance.NewPolicyResolver([]core.HookConfig{
		{
			ID:                "h1",
			ImplementationID:  "stub-conn",
			Name:              "stub-connection-hook",
			Stage:             "connection",
			Enabled:           true,
			FailBehavior:      "fail-open",
			ApplicableIngress: []string{"ALL"},
		},
	}, registry, discardLogger())
}

// buildConnResolverWithRequestOnlyHook returns a resolver whose only hook is
// a request-stage hook, so BuildPipeline("connection", ...) returns (nil, nil)
// and the listener must fall through without invoking the stub.
func buildConnResolverWithRequestOnlyHook(t *testing.T) (*compliance.PolicyResolver, *stubConnHook) {
	t.Helper()
	stub := &stubConnHook{decision: core.Approve}
	registry := core.NewHookRegistry()
	registry.Register("stub-req-only", func(cfg *core.HookConfig) (core.Hook, error) {
		return stub, nil
	})
	resolver := compliance.NewPolicyResolver([]core.HookConfig{
		{
			ID:                "h-req",
			ImplementationID:  "stub-req-only",
			Name:              "request-only",
			Stage:             "request",
			Enabled:           true,
			FailBehavior:      "fail-open",
			ApplicableIngress: []string{"ALL"},
		},
	}, registry, discardLogger())
	return resolver, stub
}

// newConnectRequest builds a CONNECT request that the listener will treat as
// HTTP/1.1 (ProtoMajor=1). httptest.NewRecorder does not implement
// http.Hijacker, so any test that reaches establishTunnel will observe a 500
// from the tunnel layer — tests use that as the "passed the connection-stage
// gate" marker, distinct from the 403 the reject path produces.
func newConnectRequest(target string) *http.Request {
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Host = target
	req.RemoteAddr = "10.0.0.1:4242"
	return req
}

// TestServeHTTP_ConnectionStage_RejectHard_Returns403 asserts that a
// connection-stage hook returning RejectHard short-circuits the CONNECT
// handler with 403 Forbidden, the hook's reason appears in the body, and
// tunnel establishment is never attempted (observable because the hook ran
// and the response code is 403, not the 500 we would see from the recorder
// failing hijacking).
func TestServeHTTP_ConnectionStage_RejectHard_Returns403(t *testing.T) {
	stub := &stubConnHook{decision: core.RejectHard, reason: "blocked by test"}
	resolver := buildConnResolverWithHook(t, stub)

	p := &ProxyServer{
		logger:             discardLogger(),
		compliancePipeline: resolver,
	}

	req := newConnectRequest("example.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "blocked by test") {
		t.Fatalf("body = %q, want substring %q", w.Body.String(), "blocked by test")
	}
	if !stub.called {
		t.Fatal("hook must have been invoked")
	}
	if stub.lastIn == nil {
		t.Fatal("hook input must be recorded")
	}
	if stub.lastIn.Stage != "connection" {
		t.Fatalf("hook input stage = %q, want connection", stub.lastIn.Stage)
	}
	if stub.lastIn.TargetHost != "example.com" {
		t.Fatalf("TargetHost = %q, want example.com", stub.lastIn.TargetHost)
	}
	if stub.lastIn.SourceIP != "10.0.0.1" {
		t.Fatalf("SourceIP = %q, want 10.0.0.1", stub.lastIn.SourceIP)
	}
	if stub.lastIn.IngressType != "COMPLIANCE_PROXY" {
		t.Fatalf("IngressType = %q, want COMPLIANCE_PROXY", stub.lastIn.IngressType)
	}
	if stub.lastIn.Method != http.MethodConnect {
		t.Fatalf("Method = %q, want CONNECT", stub.lastIn.Method)
	}
}

// TestServeHTTP_ConnectionStage_RejectHard_EmptyReasonFallback asserts that a
// reject decision with an empty reason still yields 403 with the default
// fallback string — operators and clients must always see a non-empty body.
func TestServeHTTP_ConnectionStage_RejectHard_EmptyReasonFallback(t *testing.T) {
	stub := &stubConnHook{decision: core.RejectHard, reason: ""}
	resolver := buildConnResolverWithHook(t, stub)

	p := &ProxyServer{
		logger:             discardLogger(),
		compliancePipeline: resolver,
	}

	req := newConnectRequest("example.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "connection blocked by compliance policy") {
		t.Fatalf("body = %q, want default reason", w.Body.String())
	}
}

// TestServeHTTP_ConnectionStage_NilPipeline_PassThrough asserts that when no
// compliance pipeline is configured the listener proceeds past the
// connection-stage gate. Because httptest.NewRecorder does not implement
// http.Hijacker, establishTunnel responds 500; we use that as the
// "gate was skipped" signal and assert the response is NOT 403.
func TestServeHTTP_ConnectionStage_NilPipeline_PassThrough(t *testing.T) {
	p := &ProxyServer{
		logger:             discardLogger(),
		compliancePipeline: nil,
	}

	req := newConnectRequest("example.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		t.Fatalf("status = 403 with nil pipeline; the connection-stage gate must be skipped")
	}
	// We expect 500 here because the recorder can't be hijacked — that's the
	// marker that control advanced past the gate into the tunnel phase.
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (no hijacker)", w.Code)
	}
}

// TestServeHTTP_ConnectionStage_NoConnectionHooks_PassThrough asserts that a
// non-nil resolver with no connection-stage hooks (only a request-stage hook)
// falls through the gate. The request-stage stub must not be invoked, and
// the response must not be 403.
func TestServeHTTP_ConnectionStage_NoConnectionHooks_PassThrough(t *testing.T) {
	resolver, stub := buildConnResolverWithRequestOnlyHook(t)

	p := &ProxyServer{
		logger:             discardLogger(),
		compliancePipeline: resolver,
	}

	req := newConnectRequest("example.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		t.Fatalf("status = 403 with no connection-stage hooks; must pass through")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (no hijacker)", w.Code)
	}
	if stub.called {
		t.Fatal("request-stage hook must not run in connection-stage pipeline")
	}
}

// TestServeHTTP_ConnectionStage_StrictFailClosed_UnbuildableRefuses covers the
// CONNECT site. The ProxyServer passes
// strictFailClosed=true into BuildPipeline; a mandatory (fail-closed)
// connection-stage hook whose implementationId is unregistered makes
// BuildPipeline ERROR. The handler MUST refuse the CONNECT with 403 rather than
// failing open into the tunnel phase (which the 500-no-hijacker marker would
// indicate). The agent NE host path never reaches this code.
func TestServeHTTP_ConnectionStage_StrictFailClosed_UnbuildableRefuses(t *testing.T) {
	// Empty registry → the fail-closed hook's impl is unknown → unbuildable.
	registry := core.NewHookRegistry()
	resolver := compliance.NewPolicyResolver([]core.HookConfig{
		{
			ID:                "enforcer",
			ImplementationID:  "no-such-impl",
			Name:              "mandatory-connection-enforcer",
			Stage:             "connection",
			Enabled:           true,
			FailBehavior:      "fail-closed",
			ApplicableIngress: []string{"ALL"},
		},
	}, registry, discardLogger())

	p := &ProxyServer{
		logger:             discardLogger(),
		compliancePipeline: resolver,
	}

	req := newConnectRequest("example.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — an unbuildable fail-closed connection hook under strict MUST refuse the CONNECT, not fail open (500 tunnel marker)", w.Code)
	}
}

// TestServeHTTP_ConnectionStage_Approve_PassThrough asserts that when the
// connection-stage hook returns Approve, control flows past the gate into
// the tunnel phase. The hook must run; the response must not be 403.
func TestServeHTTP_ConnectionStage_Approve_PassThrough(t *testing.T) {
	stub := &stubConnHook{decision: core.Approve}
	resolver := buildConnResolverWithHook(t, stub)

	p := &ProxyServer{
		logger:             discardLogger(),
		compliancePipeline: resolver,
	}

	req := newConnectRequest("example.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if !stub.called {
		t.Fatal("hook must have been invoked")
	}
	if w.Code == http.StatusForbidden {
		t.Fatalf("status = 403 on Approve; must pass through")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (no hijacker)", w.Code)
	}
}
