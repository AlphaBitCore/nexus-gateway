package wiring

import (
	"github.com/goccy/go-json"

	auditclassify "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/classify"
	auditevent "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
)

// ShouldUploadFlow gates the audit-queue write against the runtime
// trafficUploadLevel (Hub-pushed via agent_settings) by deriving a
// Classification from the captured event and applying the level rule.
//
// Levels:
//   - "all":        every Classification is uploaded (debugging).
//   - "processed":  Processed + Blocked + BumpFailed (default).
//   - "blocked":    Blocked + BumpFailed only (incident / strict mode).
func ShouldUploadFlow(e auditevent.Event, levelFn func() string) bool {
	level := ""
	if levelFn != nil {
		level = levelFn()
	}
	return auditclassify.ShouldUpload(auditclassify.Classify(e), level)
}

// AuditEventToMap serializes an agent-side audit.Event into the canonical
// MQ wire envelope that Hub's TrafficEventMessage consumer reads.
func AuditEventToMap(e auditevent.Event) map[string]any {
	m := map[string]any{
		"id":            e.ID,
		"traceId":       e.TraceID,
		"timestamp":     e.Timestamp,
		"sourceIp":      e.SourceIP,
		"sourceProcess": e.SourceProcess,
		"targetHost":    e.TargetHost,
		"method":        e.Method,
		"path":          e.Path,
		// Agent is a transparent forwarder — target_path mirrors request_path 1:1.
		"targetMethod":          e.Method,
		"targetPath":            e.Path,
		"statusCode":            e.StatusCode,
		"latencyMs":             e.LatencyMs,
		"action":                e.Action,
		"bumpStatus":            e.BumpStatus,
		"requestHookDecision":   e.HookDecision,
		"requestHookReason":     e.HookReason,
		"requestHookReasonCode": e.HookReasonCode,
		"complianceTags":        e.ComplianceTags,
		"providerName":          e.ProviderName,
		"modelName":             e.ModelName,
		// Domain-matched adapter id → traffic_event.ingress_format so the
		// control plane's view-time normalize recompute keys on the
		// authoritative adapter for this agent row (empty ⇒ path/sniff).
		"ingressFormat":         e.IngressFormat,
		"apiKeyClass":           e.ApiKeyClass,
		"apiKeyFingerprint":     e.ApiKeyFingerprint,
		"usageExtractionStatus": e.UsageExtractionStatus,
		"entityType":            e.EntityType,
		"entityId":              e.EntityID,
		"entityName":            e.EntityName,
		"orgId":                 e.OrgID,
		"orgName":               e.OrgName,
	}
	if len(e.Identity) > 0 {
		m["identity"] = e.Identity
	} else {
		m["identity"] = map[string]any{"status": "pending"}
	}
	if e.ErrorCode != "" {
		m["errorCode"] = e.ErrorCode
	}
	if e.ErrorReason != "" {
		m["errorReason"] = e.ErrorReason
	}
	if e.PromptTokens != nil {
		m["promptTokens"] = *e.PromptTokens
	}
	if e.CompletionTokens != nil {
		m["completionTokens"] = *e.CompletionTokens
	}
	if len(e.HooksPipeline) > 0 {
		m["requestHooksPipeline"] = e.HooksPipeline
	}
	// Agent-only metadata that doesn't have a first-class column in
	// traffic_event. Fold into `details` JSONB.
	details := map[string]any{}
	if e.DestIP != "" {
		details["destIp"] = e.DestIP
	}
	if e.DestPort != 0 {
		details["destPort"] = e.DestPort
	}
	if e.BytesIn != 0 {
		details["bytesIn"] = e.BytesIn
	}
	if e.BytesOut != 0 {
		details["bytesOut"] = e.BytesOut
	}
	if e.PolicyRuleID != "" {
		details["policyRuleId"] = e.PolicyRuleID
	}
	if e.OSUser != "" {
		details["osUser"] = e.OSUser
	}
	if len(details) > 0 {
		if raw, err := json.Marshal(details); err == nil {
			m["details"] = json.RawMessage(raw)
		}
	}
	if len(e.PayloadRequest) > 0 {
		m["payloadRequest"] = e.PayloadRequest
	}
	if len(e.PayloadResponse) > 0 {
		m["payloadResponse"] = e.PayloadResponse
	}
	// Oversize bodies: the drain step uploaded them to S3 and stamped an
	// S3 SpillRef here. Hub demuxes inline-vs-spill from these keys.
	if e.RequestSpillRef != nil {
		m["requestSpillRef"] = e.RequestSpillRef
	}
	if e.ResponseSpillRef != nil {
		m["responseSpillRef"] = e.ResponseSpillRef
	}
	// Pre-computed NormalizedPayload JSON. Hub-side
	// AgentAuditAPI.Normalize is also available; sending the agent
	// pre-normalized lets Hub skip the redundant work for AI traffic
	// the agent already understood.
	if len(e.NormalizedRequest) > 0 {
		m["normalizedRequest"] = e.NormalizedRequest
	}
	if len(e.NormalizedResponse) > 0 {
		m["normalizedResponse"] = e.NormalizedResponse
	}
	// Redaction spans for the storage-governed normalized copies above —
	// Hub forwards them onto traffic_event_normalized.*_redaction_spans.
	if len(e.RequestRedactionSpans) > 0 {
		m["requestRedactionSpans"] = e.RequestRedactionSpans
	}
	if len(e.ResponseRedactionSpans) > 0 {
		m["responseRedactionSpans"] = e.ResponseRedactionSpans
	}
	if e.UpstreamTtfbMs != nil {
		m["upstreamTtfbMs"] = *e.UpstreamTtfbMs
	}
	if e.UpstreamTotalMs != nil {
		m["upstreamTotalMs"] = *e.UpstreamTotalMs
	}
	if e.RequestHooksMs != nil {
		m["requestHooksMs"] = *e.RequestHooksMs
	}
	if e.ResponseHooksMs != nil {
		m["responseHooksMs"] = *e.ResponseHooksMs
	}
	if len(e.LatencyBreakdown) > 0 {
		m["latencyBreakdown"] = e.LatencyBreakdown
	}
	return m
}

