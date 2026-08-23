package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
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

func (s fallbackModelLookupStub) GetModelByCodeOrAlias(ctx context.Context, key string) (*store.Model, error) {
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

// TestResolveNoMatchPassthrough_RejectsUnservableModel pins the dispatch
// guard: this function is what puts bytes on a vendor's wire when no routing
// rule matched, so it must refuse a model that is out of service even when the
// lookup handed it over. Each of the three switches an operator can flip is
// checked independently — a guard that only reads one of them lets the other
// two through, which is the bug that let a disabled provider's models reach
// upstream and fail there instead of being declined here.
func TestResolveNoMatchPassthrough_RejectsUnservableModel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model store.Model
	}{
		{"model disabled", store.Model{Enabled: false, ProviderEnabled: true, Status: "active"}},
		{"provider disabled", store.Model{Enabled: true, ProviderEnabled: false, Status: "active"}},
		{"status disabled", store.Model{Enabled: true, ProviderEnabled: true, Status: "disabled"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.model
			m.ID, m.Code, m.Type = "m-out", "embed-english-v3.0", "chat"
			m.ProviderID, m.ProviderModelID = "p-cohere", "embed-english-v3.0"
			h := &Handler{deps: &Deps{Models: fallbackModelLookupStub{model: &m}}}

			_, err := h.resolveNoMatchPassthrough(context.Background(), "embed-english-v3.0",
				&vkauth.VKMeta{}, Ingress{BodyFormat: provcore.FormatOpenAI}, typology.EndpointKindChat, deferredRequest{})
			var rfe *routingFallbackError
			if !errors.As(err, &rfe) {
				t.Fatalf("an unservable model must not be dispatched; got %T: %v", err, err)
			}
			if rfe.status != http.StatusNotFound || rfe.code != "ROUTING_NO_MATCH" {
				t.Fatalf("got status=%d code=%q, want 404 ROUTING_NO_MATCH", rfe.status, rfe.code)
			}
		})
	}
}

func TestResolveNoMatchPassthrough_RejectsCrossModality(t *testing.T) {
	display := "openai"
	h := &Handler{deps: &Deps{
		Models: fallbackModelLookupStub{
			model: &store.Model{
				Enabled: true, ProviderEnabled: true, Status: "active",
				ID:                  "img-1",
				Name:                "gpt-image-1",
				Type:                "image",
				ProviderID:          "provider-1",
				ProviderDisplayName: &display,
				ProviderModelID:     "gpt-image-1",
			},
		},
	}}

	// An image model addressed on a chat endpoint must be rejected with a
	// modality-mismatch error rather than forwarded upstream to fail.
	_, err := h.resolveNoMatchPassthrough(context.Background(), "gpt-image-1", &vkauth.VKMeta{},
		Ingress{BodyFormat: provcore.FormatOpenAI}, typology.EndpointKindChat, deferredRequest{})
	if err == nil {
		t.Fatal("expected a modality-mismatch error, got nil")
	}
	var rfe *routingFallbackError
	if !errors.As(err, &rfe) {
		t.Fatalf("expected *routingFallbackError, got %T: %v", err, err)
	}
	if rfe.status != http.StatusBadRequest || rfe.code != "MODEL_MODALITY_MISMATCH" {
		t.Fatalf("got status=%d code=%q, want 400 MODEL_MODALITY_MISMATCH", rfe.status, rfe.code)
	}
}

