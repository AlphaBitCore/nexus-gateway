// routing_audit_trace.go — projection of a resolved routing result onto the
// audit record: the rich routing_trace JSONB the traffic drawer renders, and
// the router-LLM spend the smart strategy recorded on that trace.
package proxy

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// routingAuditTrace is stored in traffic_event.routing_trace.
// Its shape mirrors the /internal/routing-simulate response so that
// the Control Plane UI can display the same rich routing context in
// the traffic detail drawer that operators see in the routing simulator.
type routingAuditTrace struct {
	Stages          []routingcore.PipelineTraceEntry `json:"stages,omitempty"`
	Trace           []routingcore.TraceEntry         `json:"trace,omitempty"`
	Targets         []routingAuditTarget             `json:"targets,omitempty"`
	RecoveryTargets []routingAuditTarget             `json:"recoveryTargets,omitempty"`
	Substituted     bool                             `json:"substituted,omitempty"`
	OriginalModelID string                           `json:"originalModelId,omitempty"`
	// Attempts is the WALK, where the fields above are the PLAN. The plan says
	// which targets were considered; only this says which were tried, in what
	// order, and why that order.
	//
	// It became necessary when selection stopped being positional. A chain that
	// jumped over three entries to reach the fourth is either a context
	// overflow reaching for the largest window, a rate limit stepping off a
	// provider, or a bug — and from the plan alone those look identical. Added
	// rather than replacing anything, so rows written before it still read.
	Attempts []routingAuditAttempt `json:"attempts,omitempty"`
}