// SplitByPayloadBudget greedy-splits hubEvents into chunks each under
// budget bytes (approximate, per approxJSONSize). Always returns at
// least one chunk; a single event larger than the budget rides alone.
func SplitByPayloadBudget(hubEvents []map[string]any, budget int) [][]map[string]any {
	if len(hubEvents) == 0 {
		return nil
	}
	out := make([][]map[string]any, 0, 1)
	cur := make([]map[string]any, 0, len(hubEvents))
	curSize := 0
	for _, evt := range hubEvents {
		sz := 1024
		if pr, ok := evt["payloadRequest"].([]byte); ok {
			sz += (len(pr) * 4) / 3
		}
		if pr, ok := evt["payloadResponse"].([]byte); ok {
			sz += (len(pr) * 4) / 3
		}
		if curSize+sz > budget && len(cur) > 0 {
			out = append(out, cur)
			cur = make([]map[string]any, 0, len(hubEvents))
			curSize = 0
		}
		cur = append(cur, evt)
		curSize += sz
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// SplitHTTPByPayloadBudget is the same shape as SplitByPayloadBudget but
// for the HTTP-fallback hub.AuditEvent slice path.
func SplitHTTPByPayloadBudget(hubEvents []hub.AuditEvent, budget int) [][]hub.AuditEvent {
	if len(hubEvents) == 0 {
		return nil
	}
	out := make([][]hub.AuditEvent, 0, 1)
	cur := make([]hub.AuditEvent, 0, len(hubEvents))
	curSize := 0
	for _, e := range hubEvents {
		sz := 1024 + (len(e.PayloadRequest)*4)/3 + (len(e.PayloadResponse)*4)/3
		if curSize+sz > budget && len(cur) > 0 {
			out = append(out, cur)
			cur = make([]hub.AuditEvent, 0, len(hubEvents))
			curSize = 0
		}
		cur = append(cur, e)
		curSize += sz
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// BuildHTTPAuditEvents converts a slice of auditevent.Event to the
// hub.AuditEvent shape used by the HTTP-fallback upload path.
func BuildHTTPAuditEvents(events []auditevent.Event) []hub.AuditEvent {
	hubEvents := make([]hub.AuditEvent, len(events))
	for i, e := range events {
		var details json.RawMessage
		detailMap := map[string]any{}
		if e.DestIP != "" {
			detailMap["destIp"] = e.DestIP
		}
		if e.DestPort != 0 {
			detailMap["destPort"] = e.DestPort
		}
		if e.BytesIn != 0 {
			detailMap["bytesIn"] = e.BytesIn
		}
		if e.BytesOut != 0 {
			detailMap["bytesOut"] = e.BytesOut
		}
		if e.PolicyRuleID != "" {
			detailMap["policyRuleId"] = e.PolicyRuleID
		}
		if e.OSUser != "" {
			detailMap["osUser"] = e.OSUser
		}
		if len(detailMap) > 0 {
			if raw, err := json.Marshal(detailMap); err == nil {
				details = raw
			}
		}
		hubEvents[i] = hub.AuditEvent{
			ID:                    e.ID,
			TraceID:               e.TraceID,
			Timestamp:             e.Timestamp,
			SourceIP:              e.SourceIP,
			SourceProcess:         e.SourceProcess,
			TargetHost:            e.TargetHost,
			Method:                e.Method,
			Path:                  e.Path,
			StatusCode:            e.StatusCode,
			LatencyMs:             e.LatencyMs,
			Action:                e.Action,
			BumpStatus:            e.BumpStatus,
			RequestHookDecision:   e.HookDecision,
			RequestHookReason:     e.HookReason,
			RequestHookReasonCode: e.HookReasonCode,
			ComplianceTags:        e.ComplianceTags,
			Details:               details,
			PayloadRequest:        e.PayloadRequest,
			PayloadResponse:       e.PayloadResponse,
			RequestSpillRef:       e.RequestSpillRef,
			ResponseSpillRef:      e.ResponseSpillRef,
		}
	}
	return hubEvents
}