func TestResolveNoMatchPassthrough_SucceedsForAllowedModel(t *testing.T) {
	display := "deepseek"
	h := &Handler{deps: &Deps{
		Models: fallbackModelLookupStub{
			model: &store.Model{
				Enabled: true, ProviderEnabled: true, Status: "active",
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
	}, Ingress{BodyFormat: provcore.FormatOpenAI}, typology.EndpointKindChat, deferredRequest{})
	if err != nil {
		t.Fatalf("resolveNoMatchPassthrough returned error: %v", err)
	}
	if len(got.AllTargets()) != 1 {
		t.Fatalf("targets len = %d, want 1", len(got.AllTargets()))
	}
	target := got.AllTargets()[0]
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
				Enabled: true, ProviderEnabled: true, Status: "active",
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
	}, Ingress{BodyFormat: provcore.FormatOpenAI}, typology.EndpointKindChat, deferredRequest{})
	if err != nil {
		t.Fatalf("resolveNoMatchPassthrough returned error: %v", err)
	}
	target := got.AllTargets()[0]
	if target.AdapterType != "anthropic" {
		t.Fatalf("target.AdapterType = %q, want %q (provider adapter type wins over ingress format)", target.AdapterType, "anthropic")
	}
}

// The passthrough fallback is the default deployment's busiest routing
// path (only smart-auto-routing enabled → every specific-model request
// lands here), so the catalog's output ceiling must reach its target too.
// Carrying it on the snapshot is what lets the cache stage hand the codec
// a real cap instead of 0.
func TestResolveNoMatchPassthrough_CarriesMaxOutputTokens(t *testing.T) {
	display := "anthropic"
	maxOut := 128000
	h := &Handler{deps: &Deps{
		Models: fallbackModelLookupStub{
			model: &store.Model{
				Enabled: true, ProviderEnabled: true, Status: "active",
				ID:                  "model-opus",
				Name:                "claude-opus-4-7",
				ProviderID:          "provider-anthropic",
				ProviderAdapterType: "anthropic",
				ProviderDisplayName: &display,
				ProviderModelID:     "claude-opus-4-7",
				MaxOutputTokens:     &maxOut,
			},
		},
	}}

	got, err := h.resolveNoMatchPassthrough(context.Background(), "claude-opus-4-7", &vkauth.VKMeta{
		AllowedModels: []store.AllowedModelRef{{ProviderID: "provider-anthropic", ModelID: "claude-opus-4-7"}},
	}, Ingress{BodyFormat: provcore.FormatOpenAI}, typology.EndpointKindChat, deferredRequest{})
	if err != nil {
		t.Fatalf("resolveNoMatchPassthrough returned error: %v", err)
	}
	if got := got.AllTargets()[0].MaxOutputTokens; got != 128000 {
		t.Fatalf("target.MaxOutputTokens = %d, want the catalog's 128000", got)
	}
}

// A NULL maxOutputTokens column must degrade to 0 (the codec's "no ceiling
// known" signal), not panic on the nil deref.
func TestResolveNoMatchPassthrough_NullMaxOutputTokens_IsZero(t *testing.T) {
	display := "anthropic"
	h := &Handler{deps: &Deps{
		Models: fallbackModelLookupStub{
			model: &store.Model{
				Enabled: true, ProviderEnabled: true, Status: "active",
				ID:                  "model-nocap",
				Name:                "claude-nocap",
				ProviderID:          "provider-anthropic",
				ProviderAdapterType: "anthropic",
				ProviderDisplayName: &display,
				ProviderModelID:     "claude-nocap",
			},
		},
	}}

	got, err := h.resolveNoMatchPassthrough(context.Background(), "claude-nocap", &vkauth.VKMeta{
		AllowedModels: []store.AllowedModelRef{{ProviderID: "provider-anthropic", ModelID: "claude-nocap"}},
	}, Ingress{BodyFormat: provcore.FormatOpenAI}, typology.EndpointKindChat, deferredRequest{})
	if err != nil {
		t.Fatalf("resolveNoMatchPassthrough returned error: %v", err)
	}
	if got := got.AllTargets()[0].MaxOutputTokens; got != 0 {
		t.Fatalf("target.MaxOutputTokens = %d, want 0 for a NULL catalog column", got)
	}
}

