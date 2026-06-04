package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/ratelimit"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

type embeddingsStubVK struct{}

func (embeddingsStubVK) Authenticate(ctx context.Context, r *http.Request) (*vkauth.VKMeta, error) {
	return &vkauth.VKMeta{
		ID:             "vk-test",
		Name:           "test-slug",
		OrganizationID: "org-test",
	}, nil
}

type embeddingsFixedRouter struct {
	targets []routingcore.RoutingTarget
}

func (e embeddingsFixedRouter) ResolveTargets(ctx context.Context, rctx *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return &routingcore.RouteResult{
		Targets: e.targets,
		RuleID:  "rule-test",
	}, nil
}

type embeddingsStubCred struct{}

func (embeddingsStubCred) GetForProvider(ctx context.Context, providerID string) (apiKey, credentialID, credentialName string, err error) {
	return "sk-test", "cred", "n", nil
}

type embeddingsStubProvResolve struct{}

func (embeddingsStubProvResolve) Resolve(ctx context.Context, providerID, modelID string, hints provtarget.ResolveHints) (provcore.CallTarget, error) {
	name := "anthropic"
	format := provcore.FormatAnthropic
	if providerID == "p-gem" {
		name = "gemini"
		format = provcore.FormatGemini
	}
	return provcore.CallTarget{
		ProviderID:      providerID,
		ProviderName:    name,
		Format:          format,
		BaseURL:         "http://127.0.0.1:9",
		ProviderModelID: modelID,
	}, nil
}

// OpenAI-shaped /v1/embeddings with routing that only exposes an
// out-of-scope target (Anthropic — no embedding canonical helper)
// must fail at the cross-format gate (400 no_compatible_provider).
// Cross-format routing is allowed for OpenAI / Azure / Cohere / Gemini
// / Vertex; this regression pin protects the boundary at the in-scope
// edge.
func TestProxy_Embeddings_OpenAIIngress_GeminiOnlyTarget_NoCompatibleProvider(t *testing.T) {
	logger := slog.Default()
	hookCache := compliance.NewHookConfigCache(
		func(ctx context.Context) ([]goHooks.HookConfig, error) { return nil, nil },
		builtins.Registry,
		0,
		logger,
	)
	provReg := provcore.NewRegistry()
	provbuiltins.Register(provReg, nil, logger)
	bridge := canonicalbridge.New(provbuiltins.SchemaCodecs(logger))
	exec := executor.New(provReg, embeddingsStubProvResolve{}, store.NewHealthTracker(), bridge)

	deps := &Deps{
		Models:      nil,
		VKAuth:      embeddingsStubVK{},
		RateLimiter: ratelimit.NewLocalOnly(logger),
		CredManager: embeddingsStubCred{},
		Router: embeddingsFixedRouter{
			targets: []routingcore.RoutingTarget{
				// Anthropic is intentionally not in the embedding
				// in-scope list — it has no embedding canonical helper,
				// so EmbeddingsRoutable rejects the pair and the proxy
				// emits 400 no_compatible_provider at the cross-format
				// gate (pre-codec).
				{ProviderID: "p-anth", ProviderName: "anthropic", AdapterType: "anthropic", ModelID: "voyage-embed-stub"},
			},
		},
		Executor:        exec,
		HookConfigCache: hookCache,
		ProviderReg:     provReg,
		HealthTracker:   store.NewHealthTracker(),
		AuditWriter:     audit.NewWriter(nil, "nexus.event.ai-traffic", nil, logger),
		CanonicalBridge: bridge,
		Logger:          logger,
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIEmbeddings,
		BodyFormat: provcore.FormatOpenAI,
	})

	body := `{"model":"text-embedding-004","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-nexus-virtual-key", "dummy")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj["type"] != "no_compatible_provider" {
		t.Fatalf("error.type = %v, want no_compatible_provider: %s", errObj["type"], rec.Body.String())
	}
}
