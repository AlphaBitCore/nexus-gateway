package middleware

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	hooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
)

// stubConnHook is a connection-stage-compatible test hook whose decision is
// configurable. It asserts it runs by setting a flag, and records the input
// it received so tests can verify the middleware populated fields.
type stubConnHook struct {
	hooks.AnyEndpointAnyModality
	decision hooks.Decision
	reason   string
	called   bool
	lastIn   *hooks.HookInput
}

func (h *stubConnHook) Execute(ctx context.Context, in *hooks.HookInput) (*hooks.HookResult, error) {
	h.called = true
	h.lastIn = in
	return &hooks.HookResult{Decision: h.decision, Reason: h.reason}, nil
}

func (*stubConnHook) ConnectionStageOK() {}

func testSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildResolverWithHook creates a PolicyResolver with a single connection-stage
// hook bound to the given stub. ApplicableIngress is ["ALL"] so it matches
// every ingress the tests may pass.
func buildResolverWithHook(t *testing.T, h *stubConnHook) *compliance.PolicyResolver {
	t.Helper()
	registry := hooks.NewHookRegistry()
	registry.Register("stub-conn", func(cfg *hooks.HookConfig) (hooks.Hook, error) {
		return h, nil
	})
	return compliance.NewPolicyResolver([]hooks.HookConfig{
		{
			ID:                "h1",
			ImplementationID:  "stub-conn",
			Name:              "stub-connection-hook",
			Stage:             "connection",
			Enabled:           true,
			FailBehavior:      "fail-open",
			ApplicableIngress: []string{"ALL"},
		},
	}, registry, testSlog())
}

// buildResolverWithNoConnectionHooks returns a resolver whose only hook is a
// request-stage hook, so BuildPipeline("connection", ...) returns (nil, nil).
func buildResolverWithNoConnectionHooks(t *testing.T) *compliance.PolicyResolver {
	t.Helper()
	registry := hooks.NewHookRegistry()
	registry.Register("stub-req", func(cfg *hooks.HookConfig) (hooks.Hook, error) {
		return &stubConnHook{decision: hooks.Approve}, nil
	})
	return compliance.NewPolicyResolver([]hooks.HookConfig{
		{
			ID:                "h-req",
			ImplementationID:  "stub-req",
			Name:              "request-only",
			Stage:             "request",
			Enabled:           true,
			FailBehavior:      "fail-open",
			ApplicableIngress: []string{"ALL"},
		},
	}, registry, testSlog())
}

func TestConnectionStage_RejectHard_Returns403(t *testing.T) {
	stub := &stubConnHook{decision: hooks.RejectHard, reason: "blocked by test"}
	resolver := buildResolverWithHook(t, stub)
	supplier := func(ctx context.Context) (*compliance.PolicyResolver, error) {
		return resolver, nil
	}

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := ConnectionStage(supplier, time.Second, 5*time.Second, "AI_GATEWAY", testSlog())
	h := mw(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "10.0.0.1:4242"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "blocked by test") {
		t.Fatalf("body = %q, want substring %q", w.Body.String(), "blocked by test")
	}
	if nextCalled {
		t.Fatal("next handler must NOT be called on RejectHard")
	}
	if !stub.called {
		t.Fatal("hook must have been invoked")
	}
	if stub.lastIn == nil || stub.lastIn.Stage != "connection" {
		t.Fatalf("hook input stage = %q, want connection", stub.lastIn.Stage)
	}
	if stub.lastIn.SourceIP != "10.0.0.1" {
		t.Fatalf("SourceIP = %q, want 10.0.0.1", stub.lastIn.SourceIP)
	}
	if stub.lastIn.IngressType != "AI_GATEWAY" {
		t.Fatalf("IngressType = %q, want AI_GATEWAY", stub.lastIn.IngressType)
	}
}

func TestConnectionStage_RejectHard_EmptyReasonFallback(t *testing.T) {
	stub := &stubConnHook{decision: hooks.RejectHard, reason: ""}
	resolver := buildResolverWithHook(t, stub)
	supplier := func(ctx context.Context) (*compliance.PolicyResolver, error) {
		return resolver, nil
	}
	mw := ConnectionStage(supplier, time.Second, 5*time.Second, "AI_GATEWAY", testSlog())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("next called") }))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "connection blocked by compliance policy") {
		t.Fatalf("body = %q, want default reason", w.Body.String())
	}
}

