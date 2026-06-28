package proxy_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/models"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/proxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/ratelimit"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// stubResolver lets the handler integration tests exercise
// pre-upstream validation without a live provider catalog or vault.
type stubResolver struct{}

func (stubResolver) Resolve(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	return provcore.CallTarget{
		ProviderID:      providerID,
		ProviderName:    "openai",
		BaseURL:         "http://127.0.0.1:0",
		APIKey:          "",
		ProviderModelID: modelID,
	}, nil
}

// testDeps creates handler.Deps with no DB (stubs VK auth and routing).
// The upstream URL is injected via the provider registry mock.
func testDeps(upstreamURL string) *proxy.Deps {
	logger := slog.Default()

	// Provider adapter registry populated with the nine built-in stub
	// AdapterSpecs. The s1 stubs return ErrNotImplemented; these tests
	// exercise pre-upstream validation only (missing model, bad VK).
	provReg := provcore.NewRegistry()
	provbuiltins.Register(provReg, nil, logger)

	// Hook config cache (no hooks configured — empty config snapshot).
	hookConfigCache := compliance.NewHookConfigCache(
		func(ctx context.Context) ([]goHooks.HookConfig, error) {
			return nil, nil
		},
		builtins.Registry,
		0, // no TTL in tests
		logger,
	)

	credMgr := credmanager.NewManager(nil, nil)
	healthTracker := store.NewHealthTracker()

	return &proxy.Deps{
		Models:          nil,                            // no DB in test
		VKAuth:          nil,                            // bypassed — we test VK auth separately
		RateLimiter:     ratelimit.NewLocalOnly(logger), // satisfies handler.RateLimiter
		CredManager:     credMgr,                        // satisfies handler.CredentialLookup
		Router:          nil,                            // bypassed — we test routing separately
		Executor:        executor.New(provReg, &stubResolver{}, healthTracker, nil),
		HookConfigCache: hookConfigCache,
		ProviderReg:     provReg,
		HealthTracker:   healthTracker,
		AuditWriter:     audit.NewWriter(nil, "nexus.event.ai-traffic", nil, logger),
		Metrics:         nil, // no metrics in test
		Logger:          logger,
	}
}

// okVKAuth is a VKAuthenticator stub that always authenticates a request
// to a fixed virtual key, used to drive the post-auth admission path in
// tests that exercise body validation / routing after auth passes.
type okVKAuth struct{}

func (okVKAuth) Authenticate(_ context.Context, _ *http.Request) (*vkauth.VKMeta, error) {
	return &vkauth.VKMeta{ID: "vk-test", Name: "test"}, nil
}

func TestProxyHandler_MissingModel(t *testing.T) {
	// auth runs BEFORE the body read, so authenticate with a
	// passing VKAuth stub to reach the body-level model validation.
	deps := testDeps("")
	deps.VKAuth = okVKAuth{}
	h := proxy.NewHandler(deps).ServeProxy(proxy.Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-nexus-virtual-key", "test-slug")
	rec := httptest.NewRecorder()

	h(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "model is required") {
		t.Errorf("expected model error, got %s", body)
	}
}

// TestProxyHandler_AuthBeforeBody asserts the ordering: an
// unauthenticated request is rejected at auth (401) BEFORE the body is read
// or model-validated, so an attacker cannot force a full-body read +
// payload capture pre-auth. A missing-model body that would 400 once
// authenticated must instead surface the 401.
func TestProxyHandler_AuthBeforeBody(t *testing.T) {
	deps := testDeps("")
	deps.VKAuth = stubAuthErr{err: vkauth.ErrMissing}
	h := proxy.NewHandler(deps).ServeProxy(proxy.Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})

	// Body has no model — would 400 on the model check if it ran first.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (auth before body read), got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "model is required") {
		t.Errorf("body-level model validation ran before auth — F-0048 regression: %s", rec.Body.String())
	}
}

// stubAuthErr is a VKAuthenticator stub that always fails authentication.
type stubAuthErr struct{ err error }

func (s stubAuthErr) Authenticate(_ context.Context, _ *http.Request) (*vkauth.VKMeta, error) {
	return nil, s.err
}

func TestModelsHandler_EmptyList(t *testing.T) {
	// ModelsHandler with nil ModelLookup will error, but we can test the error path.
	h := models.ModelsHandler(nil, nil, slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	// Without DB, should return 500.
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 without DB, got %d", rec.Code)
	}
}