func TestResolveNoMatchPassthrough_RejectsUnauthorizedModel(t *testing.T) {
	h := &Handler{deps: &Deps{
		Models: fallbackModelLookupStub{
			model: &store.Model{
				Enabled: true, ProviderEnabled: true, Status: "active",
				ID:              "model-1",
				Name:            "deepseek-chat",
				ProviderID:      "provider-1",
				ProviderModelID: "deepseek-chat",
			},
		},
	}}

	_, err := h.resolveNoMatchPassthrough(context.Background(), "deepseek-chat", &vkauth.VKMeta{
		AllowedModels: []store.AllowedModelRef{{ProviderID: "provider-2", ModelID: "gpt-4o"}},
	}, Ingress{BodyFormat: provcore.FormatOpenAI}, typology.EndpointKindChat, deferredRequest{})
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

	_, err := h.resolveNoMatchPassthrough(context.Background(), "deepseek-chat", &vkauth.VKMeta{}, Ingress{BodyFormat: provcore.FormatOpenAI}, typology.EndpointKindChat, deferredRequest{})
	var routingErr *routingFallbackError
	if !errors.As(err, &routingErr) {
		t.Fatalf("expected routingFallbackError, got %T (%v)", err, err)
	}
	if routingErr.status != http.StatusNotFound || routingErr.code != "ROUTING_NO_MATCH" {
		t.Fatalf("got (%d,%q), want (%d,%q)", routingErr.status, routingErr.code, http.StatusNotFound, "ROUTING_NO_MATCH")
	}
}

// resolveOrPassthroughRouter scripts one RouteResult for the second routing
// entry point — the one the STT / video / realtime handlers use.
type resolveOrPassthroughRouter struct{ result *routingcore.RouteResult }

func (s *resolveOrPassthroughRouter) ResolveTargets(_ context.Context, _ *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return s.result, nil
}

// TestResolveRouteOrPassthrough_HonoursThePassthroughPrecondition.
//
// There are TWO entry points into routing. ServeProxy's stage got the
// precondition — passthrough only when no rule applied — and this one, which
// serves STT, video and realtime, did not. So a compliance rule redirecting
// audio elsewhere, whose targets were unavailable, was silently undone on
// exactly the endpoints nobody was watching.
func TestResolveRouteOrPassthrough_HonoursThePassthroughPrecondition(t *testing.T) {
	deps := &Deps{
		Router: &resolveOrPassthroughRouter{result: &routingcore.RouteResult{
			Dispatch: nil, RuleMatchedAndResolvedNothing: true,
		}},
		Models: fallbackModelLookupStub{model: &store.Model{
			ID: "m-whisper", Code: "whisper-1", Name: "Whisper",
			Enabled: true, ProviderEnabled: true, Status: "active",
			ProviderID: "p-openai", ProviderModelID: "whisper-1", Type: "stt",
		}},
	}
	h := NewHandler(deps)
	rctxFull := requestcontext.NewBuilder().WithEndpoint(string(typology.EndpointKindSTT)).Build()

	_, err := h.resolveRouteOrPassthrough(context.Background(), rctxFull,
		Ingress{BodyFormat: provcore.FormatOpenAI}, "whisper-1", typology.EndpointKindSTT)

	var rfe *routingFallbackError
	if !errors.As(err, &rfe) {
		t.Fatalf("a request every matching rule failed to resolve was passed through to the "+
			"model the caller named; got %T (%v)", err, err)
	}
	if rfe.code != "ROUTING_RULES_RESOLVED_NOTHING" || rfe.status != http.StatusServiceUnavailable {
		t.Fatalf("got (%d,%q), want (503,ROUTING_RULES_RESOLVED_NOTHING)", rfe.status, rfe.code)
	}
}

