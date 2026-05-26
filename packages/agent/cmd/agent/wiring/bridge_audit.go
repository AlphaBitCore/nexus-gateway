package wiring

import (
	"context"
	"log/slog"
	"time"

	auditevent "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// OnFlowComplete records an audit event after a flow completes with full
// transfer statistics. Implements platform.FlowAuditor.
func (b *ConnectionBridge) OnFlowComplete(result api.FlowResult) {
	action := "passthrough"
	switch result.Decision {
	case api.DecisionInspect:
		action = "inspect"
	case api.DecisionDeny:
		action = "deny"
	}

	var policyRuleID string
	if result.PolicyRuleID != "" {
		policyRuleID = result.PolicyRuleID
	} else {
		b.policyMu.Lock()
		policyRuleID = b.policyResults[result.FlowID]
		delete(b.policyResults, result.FlowID)
		b.policyMu.Unlock()
	}

	slog.Debug("flow complete",
		"flowId", result.FlowID,
		"host", result.DstHost,
		"action", action,
		"hookDecision", result.HookDecision,
		"latencyMs", result.DurationMs,
		"bytesIn", result.BytesIn,
		"bytesOut", result.BytesOut,
	)

	var errorCode, errorReason string
	switch {
	case result.HookDecision == "reject_hard" || result.HookDecision == "block_soft":
		errorCode = "COMPLIANCE_BLOCKED"
		errorReason = result.HookReason
	case result.Decision == api.DecisionDeny:
		errorCode = "POLICY_DENIED"
		errorReason = "request denied by policy"
		if policyRuleID != "" {
			errorReason = "request denied by policy rule " + policyRuleID
		}
	case result.BumpStatus == "BUMP_FAILED_PASSTHROUGH":
		errorCode = "BUMP_FAILED"
		errorReason = "TLS inspection unavailable, connection passed through"
	}

	// TraceID equals FlowID so compliance-proxy and ai-gateway events generated
	// by this flow can be joined across services via the shared trace id.
	e := auditevent.Event{
		ID:                    result.FlowID,
		TraceID:               result.FlowID,
		Timestamp:             result.StartedAt,
		SourceIP:              result.SrcIP,
		SourceProcess:         result.Process.Name,
		OSUser:                result.Process.User,
		TargetHost:            result.DstHost,
		DestIP:                result.DstIP,
		DestPort:              result.DstPort,
		Method:                result.Method,
		Path:                  result.Path,
		Action:                action,
		PolicyRuleID:          policyRuleID,
		BumpStatus:            result.BumpStatus,
		BytesIn:               result.BytesIn,
		BytesOut:              result.BytesOut,
		LatencyMs:             result.DurationMs,
		HookDecision:          result.HookDecision,
		HookReason:            result.HookReason,
		HookReasonCode:        result.HookReasonCode,
		ComplianceTags:        result.ComplianceTags,
		ProviderName:          result.Provider,
		ModelName:             result.Model,
		ApiKeyClass:           result.ApiKeyClass,
		ApiKeyFingerprint:     result.ApiKeyFingerprint,
		PromptTokens:          result.PromptTokens,
		CompletionTokens:      result.CompletionTokens,
		UsageExtractionStatus: result.UsageExtractionStatus,
		ErrorCode:             errorCode,
		ErrorReason:           errorReason,
		UpstreamTtfbMs:        result.UpstreamTtfbMs,
		UpstreamTotalMs:       result.UpstreamTotalMs,
		RequestHooksMs:        result.RequestHooksMs,
		ResponseHooksMs:       result.ResponseHooksMs,
		LatencyBreakdown:      result.LatencyBreakdown,
		DomainRuleID:          result.DomainRuleID,
		PathAction:            result.PathAction,
	}
	b.applyPayloadCapture(&e, result.PayloadRequest, result.PayloadResponse)
	if err := b.AuditQueue.Record(e); err != nil {
		slog.Error("failed to record flow audit event", "flowId", result.FlowID, "error", err)
	}
	if e.ProviderName != "" && b.ProviderTrafficNotifier != nil {
		b.ProviderTrafficNotifier()
	}
}

// applyPayloadCapture decides inline-vs-spill for the captured request
// and response bytes and writes the result onto the audit event.
func (b *ConnectionBridge) applyPayloadCapture(e *auditevent.Event, reqBody, respBody []byte) {
	threshold := payloadcapture.DefaultMaxInlineBodyBytes
	if b.PayloadCaptureStore != nil {
		if v := b.PayloadCaptureStore.Get().MaxInlineBodyBytes; v > 0 {
			threshold = v
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if len(reqBody) > 0 {
		inline, ref, trunc := b.routeCaptured(ctx, e.ID, "request", "application/json", reqBody, threshold)
		e.PayloadRequest = inline
		e.RequestSpillRef = ref
		_ = trunc
	}
	if len(respBody) > 0 {
		inline, ref, trunc := b.routeCaptured(ctx, e.ID, "response", "application/json", respBody, threshold)
		e.PayloadResponse = inline
		e.ResponseSpillRef = ref
		_ = trunc
	}
}

// routeCaptured runs the inline-vs-spill decision for a single direction.
// Returns (inline, spillRef, truncated) — exactly one of inline / spillRef
// is populated per call.
func (b *ConnectionBridge) routeCaptured(ctx context.Context, eventID, direction, contentType string, body []byte, threshold int64) ([]byte, *sharedaudit.SpillRef, bool) {
	if int64(len(body)) <= threshold {
		return body, nil, false
	}
	if b.SpillUploader == nil || eventID == "" {
		clip := body[:threshold]
		out := make([]byte, len(clip))
		copy(out, clip)
		return out, nil, true
	}
	ref, err := b.SpillUploader.Upload(ctx, eventID, direction, contentType, body)
	if err != nil {
		slog.Warn("spill upload failed; falling back to inline-truncated",
			"eventId", eventID, "direction", direction, "error", err)
		clip := body[:threshold]
		out := make([]byte, len(clip))
		copy(out, clip)
		return out, nil, true
	}
	return nil, &ref, false
}
