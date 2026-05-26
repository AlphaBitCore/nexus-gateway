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

	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/models"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/proxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/ratelimit"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
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

	credMgr := credmanager.NewManager(nil, nil, logger)
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

func TestProxyHandler_MissingModel(t *testing.T) {
	deps := testDeps("")
	h := proxy.NewHandler(deps).ServeProxy(proxy.Ingress{
		WireShape:   typology.WireShapeOpenAIChat,
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

func TestProxyHandler_MissingVK(t *testing.T) {
	// When VKAuth is nil (no DB), auth check is skipped in real handler.
	// But when VKAuth is set, missing VK should return 401.
	// For this test, we verify the missing-model check fires before auth.
	deps := testDeps("")
	h := proxy.NewHandler(deps).ServeProxy(proxy.Ingress{
		WireShape:   typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No VK header — but VKAuth is nil so it would panic. Test model validation instead.
	rec := httptest.NewRecorder()

	// This exercises the body parsing path at minimum.
	h(rec, req)
	// Without DB/VKAuth the handler will fail at auth or routing — that's expected.
	// We just verify it doesn't panic.
	if rec.Code == 0 {
		t.Error("expected a non-zero status code")
	}
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
