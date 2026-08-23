package routing

import (
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// reportModalityDrops records a cross-modality drop once for the whole request.
//
// The guard runs over each plan list separately, so the count has to be summed
// before it is reported: two entries saying "dropped 1" read as two unrelated
// events, and an operator counting them against the plan finds one target
// missing and two explanations.
func (r *Resolver) reportModalityDrops(plan *core.RoutingPlan, rctx *core.RoutingContext, dropped, remaining int) {
	if dropped > 0 {
		r.logger.Warn("routing: dropped cross-modality targets",
			"endpoint", rctx.EndpointType,
			"model", rctx.RequestedModel.ID,
			"dropped", dropped,
			"remaining", remaining)
		plan.PipelineTrace = append(plan.PipelineTrace, core.PipelineTraceEntry{
			Stage:    2,
			Decision: fmt.Sprintf("modality guard dropped %d target(s) not servable by %s", dropped, rctx.EndpointType),
		})
	}
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
