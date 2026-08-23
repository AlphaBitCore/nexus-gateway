package routing

import (
	"context"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// hydrateRequestedModel resolves the request `model` string against the catalog
// (Model.code exact + Model.aliases membership) and stores every matching
// Model.id on rctx.RequestedModel.CandidateIDs, with the distinct providers
// serving them on CandidateProviderIDs.
//
// The provider fields are filled ONLY when every candidate sits on ONE
// provider. A code two providers both serve — the same open-weights model on
// two hosts is the ordinary case — has no single provider, no single upstream
// name, and the catalogue query states no order, so filling them from the first
// row invents an answer that can differ between two identical requests. That is
// not hypothetical about routing: `matchConditions.providers` compared against
// exactly this field, so a rule scoped to one provider fired or did not
// according to row order. It now intersects CandidateProviderIDs, which is the
// same shape `matchConditions.models` already uses against CandidateIDs.
//
// Type is the exception, filled when every candidate agrees: a model code names
// one kind of thing whoever hosts it, and leaving it empty would take
// `requestedModel.type` expressions away from multi-provider codes for no gain.
//
// The "auto" sentinel is intentionally left without candidates so
// matchConditions.models cannot accidentally route auto requests through a UUID
// rule — those must be authored with matchConditions.requestedModelLiterals.
func (r *Resolver) hydrateRequestedModel(ctx context.Context, rctx *core.RoutingContext) {
	if rctx == nil {
		return
	}
	if rctx.RequestedModel.ID == "" || rctx.RequestedModel.ID == "auto" {
		return
	}
	candidates, err := r.db.ResolveModelCandidates(ctx, rctx.RequestedModel.ID)
	if err != nil {
		// Recorded, not swallowed: downstream, empty candidate fields otherwise
		// read as "the caller named nothing", which conditions treat as
		// inapplicable — so a blip here silently widened every provider-scoped
		// rule to every provider.
		rctx.RequestedModel.HydrationFailed = true
		r.logger.Warn("router: resolve model candidates failed; provider conditions will refuse "+
			"rather than widen", "model", rctx.RequestedModel.ID, "error", err)
		return
	}
	if len(candidates) == 0 {
		return
	}
	rctx.RequestedModel.CandidateIDs = make([]string, 0, len(candidates))
	seenProvider := make(map[string]bool, len(candidates))
	sharedType := candidates[0].Type
	for _, m := range candidates {
		rctx.RequestedModel.CandidateIDs = append(rctx.RequestedModel.CandidateIDs, m.ID)
		if m.ProviderID != "" && !seenProvider[m.ProviderID] {
			seenProvider[m.ProviderID] = true
			rctx.RequestedModel.CandidateProviderIDs = append(rctx.RequestedModel.CandidateProviderIDs, m.ProviderID)
		}
		if m.Type != sharedType {
			sharedType = ""
		}
	}
	if rctx.RequestedModel.Type == "" {
		rctx.RequestedModel.Type = sharedType
	}
	// One provider is enough for the provider fields — two rows on one
	// provider (a code and an alias of it) still name that provider
	// unambiguously. The upstream name is per-MODEL, so it needs one row.
	if len(rctx.RequestedModel.CandidateProviderIDs) == 1 {
		if rctx.RequestedModel.ProviderID == "" {
			rctx.RequestedModel.ProviderID = candidates[0].ProviderID
		}
		if rctx.RequestedModel.ProviderName == "" {
			rctx.RequestedModel.ProviderName = candidates[0].ProviderName
		}
	}
	if len(candidates) == 1 && rctx.RequestedModel.ProviderModelID == "" {
		rctx.RequestedModel.ProviderModelID = candidates[0].ProviderModelID
	}
}

// requestedIdentity returns the traffic_event REQUESTED-side identity
// (model_id / provider_id / provider_name) for a hydrated RequestedModel. It is
// populated only when the client asked for a SPECIFIC model that resolved
// unambiguously to exactly one catalog model — "auto" (no candidates) and
// multi-provider codes (candidate order is non-deterministic and VK access is a
// routing concern) yield empties so the requested columns stay NULL rather than
// guessing. The routed_* columns always carry the actually-served target.
func requestedIdentity(rm core.RequestedModel) (modelID, providerID, providerName string) {
	if rm.ID != "auto" && len(rm.CandidateIDs) == 1 {
		return rm.CandidateIDs[0], rm.ProviderID, rm.ProviderName
	}
	return "", "", ""
}

// callerNamedTheModel reports whether the client asked for one specific model
// rather than delegating the choice.
//
// The gateway only overrides a target's eligibility when it made the choice
// itself. `auto`, an empty model, and a code fanning out to several rows all
// mean "you pick"; anything resolving to exactly one row is the caller's pick,
// and its limits are between them and the upstream.
//
// Same test requestedIdentity uses for the requested-side audit columns, so the
// audit row and the routing decision cannot disagree about who chose.
func callerNamedTheModel(rm core.RequestedModel) bool {
	return rm.ID != "" && rm.ID != "auto" && len(rm.CandidateIDs) == 1
}

// filterRecoveryByCapability applies the ceiling per modality with a
// catalog-silence escape, and the floor unconditionally.
//
// A dimension is enforced only when the CATALOG describes it. No chat row
// declares file or video today, so enforcing those deletes every recovery
// target on every document request — the catalog is silent, not the models.
//
// Asked of the snapshot, never of this list. The smart strategy asks the
// equivalent question by watching for a dimension that empties its pool, which
// works there because its pool IS the catalog. A recovery list is two or three
// admin-configured models, where "neither declares audio" says nothing about
// the catalog — treating it as silence hands the audio request straight back to
// the text-only model.
//
// The floor gets no escape: a model requiring audio cannot serve a text
// request, which is a property of the model, not a gap in the catalog.
func filterRecoveryByCapability(snap *capability.Snapshot, targets []core.RoutingTarget,
	carried []string, trace *[]core.TraceEntry) []core.RoutingTarget {
	kept := targets
	var skipped, blocking []string
	for _, m := range carried {
		// Text can never exclude a target — the ceiling predicate admits it
		// unconditionally — so walking it would add a no-op pass and then name
		// text in a trace line claiming the dropped targets could not serve it.
		if m == "text" {
			continue
		}
		if !snap.Describes(m) {
			skipped = append(skipped, m)
			continue
		}
		pass := make([]core.RoutingTarget, 0, len(kept))
		for _, t := range kept {
			if snap.AcceptsModality(t.ModelID, m) {
				pass = append(pass, t)
			}
		}
		if len(pass) < len(kept) {
			blocking = append(blocking, m)
		}
		kept = pass
	}
	floored := kept[:0]
	for _, t := range kept {
		if snap.MeetsFloor(t.ModelID, carried) {
			floored = append(floored, t)
		}
	}
	kept = floored

	// The trace names the modalities that actually excluded something, not the
	// whole carried set. An operator reading "cannot serve [text audio]" against
	// a target that reads text perfectly well is being told something false, and
	// the false half is the part they are most likely to act on.
	if dropped := len(targets) - len(kept); dropped > 0 {
		reason := fmt.Sprintf("%v", blocking)
		if len(blocking) == 0 {
			reason = "the required-modality floor"
		}
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "recovery",
			Decision: fmt.Sprintf(
				"capability filter: dropped %d recovery target(s) over %s",
				dropped, reason),
		})
	}
	if len(skipped) > 0 {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "recovery",
			Decision: fmt.Sprintf(
				"capability filter: the catalog describes no %v, so it was not enforced",
				skipped),
		})
	}
	return kept
}

