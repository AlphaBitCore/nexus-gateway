package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// spillEmitTimeout caps how long buildEvent will wait for a spillstore
// PutObject during request-time audit assembly. 5 s is well above the
// typical S3 PutObject latency but short enough that an unreachable spill
// backend cannot stall the proxy indefinitely.
const spillEmitTimeout = 5 * time.Second

// nullableString converts an empty string into a nil *string so the audit
// row's response_hook_decision column ends up SQL NULL when no response
// pipeline ran. Non-empty strings round-trip through a fresh pointer.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// AuditInfo holds the per-request metadata needed for audit logging that is
// not present in HookInput (which carries only hook-relevant fields).
type AuditInfo struct {
	TransactionID string
	ConnectionID  string
	// TraceID is the cross-service correlation id (X-Nexus-Request-Id header).
	// Seeded by the agent for intercepted flows; falls back to TransactionID
	// for traffic that enters the proxy directly without an upstream trace id.
	TraceID string
	// Headers are the sanitised request headers (auth headers already stripped).
	// Used to extract User-Agent for the auto-discovery dashboard.
	Headers map[string][]string
	// RequestMeta is the LLM signal extracted by the traffic Adapter's
	// DetectRequestMeta before the hook pipeline runs. Empty-string fields
	// mean "unknown / non-LLM" and become SQL NULL on the Hub side.
	RequestMeta traffic.RequestMeta

	// PhaseSink captures upstream TTFB + upstream-total during the forward
	// roundtrip (see shared/traffic/tracing.go). forward_handler creates
	// one PhaseSink per request and stamps the pointer here so buildEvent
	// can read the captured values at audit time. Nil when the request
	// never reached the upstream (e.g. request-stage block).
	PhaseSink *traffic.PhaseSink

	// LatencyBreakdown carries per-phase latency keys for compliance-proxy
	// rows (conn_setup_ms, tls_handshake_ms). forward_handler builds the
	// map and stamps it here; buildEvent copies it onto the AuditEvent.
	LatencyBreakdown map[string]int

	// ResponseContentType is the Content-Type of the upstream response.
	// Stamped by forward_handler / sse handler after the upstream call
	// returns so the spillstore Body container carries the truthful value.
	// The audit-time normalizer's byte-sniff fallback covers missing cases.
	ResponseContentType string

	// DomainRuleID is the matched interception_domain.id when the host
	// hit the structured domain table. Empty means no row matched;
	// classify() flags such rows as Untracked (local-only upload).
	// PathAction is the resolved per-path or default-domain action
	// ("PROCESS" / "PASSTHROUGH" / "BLOCK") so the Hub can distinguish
	// Inspect (PASSTHROUGH) from Processed (PROCESS + hooks ran).
	DomainRuleID string
	PathAction   string

	// SourceProcess and SourceProcessBundle carry the originating macOS
	// app name and bundle ID (extracted from NEAppProxyFlow.metaData at
	// flow_new by the agent). Left empty by the compliance-proxy.
	SourceProcess       string
	SourceProcessBundle string
	// SourceUser is the OS user owning the source process (agent only).
	SourceUser string

	// RequestNormalized / ResponseNormalized — V2 (#58) pre-normalized
	// payload JSON, stamped by forward_handler after runtimeNormalize
	// runs for the hook pipeline. Forwarded through buildEvent to the
	// AuditEvent so agent SQLite can persist the normalized shape and
	// the Agent UI Event Details Normalized tab can render without a
	// Hub round-trip. Empty json.RawMessage = no AI adapter matched.
	RequestNormalized  json.RawMessage
	ResponseNormalized json.RawMessage
}

// AuditEmitter maps compliance pipeline results to audit events and enqueues them.
type AuditEmitter struct {
	writer         audit.Writer
	logger         *slog.Logger
	spill          spillstore.SpillStore
	payloadCapture *payloadcapture.Store
}

// WithSpillStore wires an out-of-band body backend so captured bodies
// larger than MaxInlineBodyBytes are written to the configured backend
// and the audit row keeps only the SpillRef. Returns the receiver for
// chaining.
func (e *AuditEmitter) WithSpillStore(store spillstore.SpillStore) *AuditEmitter {
	e.spill = store
	return e
}