func TestConnectionStage_Approve_CallsNext(t *testing.T) {
	stub := &stubConnHook{decision: hooks.Approve}
	resolver := buildResolverWithHook(t, stub)
	supplier := func(ctx context.Context) (*compliance.PolicyResolver, error) {
		return resolver, nil
	}

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := ConnectionStage(supplier, time.Second, 5*time.Second, "AI_GATEWAY", testSlog())
	h := mw(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !nextCalled {
		t.Fatal("next handler must be called on Approve")
	}
	if !stub.called {
		t.Fatal("hook must have been invoked")
	}
}

func TestConnectionStage_NoConnectionHooks_CallsNext(t *testing.T) {
	resolver := buildResolverWithNoConnectionHooks(t)
	supplier := func(ctx context.Context) (*compliance.PolicyResolver, error) {
		return resolver, nil
	}

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := ConnectionStage(supplier, time.Second, 5*time.Second, "AI_GATEWAY", testSlog())
	h := mw(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !nextCalled {
		t.Fatal("next handler must be called when no connection-stage hooks are configured")
	}
}

func TestConnectionStage_SupplierError_FailsOpen(t *testing.T) {
	supplier := func(ctx context.Context) (*compliance.PolicyResolver, error) {
		return nil, errors.New("resolver boom")
	}

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := ConnectionStage(supplier, time.Second, 5*time.Second, "AI_GATEWAY", testSlog())
	h := mw(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open on supplier error)", w.Code)
	}
	if !nextCalled {
		t.Fatal("next handler must be called when supplier errors (fail-open)")
	}
}

func TestConnectionStage_SupplierReturnsNil_FailsOpen(t *testing.T) {
	supplier := func(ctx context.Context) (*compliance.PolicyResolver, error) {
		return nil, nil
	}

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := ConnectionStage(supplier, time.Second, 5*time.Second, "AI_GATEWAY", testSlog())
	h := mw(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !nextCalled {
		t.Fatal("next handler must be called when supplier returns (nil, nil)")
	}
}

// TestConnectionStage_PipelineBuildError_FailsOpen covers the
// `pipe, err := resolver.BuildPipeline(...); if err != nil` branch:
// when BuildPipeline returns an error (here triggered by a hook factory
// that itself returns an error), the middleware must fail open (200 +
// next called).
func TestConnectionStage_PipelineBuildError_FailsOpen(t *testing.T) {
	registry := hooks.NewHookRegistry()
	registry.Register("boom-factory", func(_ *hooks.HookConfig) (hooks.Hook, error) {
		return nil, errors.New("factory unavailable")
	})
	resolver := compliance.NewPolicyResolver([]hooks.HookConfig{
		{
			ID:                "h-bad",
			ImplementationID:  "boom-factory",
			Name:              "factory-error",
			Stage:             "connection",
			Enabled:           true,
			FailBehavior:      "fail-open",
			ApplicableIngress: []string{"ALL"},
		},
	}, registry, testSlog())
	supplier := func(_ context.Context) (*compliance.PolicyResolver, error) {
		return resolver, nil
	}

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := ConnectionStage(supplier, time.Second, 5*time.Second, "AI_GATEWAY", testSlog())
	h := mw(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open on BuildPipeline error)", w.Code)
	}
	if !nextCalled {
		t.Fatal("next handler must be called on BuildPipeline error fail-open")
	}
}

func TestConnectionStage_NilSupplier_Noop(t *testing.T) {
	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := ConnectionStage(nil, time.Second, 5*time.Second, "AI_GATEWAY", testSlog())
	h := mw(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !nextCalled {
		t.Fatal("next handler must be called when supplier is nil (no-op wrap)")
	}
}

func TestClientIP_XForwardedFor_FirstEntry(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.2")
	if got := ClientIP(r); got != "203.0.113.7" {
		t.Fatalf("ClientIP = %q, want 203.0.113.7", got)
	}
}

func TestClientIP_XRealIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Real-IP", "198.51.100.5")
	if got := ClientIP(r); got != "198.51.100.5" {
		t.Fatalf("ClientIP = %q, want 198.51.100.5", got)
	}
}

func TestClientIP_RemoteAddrFallback(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.9:55123"
	if got := ClientIP(r); got != "192.0.2.9" {
		t.Fatalf("ClientIP = %q, want 192.0.2.9", got)
	}
}

func TestTLSInfoFromRequest_PlainHTTP_Nil(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := tlsInfoFromRequest(r); got != nil {
		t.Fatalf("tlsInfoFromRequest on plain HTTP = %+v, want nil", got)
	}
}
