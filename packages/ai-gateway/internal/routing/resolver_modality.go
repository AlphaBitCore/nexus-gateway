package routing

import (
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// applyModalityGuard drops every target whose catalog model type cannot serve
// the request's endpoint modality — so no routing strategy (single, loadbalance,
// conditional, latency, smart, fallback) dispatches a cross-modality model, e.g.
// an image model for a chat request or a chat model auto-routed onto an image
// endpoint. A drop is surfaced (WARN + pipeline-trace entry) rather than silent,
// so an operator debugging a downstream 404 sees the modality guard as the
// cause. Runs on the routing hot path: one inlined string comparison per target,
// filtered in place with no allocation.
func (r *Resolver) applyModalityGuard(targets []core.RoutingTarget, rctx *core.RoutingContext, plan *core.RoutingPlan) []core.RoutingTarget {
	kept, dropped := filterByModality(targets, rctx.EndpointType)
	if dropped > 0 {
		r.logger.Warn("routing: dropped cross-modality targets",
			"endpoint", rctx.EndpointType,
			"model", rctx.RequestedModel.ID,
			"dropped", dropped,
			"remaining", len(kept))
		plan.PipelineTrace = append(plan.PipelineTrace, core.PipelineTraceEntry{
			Stage:    2,
			Decision: fmt.Sprintf("modality guard dropped %d target(s) not servable by %s", dropped, rctx.EndpointType),
		})
	}
	return kept
}

// filterByModality drops every target whose catalog model type cannot serve
// the request's EndpointKind (see typology.EndpointKindAcceptsModelType). It
// filters in place — no allocation on the routing hot path — and preserves the
// order of the survivors. Kinds that do not bind to a catalog model (guardrail,
// batch, job, models) and targets with an empty ModelType keep every target,
// so the guard only ever removes a positively-mismatched modality. Returns the
// survivors and the number dropped so the caller can surface a non-silent drop.
func filterByModality(targets []core.RoutingTarget, kind typology.EndpointKind) ([]core.RoutingTarget, int) {
	n := 0
	for _, t := range targets {
		if typology.EndpointKindAcceptsModelType(kind, t.ModelType) {
			targets[n] = t
			n++
		}
	}
	return targets[:n], len(targets) - n
}