// The negative half: no rule matched, so this endpoint still passes through.
// Without it the fix above reads as "refuse whenever routing is empty", which
// would take STT, video and realtime down on every default deployment.
func TestResolveRouteOrPassthrough_StillPassesThroughWhenNoRuleMatched(t *testing.T) {
	deps := &Deps{
		Router: &resolveOrPassthroughRouter{result: &routingcore.RouteResult{Dispatch: nil}},
		Models: fallbackModelLookupStub{model: &store.Model{
			ID: "m-whisper", Code: "whisper-1", Name: "Whisper",
			Enabled: true, ProviderEnabled: true, Status: "active",
			ProviderID: "p-openai", ProviderModelID: "whisper-1", Type: "stt",
		}},
	}
	h := NewHandler(deps)
	rctxFull := requestcontext.NewBuilder().WithEndpoint(string(typology.EndpointKindSTT)).Build()

	got, err := h.resolveRouteOrPassthrough(context.Background(), rctxFull,
		Ingress{BodyFormat: provcore.FormatOpenAI}, "whisper-1", typology.EndpointKindSTT)
	if err != nil {
		t.Fatalf("no rule matched, so the requested model is the right answer: %v", err)
	}
	if len(got.AllTargets()) != 1 || got.AllTargets()[0].ModelID != "m-whisper" {
		t.Fatalf("passthrough did not install the requested model: %+v", got)
	}
}

// TestResolveNoMatchPassthrough_CatalogNotLoadedIsNot404 separates the two
// facts that used to share a status code.
//
// The production shape (staging, 2026-08-11): for 34 minutes every request for
// claude-sonnet-4-6, claude-opus-4-6, claude-haiku-4-5-20251001, gpt-4o,
// text-embedding-3-large and gemini-embedding-001 came back
// "404 ROUTING_NO_MATCH / no available provider for model X / Ensure the model
// exists and is enabled". All six existed, were enabled, and sat on enabled
// providers the whole time — the catalog index simply had not loaded. A 404 is
// the one answer an SDK treats as final, so the gateway spent half an hour
// telling clients that models it was about to serve did not exist.
func TestResolveNoMatchPassthrough_CatalogNotLoadedIsNot404(t *testing.T) {
	h := &Handler{deps: &Deps{Models: fallbackModelLookupStub{
		err: fmt.Errorf("cachelayer: model code/alias %q: %w", "claude-sonnet-4-6", cachelayer.ErrIndexUnavailable),
	}}}

	_, err := h.resolveNoMatchPassthrough(context.Background(), "claude-sonnet-4-6",
		&vkauth.VKMeta{}, Ingress{BodyFormat: provcore.FormatOpenAI}, typology.EndpointKindChat, deferredRequest{})

	var rfe *routingFallbackError
	if !errors.As(err, &rfe) {
		t.Fatalf("expected a routing fallback error; got %T: %v", err, err)
	}
	if rfe.status == http.StatusNotFound {
		t.Fatal("an unloaded catalog must not be reported as a missing model — 404 tells the client to stop retrying a condition that clears on its own")
	}
	if rfe.status != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 (transient, retryable)", rfe.status)
	}
	if rfe.code != "MODEL_CATALOG_UNAVAILABLE" {
		t.Fatalf("code=%q, want MODEL_CATALOG_UNAVAILABLE", rfe.code)
	}
	if strings.Contains(rfe.message, "no available provider") {
		t.Errorf("message must not claim the model is unavailable; got %q", rfe.message)
	}
}

// TestResolveNoMatchPassthrough_GenuineMissStays404 — the other half. Once the
// catalog IS loaded, a model that is not in it really is absent and must keep
// returning the permanent 404; the new branch must not swallow that.
func TestResolveNoMatchPassthrough_GenuineMissStays404(t *testing.T) {
	h := &Handler{deps: &Deps{Models: fallbackModelLookupStub{
		err: fmt.Errorf("cachelayer: model code/alias %q: %w", "nope", pgx.ErrNoRows),
	}}}

	_, err := h.resolveNoMatchPassthrough(context.Background(), "nope",
		&vkauth.VKMeta{}, Ingress{BodyFormat: provcore.FormatOpenAI}, typology.EndpointKindChat, deferredRequest{})

	var rfe *routingFallbackError
	if !errors.As(err, &rfe) {
		t.Fatalf("expected a routing fallback error; got %T: %v", err, err)
	}
	if rfe.status != http.StatusNotFound || rfe.code != "ROUTING_NO_MATCH" {
		t.Fatalf("got status=%d code=%q, want 404 ROUTING_NO_MATCH", rfe.status, rfe.code)
	}
}