// WithPayloadCaptureStore wires the runtime payload-capture config
// snapshot. The emitter reads MaxInlineBodyBytes from this store on
// every event so admin-driven shadow updates take effect without a
// service restart. Returns the receiver for chaining.
func (e *AuditEmitter) WithPayloadCaptureStore(s *payloadcapture.Store) *AuditEmitter {
	e.payloadCapture = s
	return e
}

// NewAuditEmitter creates an emitter backed by the audit writer.
func NewAuditEmitter(writer audit.Writer, logger *slog.Logger) *AuditEmitter {
	return &AuditEmitter{
		writer: writer,
		logger: logger,
	}
}

// EmitDual emits an audit event carrying both request-stage and response-stage
// pipeline results. Call this from post-upstream paths (SSE and non-SSE).
// Either result may be nil; each pipeline lands in its own column on
// traffic_event.
func (e *AuditEmitter) EmitDual(
	input *core.HookInput,
	info AuditInfo,
	requestResult *CompliancePipelineResult,
	responseResult *CompliancePipelineResult,
	bumpStatus string,
	statusCode int,
	latencyMs int,
	requestBody []byte,
	responseBody []byte,
	usage traffic.UsageMeta,
) {
	event := e.buildEvent(input, info, requestResult, responseResult, bumpStatus, statusCode, latencyMs, requestBody, responseBody, usage)
	e.writer.Enqueue(event)
}

// Emit is the single-pipeline emit kept for sites that historically only
// carried one stage's result (request hard-reject path, exempted-request
// path, kill-switch path). Internally it forwards to EmitDual with the
// response result nil. New call sites should prefer EmitDual.
func (e *AuditEmitter) Emit(
	input *core.HookInput,
	info AuditInfo,
	result *CompliancePipelineResult,
	bumpStatus string,
	statusCode int,
	latencyMs int,
	requestBody []byte,
	responseBody []byte,
	usage traffic.UsageMeta,
) {
	event := e.buildEvent(input, info, result, nil, bumpStatus, statusCode, latencyMs, requestBody, responseBody, usage)
	e.writer.Enqueue(event)
}