// filterByFloor drops targets whose required-modality floor this request cannot
// meet. It is the CEILING's opposite and it travels further, because the two
// answer questions of different certainty.
//
// The ceiling ("does this model accept images") is a claim about the catalogue,
// which may be incomplete — so it has an escape hatch, and an explicit pick by
// an admin is honoured rather than second-guessed. The floor ("this model
// requires audio") is a claim about the MODEL: a text-only request sent to it
// cannot succeed, whoever chose it. Honouring the pick buys a round trip and an
// upstream error the gateway could have named.
//
// So the floor holds on every selection path. It already held on three — the
// smart strategy's sizing pass, the explicit-model passthrough's floorGuard,
// and the recovery list — and the primary targets of a non-smart rule were the
// fourth, where an admin pointing a `single` rule at a model with a floor got
// the upstream's refusal instead of ours.
func filterByFloor(snap *capability.Snapshot, targets []core.RoutingTarget,
	carried []string, trace *[]core.TraceEntry,
) []core.RoutingTarget {
	kept := make([]core.RoutingTarget, 0, len(targets))
	for _, t := range targets {
		if snap.MeetsFloor(t.ModelID, carried) {
			kept = append(kept, t)
		}
	}
	if dropped := len(targets) - len(kept); dropped > 0 {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "capability",
			Decision: fmt.Sprintf(
				"required-modality floor: dropped %d target(s) this request cannot satisfy",
				dropped),
		})
	}
	return kept
}
