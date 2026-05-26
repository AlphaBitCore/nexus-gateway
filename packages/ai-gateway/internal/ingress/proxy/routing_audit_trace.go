package proxy

import routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"

// routingAuditTrace is stored in traffic_event.routing_trace.
// Its shape mirrors the /internal/routing-simulate response so that
// the Control Plane UI can display the same rich routing context in
// the traffic detail drawer that operators see in the routing simulator.
type routingAuditTrace struct {
	Stages          []routingcore.PipelineTraceEntry `json:"stages,omitempty"`
	Trace           []routingcore.TraceEntry         `json:"trace,omitempty"`
	Targets         []routingAuditTarget        `json:"targets,omitempty"`
	RecoveryTargets []routingAuditTarget        `json:"recoveryTargets,omitempty"`
	Substituted     bool                        `json:"substituted,omitempty"`
	OriginalModelID string                      `json:"originalModelId,omitempty"`
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
	if len(result.Trace) == 0 && len(result.PipelineTrace) == 0 && len(result.Targets) == 0 {
		return nil
	}

	// RouteResult.Targets is a flat health-ranked list of primary, fallback,
	// and recovery targets. Split by Source so the audit record mirrors the
	// separate targets/recoveryTargets shape from the routing simulator.
	var targets, recovery []routingAuditTarget
	for _, t := range result.Targets {
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
