// stage_routing_passthrough.go — the requested-model passthrough: what the
// gateway dispatches when no routing rule matched the model the caller named.
//
// Split from stage_routing.go, which owns the rule-driven stage. The two ask
// different questions and fail differently — a rule-path fault means routing
// misresolved, a passthrough fault means the model itself cannot serve the
// request — and the guards below are the ones the resolver applies on its own
// path, kept in step by design rather than by memory.
package proxy

import (
	"context"
	"net/http"
	"slices"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// canonicalFn defers materialising the request canonical. It is a func rather
// than a payload because the guard below needs it for roughly one request in a
// hundred, and calling it eagerly would undo the lazy-canonical win.
type canonicalFn func() *normcore.NormalizedPayload

// deferredRequest carries the request views the passthrough guards need, each
// behind a call so nothing is computed for the requests that skip the guard.
//
// Bundled rather than passed as parallel arguments: the guards are added one
// at a time and a signature that grows a parameter per guard invites a call
// site that supplies one and forgets the next — which is how the two guards
// being repaired here came to exist on one selection path and not the other.
type deferredRequest struct {
	canonical canonicalFn
	rawBody   func() []byte
}

func (h *Handler) resolveNoMatchPassthrough(ctx context.Context, requestedModel string, vkMeta *vkauth.VKMeta, in Ingress, endpointKind typology.EndpointKind, req deferredRequest) (*routingcore.RouteResult, error) {
	if h.deps == nil || h.deps.Models == nil {
		return nil, &routingFallbackError{
			status:  http.StatusInternalServerError,
			code:    "ROUTING_NO_MATCH",
			message: "passthrough fallback is unavailable",
			hint:    "Model lookup dependency is not configured",
		}
	}

	// Resolve by code OR alias: a client may address a model by an
	// admin-configured alias with no routing rule, which must still route to
	// the model (the model's ProviderModelID then reaches the wire, and the
	// passthrough body rewrite swaps the alias for it). O(1) from the
	// in-memory index — no per-request DB read.
	model, err := h.deps.Models.GetModelByCodeOrAlias(ctx, requestedModel)
	// An unbuilt catalog index and an absent model are opposite facts and must
	// not share a status. "This model does not exist" is a permanent client
	// error that tells an SDK to stop; "the catalog has not loaded" is ours and
	// clears on its own. Answering the second with the first is what made six
	// enabled models look deleted for 34 minutes on staging (2026-08-11).
	if cachelayer.IsIndexUnavailable(err) {
		return nil, &routingFallbackError{
			status:  http.StatusServiceUnavailable,
			code:    "MODEL_CATALOG_UNAVAILABLE",
			message: "model catalog is not loaded yet; the request was not routed",
			hint:    "Transient: retry shortly. The model was never looked up, so this says nothing about whether it exists",
		}
	}
	if err != nil || model == nil {
		return nil, &routingFallbackError{
			status:  http.StatusNotFound,
			code:    "ROUTING_NO_MATCH",
			message: "no available provider for model " + requestedModel,
			hint:    "Ensure the model exists and is enabled",
		}
	}

	// This function is the dispatch constructor for the no-rule path: whatever
	// it returns goes on a vendor's wire without a second opinion. The lookup
	// above is expected to have withdrawn unservable models already, but that
	// is a property of how the index was built, not of the ModelLookup
	// interface — so assert it here rather than trusting the caller's wiring.
	// The rule-driven path asserts the same thing in resolver.lookupTarget.
	if !model.Servable() {
		return nil, &routingFallbackError{
			status:  http.StatusNotFound,
			code:    "ROUTING_NO_MATCH",
			message: "no available provider for model " + requestedModel,
			hint:    "The model or the provider serving it is disabled; re-enable it or route to another model",
		}
	}

	if vkMeta != nil && len(vkMeta.AllowedModels) > 0 &&
		!routingcore.ModelMatchesAllowedRefs(model.ID, model.ProviderModelID, model.ProviderID, vkMeta.AllowedModels) {
		return nil, &routingFallbackError{
			status:  http.StatusForbidden,
			code:    "MODEL_NOT_ALLOWED",
			message: "model " + requestedModel + " is not allowed for this virtual key",
			hint:    "Use an allowed model or request policy update",
		}
	}

	// Modality guard for the requested-model passthrough: reject a model whose
	// modality does not match the endpoint (e.g. an image model addressed on
	// /v1/chat/completions) with a clean 400 rather than forwarding it upstream
	// to fail. The rule-based paths get the same guard via the resolver's
	// filterByModality; this covers the explicit-model path the resolver never
	// sees.
	if !typology.EndpointKindAcceptsModelType(endpointKind, model.Type) {
		return nil, &routingFallbackError{
			status:  http.StatusBadRequest,
			code:    "MODEL_MODALITY_MISMATCH",
			message: "model " + requestedModel + " (" + model.Type + ") cannot serve a " + string(endpointKind) + " request",
			hint:    "Use a model whose modality matches this endpoint",
		}
	}

	// The floor + input-modality ceiling are the gateway's OWN modality verdict
	// for a model the caller NAMED (this path runs only for an unmatched
	// explicit model — `auto` with no rule is rejected before here). By default
	// that verdict is deferred to the upstream, because it reads from a
	// catalogue that has been wrong; EnforceNamedModelModality enforces it here
	// instead. See namedModelModalityGuard.
	if err := h.namedModelModalityGuard(model.ID, requestedModel, req.canonical); err != nil {
		return nil, err
	}

	// The embeddings capability pre-filter, the second guard the resolver runs
	// and this path did not. Same snapshot, same comparison, other path. NOT
	// governed by EnforceNamedModelModality: it checks parameter compatibility
	// (dimensions, encoding format, input_type), not modality, so a caller
	// naming an embeddings model still gets our named 400 rather than the
	// provider's — the "our catalogue may be wrong about modality" argument
	// does not apply to a dimensions value the model demonstrably cannot emit.
	if err := h.embeddingCapabilityGuard(model.ID, requestedModel, endpointKind, req.rawBody); err != nil {
		return nil, err
	}

	providerName := model.ProviderName
	if providerName == "" {
		providerName = model.ProviderID
	}
	// Use the provider's actual wire adapter type so the normaliser
	// (L3/L4) and cache-key preparation use the correct format.
	// Falls back to the ingress format when adapter_type is not
	// stored (legacy rows or test doubles).
	adapterType := model.ProviderAdapterType
	if adapterType == "" {
		adapterType = string(in.BodyFormat)
	}
	maxOut := 0
	if model.MaxOutputTokens != nil {
		maxOut = *model.MaxOutputTokens
	}
	target := routingcore.RoutingTarget{
		ProviderID:      model.ProviderID,
		ProviderName:    providerName,
		AdapterType:     adapterType,
		ModelID:         model.ID,
		ModelCode:       model.Code,
		ModelName:       model.Name,
		ModelType:       model.Type,
		ProviderModelID: model.ProviderModelID,
		BaseURL:         model.ProviderBaseURL,
		Reasons:         slices.Contains(model.Features, routingcore.FeatureReasoning),
		MaxOutputTokens: maxOut,
		Source:          "passthrough-fallback",
	}
	return &routingcore.RouteResult{
		// Passthrough is one target and no chain: the caller named a model
		// and nothing was chosen on their behalf, so there is nothing an admin
		// wrote down to fall back to.
		Dispatch: []routingcore.RoutingTarget{target},
		RuleID:   "passthrough-fallback",
		RuleName: "passthrough-fallback",
		// The client requested this specific model and no routing rule
		// substituted it — passthrough sends straight to it — so the requested
		// side IS this model. Without this, the common default deployment
		// (only smart-auto-routing enabled, so specific-model requests fall to
		// passthrough) would leave the requested columns NULL.
		RequestedModelID:      model.ID,
		RequestedProviderID:   model.ProviderID,
		RequestedProviderName: providerName,
	}, nil
}
