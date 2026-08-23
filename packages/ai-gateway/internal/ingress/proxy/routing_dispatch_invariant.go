// routing_dispatch_invariant.go — the served model must be one the router put
// forward.
//
// Scope is narrow: it catches a served model absent from the recorded candidate
// list, i.e. the executor attributing a response to a target outside the plan.
// It does NOT catch health reorder, the quota downgrade, or the recovery bypass
// — all three preserve set membership, and this is a set-membership check. The
// order/eligibility version lives in the executor.
//
// WARN, not refuse: at finalize the response is written and the upstream paid.
// Rows without a routing trace (realtime metering) exit at the type assertion.
package proxy

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// checkRoutedTargetWasACandidate warns when the model recorded as having served
// the request is absent from the routing trace's own candidate list.
//
// Silent when there is nothing to compare: no trace (the request never reached
// routing), no routed model (nothing dispatched), or an empty candidate list (a
// passthrough serving without a resolved plan). Each would flag the ABSENCE of
// a list rather than a departure from one.
//
// Unconditional rather than sampled — the question is "did it happen at all".
// Allocation-free on the non-violating path.
func checkRoutedTargetWasACandidate(rec *audit.Record, logger *slog.Logger) {
	if rec == nil || logger == nil || rec.RoutedModelID == "" {
		return
	}
	trace, ok := rec.RoutingTrace.(*routingAuditTrace)
	if !ok || trace == nil {
		return
	}
	if len(trace.Targets) == 0 && len(trace.RecoveryTargets) == 0 {
		return
	}
	for _, t := range trace.Targets {
		if t.ModelID == rec.RoutedModelID {
			return
		}
	}
	for _, t := range trace.RecoveryTargets {
		if t.ModelID == rec.RoutedModelID {
			return
		}
	}

	candidates := make([]string, 0, len(trace.Targets)+len(trace.RecoveryTargets))
	for _, t := range trace.Targets {
		candidates = append(candidates, t.ModelID)
	}
	for _, t := range trace.RecoveryTargets {
		candidates = append(candidates, t.ModelID)
	}
	logger.Warn("routing invariant: the model that served this request is not in its own candidate list",
		"requestId", rec.RequestID,
		"routedModelId", rec.RoutedModelID,
		"candidates", candidates,
		"note", "a stage after target selection replaced the pick without re-checking it",
	)
}
