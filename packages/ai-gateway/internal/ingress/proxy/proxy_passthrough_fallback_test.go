package proxy

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

type fallbackModelLookupStub struct {
	model *store.Model
	err   error
}

func (s fallbackModelLookupStub) GetModel(ctx context.Context, id string) (*store.Model, error) {
	return nil, errors.New("not used")
}

func (s fallbackModelLookupStub) GetModelByCode(ctx context.Context, idOrName string) (*store.Model, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.model, nil
}

func (s fallbackModelLookupStub) ListEnabledModels(ctx context.Context) ([]store.Model, error) {
	return nil, errors.New("not used")
}

func (s fallbackModelLookupStub) FetchModelPricing(ctx context.Context, modelIDs []string) ([]store.ModelPricing, error) {
	return nil, errors.New("not used")
}

func TestResolveNoMatchPassthrough_SucceedsForAllowedModel(t *testing.T) {
	display := "deepseek"
	h := &Handler{deps: &Deps{
		Models: fallbackModelLookupStub{
			model: &store.Model{
				ID:                  "model-1",
				Name:                "deepseek-chat",
				ProviderID:          "provider-1",
				ProviderDisplayName: &display,
				ProviderModelID:     "deepseek-chat",
			},
		},
	}}

	got, err := h.resolveNoMatchPassthrough(context.Background(), "deepseek-chat", &vkauth.VKMeta{
		AllowedModels: []store.AllowedModelRef{{ProviderID: "provider-1", ModelID: "deepseek-chat"}},
	}, Ingress{BodyFormat: provcore.FormatOpenAI})
	if err != nil {
		t.Fatalf("resolveNoMatchPassthrough returned error: %v", err)
	}
	if len(got.Targets) != 1 {
		t.Fatalf("targets len = %d, want 1", len(got.Targets))
	}
	target := got.Targets[0]
	if target.ModelID != "model-1" {
		t.Fatalf("target.ModelID = %q, want model-1", target.ModelID)
	}
	if target.AdapterType != string(provcore.FormatOpenAI) {
		t.Fatalf("target.AdapterType = %q, want %q", target.AdapterType, provcore.FormatOpenAI)
	}
	if got.RuleID != "passthrough-fallback" || got.RuleName != "passthrough-fallback" {
		t.Fatalf("rule metadata = (%q,%q), want passthrough-fallback", got.RuleID, got.RuleName)
	}
	// Requested side: the client pinned this specific model and passthrough
	// sends straight to it, so the REQUESTED columns must carry it (not stay
	// NULL — passthrough is the default-config path for specific-model requests).
	if got.RequestedModelID != "model-1" || got.RequestedProviderID != "provider-1" || got.RequestedProviderName != "provider-1" {
		t.Fatalf("requested side = %q/%q/%q, want model-1/provider-1/provider-1",
			got.RequestedModelID, got.RequestedProviderID, got.RequestedProviderName)
	}
}

func TestResolveNoMatchPassthrough_UsesProviderAdapterType(t *testing.T) {
	display := "anthropic"
	h := &Handler{deps: &Deps{
		Models: fallbackModelLookupStub{
			model: &store.Model{
				ID:                  "model-claude",
				Name:                "claude-haiku-4-5-20251001",
				ProviderID:          "provider-anthropic",
				ProviderAdapterType: "anthropic",
				ProviderDisplayName: &display,
				ProviderModelID:     "claude-haiku-4-5-20251001",
			},
		},
	}}

	got, err := h.resolveNoMatchPassthrough(context.Background(), "claude-haiku-4-5-20251001", &vkauth.VKMeta{
		AllowedModels: []store.AllowedModelRef{{ProviderID: "provider-anthropic", ModelID: "claude-haiku-4-5-20251001"}},
	}, Ingress{BodyFormat: provcore.FormatOpenAI})
	if err != nil {
		t.Fatalf("resolveNoMatchPassthrough returned error: %v", err)
	}
	target := got.Targets[0]
	if target.AdapterType != "anthropic" {
		t.Fatalf("target.AdapterType = %q, want %q (provider adapter type wins over ingress format)", target.AdapterType, "anthropic")
	}
}

func TestResolveNoMatchPassthrough_RejectsUnauthorizedModel(t *testing.T) {
	h := &Handler{deps: &Deps{
		Models: fallbackModelLookupStub{
			model: &store.Model{
				ID:              "model-1",
				Name:            "deepseek-chat",
				ProviderID:      "provider-1",
				ProviderModelID: "deepseek-chat",
			},
		},
	}}

	_, err := h.resolveNoMatchPassthrough(context.Background(), "deepseek-chat", &vkauth.VKMeta{
		AllowedModels: []store.AllowedModelRef{{ProviderID: "provider-2", ModelID: "gpt-4o"}},
	}, Ingress{BodyFormat: provcore.FormatOpenAI})
	var routingErr *routingFallbackError
	if !errors.As(err, &routingErr) {
		t.Fatalf("expected routingFallbackError, got %T (%v)", err, err)
	}
	if routingErr.status != http.StatusForbidden || routingErr.code != "MODEL_NOT_ALLOWED" {
		t.Fatalf("got (%d,%q), want (%d,%q)", routingErr.status, routingErr.code, http.StatusForbidden, "MODEL_NOT_ALLOWED")
	}
}

func TestResolveNoMatchPassthrough_ReturnsNotFoundWhenModelMissing(t *testing.T) {
	h := &Handler{deps: &Deps{
		Models: fallbackModelLookupStub{err: errors.New("store: get model by id or name: no rows")},
	}}

	_, err := h.resolveNoMatchPassthrough(context.Background(), "deepseek-chat", &vkauth.VKMeta{}, Ingress{BodyFormat: provcore.FormatOpenAI})
	var routingErr *routingFallbackError
	if !errors.As(err, &routingErr) {
		t.Fatalf("expected routingFallbackError, got %T (%v)", err, err)
	}
	if routingErr.status != http.StatusNotFound || routingErr.code != "ROUTING_NO_MATCH" {
		t.Fatalf("got (%d,%q), want (%d,%q)", routingErr.status, routingErr.code, http.StatusNotFound, "ROUTING_NO_MATCH")
	}
}