// buildEvent constructs an AuditEvent from up to two pipeline results.
// `requestResult` and `responseResult` may each be nil — fields stamped
// onto the corresponding stage stay zero-value (empty string / nil pointer
// / nil slice) and the Hub db-writer persists them as SQL NULL.
func (e *AuditEmitter) buildEvent(
	input *core.HookInput,
	info AuditInfo,
	requestResult *CompliancePipelineResult,
	responseResult *CompliancePipelineResult,
	bumpStatus string,
	statusCode int,
	latencyMs int,
	requestBody []byte,
	responseBody []byte,
	usage traffic.UsageMeta,
) audit.AuditEvent {
	reqDecision, reqReason, reqReasonCode, reqPipeline, reqBlocking, reqTags := stagePayload(e, info, requestResult)
	respDecision, respReason, respReasonCode, respPipeline, respBlocking, respTags := stagePayload(e, info, responseResult)

	// Compliance tags are union-merged across stages (response stage may
	// add tags the request stage did not). Result.Tags are appended;
	// dedup is the analytics consumer's problem. Use a fresh slice so
	// reqTags' backing array cannot be aliased into the merged result.
	complianceTags := make([]string, 0, len(reqTags)+len(respTags))
	complianceTags = append(complianceTags, reqTags...)
	complianceTags = append(complianceTags, respTags...)

	var sc *int
	if statusCode > 0 {
		sc = &statusCode
	}

	errorCode, errorReason := classifyComplianceError(requestResult, responseResult, bumpStatus, statusCode, responseBody)

	// Capture User-Agent for the auto-discovery dashboard. Only present for
	// successfully bumped requests; passthrough flows have UA inside TLS.
	userAgent := extractUserAgent(info.Headers)

	// Map request-side LLM signals from the traffic adapter detector.
	// Empty-string fields become SQL NULL at the Hub.
	provider := info.RequestMeta.Provider
	model := info.RequestMeta.Model
	apiKeyClass := info.RequestMeta.ApiKeyClass
	apiKeyFP := info.RequestMeta.ApiKeyFingerprint

	// usage_extraction_status defaults to non_llm when no AI adapter was
	// hit and the caller did not stamp a status.
	usageStatus := string(usage.Status)
	if usageStatus == "" {
		if provider == "" {
			usageStatus = string(traffic.UsageStatusNonLLM)
		} else {
			usageStatus = string(traffic.UsageStatusNoBody)
		}
	}

	var promptTokens, completionTokens int64
	if usage.PromptTokens != nil {
		promptTokens = int64(*usage.PromptTokens)
	}
	if usage.CompletionTokens != nil {
		completionTokens = int64(*usage.CompletionTokens)
	}

	eventID := uuid.NewString()
	threshold := payloadcapture.DefaultMaxInlineBodyBytes
	if e.payloadCapture != nil {
		threshold = e.payloadCapture.Get().MaxInlineBodyBytes
	}
	// Bound by spillEmitTimeout: spillstore.EmitBody can issue network I/O
	// (S3 PutObject) and must not stall the proxy indefinitely. On timeout
	// EmitBody returns an inline-only container flagged truncated.
	ctx, cancel := context.WithTimeout(context.Background(), spillEmitTimeout)
	defer cancel()
	// Stamp actual Content-Types so the spillstore Body container carries
	// the truthful value. Request CT comes from the sanitised request headers;
	// response CT is stamped by the caller after the upstream call returns.
	requestCT := headerLookup(info.Headers, "Content-Type")
	requestBodyContainer := spillstore.EmitBody(ctx, e.spill, threshold, requestBody, requestCT, eventID, "request", false, e.logger)
	responseBodyContainer := spillstore.EmitBody(ctx, e.spill, threshold, responseBody, info.ResponseContentType, eventID, "response", false, e.logger)

	// Latency phase fields. Hook aggregates derive from per-hook latency_ms
	// in the JSONB pipelines. Upstream phase fields come from the PhaseSink
	// populated by shared/traffic tracing during the forward roundtrip.
	// LatencyBreakdown carries compliance-proxy long-tail keys from
	// forward_handler (conn_setup_ms, tls_handshake_ms).
	requestHooksMs := sumHooksPipelineLatencyMs(reqPipeline)
	responseHooksMs := sumHooksPipelineLatencyMs(respPipeline)
	var upstreamTtfb, upstreamTotal *int
	if info.PhaseSink != nil {
		upstreamTtfb = info.PhaseSink.TtfbMs()
		upstreamTotal = info.PhaseSink.TotalMs()
	}
	latencyBreakdown := info.LatencyBreakdown

	return audit.AuditEvent{
		ID:                     eventID,
		TransactionID:          info.TransactionID,
		ConnectionID:           info.ConnectionID,
		TraceID:                info.TraceID,
		TrafficSource:          "COMPLIANCE_PROXY",
		IngressType:            input.IngressType,
		BumpStatus:             bumpStatus,
		SourceIP:               input.SourceIP,
		TargetHost:             input.TargetHost,
		Method:                 input.Method,
		Path:                   input.Path,
		StatusCode:             sc,
		RequestHookDecision:    reqDecision,
		RequestHookReason:      reqReason,
		RequestHookReasonCode:  reqReasonCode,
		RequestHooksPipeline:   reqPipeline,
		RequestBlockingRule:    reqBlocking,
		ResponseHookDecision:   nullableString(respDecision),
		ResponseHookReason:     respReason,
		ResponseHookReasonCode: respReasonCode,
		ResponseHooksPipeline:  respPipeline,
		ResponseBlockingRule:   respBlocking,
		ComplianceTags:         complianceTags,
		LatencyMs:              latencyMs,
		Timestamp:              time.Now().UTC(),
		UserAgent:              userAgent,
		Provider:               provider,
		Model:                  model,
		PromptTokens:           promptTokens,
		CompletionTokens:       completionTokens,
		TotalTokens:            promptTokens + completionTokens,
		APIKeyClass:            apiKeyClass,
		APIKeyFingerprint:      apiKeyFP,
		UsageExtractionStatus:  usageStatus,
		RequestBody:            requestBodyContainer,
		ResponseBody:           responseBodyContainer,
		ErrorCode:              errorCode,
		ErrorReason:            errorReason,
		UpstreamTtfbMs:         upstreamTtfb,
		UpstreamTotalMs:        upstreamTotal,
		RequestHooksMs:         requestHooksMs,
		ResponseHooksMs:        responseHooksMs,
		LatencyBreakdown:       latencyBreakdown,
		DomainRuleID:        info.DomainRuleID,
		PathAction:          info.PathAction,
		SourceProcess:       info.SourceProcess,
		SourceProcessBundle: info.SourceProcessBundle,
		// V2 (#58) — pre-normalized payload JSON forwarded from
		// forward_handler's runtimeNormalize. nil/empty for non-AI traffic
		// and non-bumped flows.
		RequestNormalized:  info.RequestNormalized,
		ResponseNormalized: info.ResponseNormalized,
	}
}