// routingAuditAttempt is one entry in the dispatch walk.
type routingAuditAttempt struct {
	Seq          int    `json:"seq"`
	ProviderName string `json:"provider,omitempty"`
	ModelCode    string `json:"model,omitempty"`
	// Dispatched separates a call that reached an upstream from a target that
	// was passed over. Both belong in the record — a target that never ran is
	// the thing an operator is most often trying to account for — but only the
	// first kind cost anything.
	Dispatched bool   `json:"dispatched"`
	StatusCode int    `json:"status,omitempty"`
	Code       string `json:"code,omitempty"`
	// SelectionReason is why this target came next: next-in-list,
	// largest-window, different-provider, deprioritised-retry.
	SelectionReason string `json:"selectionReason,omitempty"`
	// ErrorClass is what the failure was classified as, and the reason the
	// walk went where it did next. Status and code do not cover it: a
	// transport error has neither, and a provider error the classifier did not
	// recognise looks exactly like one it did.
	ErrorClass string `json:"errorClass,omitempty"`
	LatencyMs  int    `json:"latencyMs,omitempty"`
	// Coerced names the request fields we rewrote before dispatching to this
	// target, as "<from>→<to>". The response header carrying it reaches a
	// caller who discards it; the operator asking hours later has this row.
	Coerced []string `json:"coerced,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// recordWalk appends the executor's attempt chain to a routing trace already
// on the audit record. Silent when the request never reached routing, or when
// the walk made no attempts to describe.
func recordWalk(rec *audit.Record, attempts []executor.Attempt) {
	if rec == nil || len(attempts) == 0 {
		return
	}
	trace, ok := rec.RoutingTrace.(*routingAuditTrace)
	if !ok || trace == nil {
		return
	}
	out := make([]routingAuditAttempt, 0, len(attempts))
	for i := range attempts {
		a := &attempts[i]
		out = append(out, routingAuditAttempt{
			Seq:             i + 1,
			ProviderName:    a.Target.ProviderName,
			ModelCode:       a.Target.ModelCode,
			Dispatched:      a.Dispatched,
			StatusCode:      a.StatusCode,
			Code:            a.Code,
			SelectionReason: a.SelectionReason,
			ErrorClass:      a.ErrorClass,
			LatencyMs:       a.LatencyMs,
			Coerced:         a.Coerced,
			Error:           a.Error,
		})
	}
	trace.Attempts = out
}

type routingAuditTarget struct {
	ProviderID      string `json:"providerId,omitempty"`
	ProviderName    string `json:"providerName,omitempty"`
	ModelID         string `json:"modelId,omitempty"`
	ModelCode       string `json:"modelCode,omitempty"`
	ModelName       string `json:"modelName,omitempty"`
	ProviderModelID string `json:"providerModelId,omitempty"`
	Source          string `json:"source,omitempty"`
	AdapterType     string `json:"adapterType,omitempty"`
}

func buildRoutingAuditTrace(result *routingcore.RouteResult) *routingAuditTrace {
	if result == nil {
		return nil
	}
	if len(result.Trace) == 0 && len(result.PipelineTrace) == 0 && len(result.AllTargets()) == 0 {
		return nil
	}

	// The audit row keeps the shape it has always published: split by Source,
	// not by which plan list the target came from. Those differ — an inline
	// fallback-chain entry lives in the plan's Fallback list but has always
	// been published under "targets" — and moving entries between the two
	// arrays would change a field the Traffic drawer reads.
	var targets, recovery []routingAuditTarget
	for _, t := range result.AllTargets() {
		at := routingAuditTarget{
			ProviderID:      t.ProviderID,
			ProviderName:    t.ProviderName,
			ModelID:         t.ModelID,
			ModelCode:       t.ModelCode,
			ModelName:       t.ModelName,
			ProviderModelID: t.ProviderModelID,
			Source:          t.Source,
			AdapterType:     t.AdapterType,
		}
		if t.Source == "recovery" {
			recovery = append(recovery, at)
		} else {
			targets = append(targets, at)
		}
	}

	return &routingAuditTrace{
		Stages:          result.PipelineTrace,
		Trace:           result.Trace,
		Targets:         targets,
		RecoveryTargets: recovery,
		Substituted:     result.Substituted,
		OriginalModelID: result.OriginalModelID,
	}
}

// internalOpsTypeSmartRouter is the internal_ops_breakdown entry type for a
// smart-router LLM call. Consumers group on it, so it is a persisted value.
const internalOpsTypeSmartRouter = "smart-router"

// drainRouterCost accumulates the router-LLM cost recorded on a routing
// result's trace onto the audit record. Called for every result the routing
// stage observes — including one that is about to be replaced by the
// passthrough fallback — because the router call is already paid for by then
// and the replacement result's trace does not carry it.
//
// No double-count is possible: the pre-swap call is the first statement of the
// zero-targets branch, where the drained object is either discarded by the swap
// or abandoned by an early return, so the surviving result drained afterwards is
// never the same object. The smart strategy stamps the cost on exactly one trace
// entry per router call, which is what makes the sum correct.
//
// KNOWN LIMITATION — RouterCostUsd sums across every router call on the request
// while RouterProviderID is first-write-wins, so a request that made two router
// calls served by DIFFERENT vendors (two smart strategy nodes — a primary rule
// plus a smart fallback rule) books the combined cost against the first vendor
// alone. traffic_event carries one router_provider_id column, so a faithful
// per-vendor split needs a schema change and is deliberately out of scope here.
// The warning below is the signal: it is the only way that mis-attribution
// becomes visible, so it must not be dropped or downgraded. Note
// totalRouterCostUsd is rec.RouterCostUsd's running total AT THE POINT this
// entry is drained, not the request's final total — with three or more
// distinct vendors the first warning fires before later trace entries have
// been summed in, so it under-reports. attributedProviderId,
// unattributedProviderId, and unattributedCostUsd are each exact for the
// entry that triggered this warning.
func drainRouterCost(rec *audit.Record, rr *routingcore.RouteResult, logger *slog.Logger) {
	if rec == nil || rr == nil {
		return
	}
	for _, e := range rr.Trace {
		rec.RouterCostUsd += e.RouterCostUsd
		// Itemise the call beside the summed amount. Gated on token counts, not
		// on cost: an unpriced router model produces real usage and a zero cost,
		// and that call still happened and still needs a record. Gating on cost
		// would drop exactly the calls whose spend we cannot otherwise see.
		// Per-entry provider, so the multi-vendor case below — which the single
		// router_provider_id column cannot represent — stays recoverable here.
		if e.RouterPromptTokens > 0 || e.RouterCompletionTokens > 0 {
			rec.InternalOpsBreakdown = append(rec.InternalOpsBreakdown, audit.InternalOpsEntry{
				Type:                internalOpsTypeSmartRouter,
				Model:               e.RouterModelID,
				ProviderID:          e.RouterProviderID,
				PromptTokens:        e.RouterPromptTokens,
				CompletionTokens:    e.RouterCompletionTokens,
				CacheReadTokens:     e.RouterCacheReadTokens,
				CacheCreationTokens: e.RouterCacheCreationTokens,
				CostUsd:             e.RouterCostUsd,
			})
		}
		if e.RouterProviderID == "" {
			continue
		}
		if rec.RouterProviderID == "" {
			rec.RouterProviderID = e.RouterProviderID
			continue
		}
		if e.RouterProviderID != rec.RouterProviderID && logger != nil {
			logger.Warn("router spend attributed to one vendor while a second vendor also served a router call on this request",
				"requestId", rec.RequestID,
				"attributedProviderId", rec.RouterProviderID,
				"unattributedProviderId", e.RouterProviderID,
				"unattributedCostUsd", e.RouterCostUsd,
				"totalRouterCostUsd", rec.RouterCostUsd,
			)
		}
	}
}
