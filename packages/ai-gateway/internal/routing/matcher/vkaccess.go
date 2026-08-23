package matcher

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// VKAccessFilter enforces the virtual key's model-access rule on the rule path.
//
// THE INVARIANT, stated once here because it is implemented in three places and
// they look inconsistent until you know why:
//
//	A virtual key may be SERVED only models on its allow list. What a caller may
//	ASK for is restricted only when the asked-for model is also the served one.
//
// Access is an ALLOW list; there is no deny concept. An empty list means
// unrestricted, not "nothing".
//
// The three legs:
//
//  1. No routing rule matched — the requested-model passthrough serves exactly
//     what was asked, so request and target are the same model and checking it
//     covers both. ingress/proxy/stage_routing_passthrough.go, 403
//     MODEL_NOT_ALLOWED. Note it tests the RESOLVED catalog model, not the raw
//     requested string; the error message names the request, the predicate does
//     not.
//  2. A routing rule matched — the rule redirects, so the requested model is
//     not what runs and cannot be the thing gated. Only the target is checked,
//     which is this filter.
//  3. Smart routing — the candidate set is narrowed to authorised models BEFORE
//     the judge prompt is built (strategies/strategy_smart.go), so the router
//     LLM can never select a model the key lacks. Filtering after the pick would
//     pay for a judge call and then discard its answer.
//
// WHY NOT "the requested model must always be allowed": `model: "auto"` is not
// a catalog model and can never appear on an allow list, so that rule would
// refuse every auto request from any restricted key — it would delete routing
// as a feature. Model codes that fan out to several providers have the same
// problem: there is no single ref to match. This was worked through
// adversarially; do not "fix" leg 2 to match leg 1.
type VKAccessFilter struct{}

// Keep returns the targets the request's virtual key permits. A request with no
// virtual key, or a key with an empty allow list, permits everything — an empty
// list means "unrestricted", not "nothing".
func (VKAccessFilter) Keep(targets []core.RoutingTarget, rctx *core.RoutingContext) []core.RoutingTarget {
	if rctx == nil || rctx.VirtualKey == nil || len(rctx.VirtualKey.AllowedModels) == 0 {
		return targets
	}
	var kept []core.RoutingTarget
	for _, t := range targets {
		if core.ModelMatchesAllowedRefs(t.ModelID, t.ProviderModelID, t.ProviderID, rctx.VirtualKey.AllowedModels) {
			kept = append(kept, t)
		}
	}
	return kept
}
