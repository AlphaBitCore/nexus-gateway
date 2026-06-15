package routing

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// hydrateRequestedModel resolves the request `model` string against the
// catalog (Model.code exact + Model.aliases membership) and stores every
// matching Model.id on rctx.RequestedModel.CandidateIDs. Provider/Type/
// ProviderModelID/ProviderName are filled from the first candidate when empty
// so matchConditions.providers / modelTypes have something to match on
// (single-provider catalog, today's common case) and the requested-side
// audit columns can be stamped. The "auto" sentinel is intentionally left
// without candidates so matchConditions.models cannot accidentally route auto
// requests through a UUID rule — those must be authored with
// matchConditions.requestedModelLiterals.
func (r *Resolver) hydrateRequestedModel(ctx context.Context, rctx *core.RoutingContext) {
	if rctx == nil {
		return
	}
	if rctx.RequestedModel.ID == "" || rctx.RequestedModel.ID == "auto" {
		return
	}
	candidates, err := r.db.ResolveModelCandidates(ctx, rctx.RequestedModel.ID)
	if err != nil {
		r.logger.Debug("router: resolve model candidates", "model", rctx.RequestedModel.ID, "error", err)
		return
	}
	if len(candidates) == 0 {
		return
	}
	rctx.RequestedModel.CandidateIDs = make([]string, 0, len(candidates))
	for _, m := range candidates {
		rctx.RequestedModel.CandidateIDs = append(rctx.RequestedModel.CandidateIDs, m.ID)
	}
	first := candidates[0]
	if rctx.RequestedModel.ProviderID == "" {
		rctx.RequestedModel.ProviderID = first.ProviderID
	}
	if rctx.RequestedModel.ProviderName == "" {
		rctx.RequestedModel.ProviderName = first.ProviderName
	}
	if rctx.RequestedModel.Type == "" {
		rctx.RequestedModel.Type = first.Type
	}
	if rctx.RequestedModel.ProviderModelID == "" {
		rctx.RequestedModel.ProviderModelID = first.ProviderModelID
	}
}

// requestedIdentity returns the traffic_event REQUESTED-side identity
// (model_id / provider_id / provider_name) for a hydrated RequestedModel. It is
// populated only when the client asked for a SPECIFIC model that resolved
// unambiguously to exactly one catalog model — "auto" (no candidates) and
// multi-provider codes (candidate order is non-deterministic and narrowing is a
// routing concern) yield empties so the requested columns stay NULL rather than
// guessing. The routed_* columns always carry the actually-served target.
func requestedIdentity(rm core.RequestedModel) (modelID, providerID, providerName string) {
	if rm.ID != "auto" && len(rm.CandidateIDs) == 1 {
		return rm.CandidateIDs[0], rm.ProviderID, rm.ProviderName
	}
	return "", "", ""
}