// sumHooksPipelineLatencyMs walks the hooks_pipeline JSONB blob (an array
// of {latencyMs: int, ...} objects produced by stagePayload) and returns
// the sum of per-hook latency in ms. Returns nil for an empty / unparseable
// blob so the resulting traffic_event column stays NULL — distinguishing
// "no hooks ran" from "hooks ran in 0ms".
func sumHooksPipelineLatencyMs(pipelineJSON []byte) *int {
	if len(pipelineJSON) == 0 {
		return nil
	}
	var rows []struct {
		LatencyMs int `json:"latencyMs"`
	}
	if err := json.Unmarshal(pipelineJSON, &rows); err != nil {
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	total := 0
	for _, r := range rows {
		if r.LatencyMs > 0 {
			total += r.LatencyMs
		}
	}
	return &total
}

// classifyComplianceError derives (errorCode, errorReason) from the available
// pipeline results and upstream response. Priority order:
//  1. Request pipeline blocked → COMPLIANCE_BLOCKED
//  2. Response pipeline blocked → COMPLIANCE_BLOCKED
//  3. TLS bump failure → BUMP_FAILED
//  4. Upstream HTTP error → PROVIDER_ERROR with extracted message
func classifyComplianceError(
	requestResult *CompliancePipelineResult,
	responseResult *CompliancePipelineResult,
	bumpStatus string,
	statusCode int,
	responseBody []byte,
) (code, reason string) {
	if requestResult != nil && (requestResult.Decision == RejectHard || requestResult.Decision == BlockSoft) {
		return "COMPLIANCE_BLOCKED", requestResult.Reason
	}
	if responseResult != nil && (responseResult.Decision == RejectHard || responseResult.Decision == BlockSoft) {
		return "COMPLIANCE_BLOCKED", responseResult.Reason
	}
	if bumpStatus == "BUMP_FAILED_PASSTHROUGH" {
		return "BUMP_FAILED", "TLS inspection unavailable, connection passed through"
	}
	if statusCode >= 400 {
		return "PROVIDER_ERROR", extractProviderErrorMessage(responseBody, statusCode)
	}
	return "", ""
}

// extractProviderErrorMessage extracts a human-readable error message from a
// provider response body. Handles .error.message (OpenAI / Anthropic / Gemini)
// and top-level .message. Falls back to a truncated raw body, or a generic
// "provider returned HTTP <N>" when the body is empty.
func extractProviderErrorMessage(body []byte, statusCode int) string {
	if len(body) == 0 {
		return fmt.Sprintf("provider returned HTTP %d", statusCode)
	}
	if msg := gjson.GetBytes(body, "error.message").String(); msg != "" {
		return msg
	}
	if msg := gjson.GetBytes(body, "message").String(); msg != "" {
		return msg
	}
	if len(body) > 300 {
		return string(body[:300]) + "..."
	}
	return string(body)
}

// stagePayload reduces a single CompliancePipelineResult into the per-stage
// columns persisted on traffic_event. A nil result returns zero values so
// the corresponding columns end up SQL NULL.
//
// Returned fields, in order:
//
//	decision string         — RequestHookDecision / ResponseHookDecision
//	reason   *string        — RequestHookReason   / ResponseHookReason
//	code     *string        — RequestHookReasonCode / ResponseHookReasonCode
//	pipeline []byte (JSONB) — RequestHooksPipeline / ResponseHooksPipeline
//	blocking []byte (JSONB) — RequestBlockingRule / ResponseBlockingRule
//	tags     []string       — appended into the merged compliance_tags column
func stagePayload(e *AuditEmitter, info AuditInfo, r *CompliancePipelineResult) (string, *string, *string, []byte, []byte, []string) {
	if r == nil {
		return "", nil, nil, nil, nil, nil
	}
	decision := string(r.Decision)
	var reason, code *string
	if r.Reason != "" {
		v := r.Reason
		reason = &v
	}
	if r.ReasonCode != "" {
		v := r.ReasonCode
		code = &v
	}
	var pipeline []byte
	if len(r.HookResults) > 0 {
		if data, err := json.Marshal(r.HookResults); err != nil {
			e.logger.Error("compliance/emitter: failed to marshal hooks pipeline",
				"error", err,
				"transactionId", info.TransactionID,
			)
		} else {
			pipeline = data
		}
	}
	var blocking []byte
	if r.BlockingRule != nil {
		payload := rulepack.BlockingRule{
			Pack:        r.BlockingRule.Pack,
			PackVersion: r.BlockingRule.PackVersion,
			RuleID:      r.BlockingRule.RuleID,
		}
		if data, err := json.Marshal(payload); err != nil {
			e.logger.Error("compliance/emitter: failed to marshal blocking rule",
				"error", err,
				"transactionId", info.TransactionID,
			)
		} else {
			blocking = data
		}
	}
	return decision, reason, code, pipeline, blocking, r.Tags
}

// headerLookup returns the first value of a canonical-cased header key
// from the map (Go's net/http canonicalises on copy). Returns "" when absent.
func headerLookup(headers map[string][]string, key string) string {
	v := headers[key]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// extractUserAgent returns the User-Agent header value, or nil when absent so
// the audit row stores SQL NULL (auto-discovery uses IS NOT NULL semantics).
func extractUserAgent(headers map[string][]string) *string {
	// Direct O(1) map lookup using the canonical key form.
	// Go's net/http always canonicalises header keys, so "User-Agent"
	// is the only form that will appear in headers copied from r.Header.
	v := headers["User-Agent"]
	if len(v) == 0 || v[0] == "" {
		return nil
	}
	ua := v[0]
	// Cap at 512 chars to prevent oversized Chrome/Edge UA strings from
	// dominating the audit row; truncate with an ellipsis marker.
	if len(ua) > 512 {
		ua = ua[:509] + "..."
	}
	return &ua
}

// EmitKillSwitchPassthrough records an audit event for a connection that
// bypassed TLS bump because the kill switch was engaged. This ensures the
// compliance gap is visible in dashboards and analytics.
func (e *AuditEmitter) EmitKillSwitchPassthrough(sourceAddr, targetHost string) {
	sourceIP, _, _ := net.SplitHostPort(sourceAddr)
	if sourceIP == "" {
		sourceIP = sourceAddr
	}

	reason := "kill switch engaged — TLS bump bypassed"
	reasonCode := "KILLSWITCH_ENGAGED"

	event := audit.AuditEvent{
		ID:                    uuid.NewString(),
		TransactionID:         uuid.NewString(),
		TrafficSource:         "COMPLIANCE_PROXY",
		IngressType:           "COMPLIANCE_PROXY",
		BumpStatus:            "BUMP_DISABLED_EMERGENCY",
		SourceIP:              sourceIP,
		TargetHost:            targetHost,
		RequestHookDecision:   "PASSTHROUGH",
		RequestHookReason:     &reason,
		RequestHookReasonCode: &reasonCode,
		Timestamp:             time.Now().UTC(),
	}

	e.writer.Enqueue(event)
}

// EmitExempted records an audit event for a request exempted from compliance
// hooks. The hookDecision is "EXEMPTED" so dashboards can distinguish these
// from normal APPROVE/REJECT decisions.
func (e *AuditEmitter) EmitExempted(sourceIP, targetHost, exemptionID, exemptionReason string) {
	reason := fmt.Sprintf("temporary exemption %s: %s", exemptionID, exemptionReason)
	reasonCode := "EXEMPTED"

	event := audit.AuditEvent{
		ID:                    uuid.NewString(),
		TransactionID:         uuid.NewString(),
		TrafficSource:         "COMPLIANCE_PROXY",
		IngressType:           "COMPLIANCE_PROXY",
		BumpStatus:            "BUMP_SUCCESS",
		SourceIP:              sourceIP,
		TargetHost:            targetHost,
		RequestHookDecision:   "EXEMPTED",
		RequestHookReason:     &reason,
		RequestHookReasonCode: &reasonCode,
		Timestamp:             time.Now().UTC(),
	}

	e.writer.Enqueue(event)
}
