package audit

import (
	"encoding/json"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// toMessage converts an internal AuditEvent to the canonical wire format
// shared by all three data-plane producers (ai-gateway, compliance-proxy,
// agent-via-hub). The Hub db-writer deserializes this exact struct into
// `traffic_event` rows. thingID / thingName identify the emitting proxy
// instance and land on traffic_event.thing_id / thing_name.
func toMessage(e AuditEvent, thingID, thingName string) mq.TrafficEventMessage {
	details := map[string]any{
		"transactionId":       e.TransactionID,
		"connectionId":        e.ConnectionID,
		"trafficSource":       e.TrafficSource,
		"ingressType":         e.IngressType,
		"dsarDeleteRequested": e.DSARDeleteRequested,
	}
	if e.UserAgent != nil {
		details["userAgent"] = *e.UserAgent
	}

	var requestHooksPipeline any
	if len(e.RequestHooksPipeline) > 0 {
		requestHooksPipeline = json.RawMessage(e.RequestHooksPipeline)
	}
	var responseHooksPipeline any
	if len(e.ResponseHooksPipeline) > 0 {
		responseHooksPipeline = json.RawMessage(e.ResponseHooksPipeline)
	}

	// The compliance proxy has no per-request auth context — sourceIP is
	// the only identity signal it carries. Stamp identity.status="pending"
	// so the Hub IdentityEnricher job picks the row up on its next tick
	// and resolves the user via DeviceAssignment.ip_address lookup. Rows
	// that leave identity NULL are invisible to the job's WHERE filter.
	identity := map[string]any{"status": "pending"}

	msg := mq.TrafficEventMessage{
		ID:                    e.ID,
		Source:                "compliance-proxy",
		// SourceProcess + Action persist onto traffic_event.source_process
		// / .action; the consumer reads via stripNulPtr(e.SourceProcess) /
		// stripNulPtr(e.Action). Static values per emitter — Action carries
		// the role of the event in the proxy pipeline.
		SourceProcess: "compliance-proxy",
		Action:        "compliance-traffic",
		TraceID:               e.TraceID,
		Timestamp:             e.Timestamp,
		SourceIP:              e.SourceIP,
		Identity:              identity,
		TargetHost:            e.TargetHost,
		Method:                e.Method,
		Path:                  e.Path,
		// The compliance-proxy is a transparent forwarder — the upstream
		// path equals the client-requested path, so target_path mirrors
		// path 1:1 and target_method mirrors method.
		TargetMethod: e.Method,
		TargetPath:   e.Path,
		LatencyMs:             e.LatencyMs,
		BumpStatus:            e.BumpStatus,
		ProviderID:            e.Provider,
		ProviderName:          e.Provider,
		ModelID:               e.Model,
		ModelName:             e.Model,
		PromptTokens:          e.PromptTokens,
		CompletionTokens:      e.CompletionTokens,
		TotalTokens:           e.TotalTokens,
		APIKeyClass:           e.APIKeyClass,
		APIKeyFingerprint:     e.APIKeyFingerprint,
		UsageExtractionStatus: e.UsageExtractionStatus,
		RequestHookDecision:   e.RequestHookDecision,
		RequestHooksPipeline:  requestHooksPipeline,
		ResponseHooksPipeline: responseHooksPipeline,
		Details:               details,
		ThingID:               thingID,
		ThingName:             thingName,
	}
	if e.StatusCode != nil {
		msg.StatusCode = *e.StatusCode
	}
	if e.RequestHookReason != nil {
		msg.RequestHookReason = *e.RequestHookReason
	}
	if e.RequestHookReasonCode != nil {
		msg.RequestHookReasonCode = *e.RequestHookReasonCode
	}
	if e.ResponseHookDecision != nil {
		msg.ResponseHookDecision = *e.ResponseHookDecision
	}
	if e.ResponseHookReason != nil {
		msg.ResponseHookReason = *e.ResponseHookReason
	}
	if e.ResponseHookReasonCode != nil {
		msg.ResponseHookReasonCode = *e.ResponseHookReasonCode
	}
	msg.ComplianceTags = e.ComplianceTags
	if len(e.RequestBlockingRule) > 0 {
		raw := json.RawMessage(e.RequestBlockingRule)
		msg.RequestBlockingRule = &raw
	}
	if len(e.ResponseBlockingRule) > 0 {
		raw := json.RawMessage(e.ResponseBlockingRule)
		msg.ResponseBlockingRule = &raw
	}
	msg.RequestBody = e.RequestBody
	msg.ResponseBody = e.ResponseBody
	if e.ErrorCode != "" {
		c := e.ErrorCode
		msg.ErrorCode = &c
	}
	if e.ErrorReason != "" {
		r := e.ErrorReason
		msg.ErrorReason = &r
	}
	// Latency phase breakdown. nil pointers stay nil → SQL NULL.
	msg.UpstreamTtfbMs = e.UpstreamTtfbMs
	msg.UpstreamTotalMs = e.UpstreamTotalMs
	msg.RequestHooksMs = e.RequestHooksMs
	msg.ResponseHooksMs = e.ResponseHooksMs
	if len(e.LatencyBreakdown) > 0 {
		msg.LatencyBreakdown = e.LatencyBreakdown
	}
	// Forward attestation passthrough markers from the AuditEvent the verifier
	// stamps onto the row when CP transparently tunneled a CONNECT carrying a
	// verified header. Both empty/false on regular MITM rows; producer keeps
	// omitempty so unattested wire payloads are byte-identical to non-attested
	// builds.
	msg.AttestationVerified = e.AttestationVerified
	msg.AttestationAgentID = e.AttestationAgentID
	return msg
}

// ensure audit import path is referenced.
var _ = audit.BodyAbsent

// applyNormalize populates msg.RequestNormalized / ResponseNormalized
// (and the status / error / version columns) by invoking the wired
// shared/normalize closure against the captured raw bytes. No-op when
// fn is nil, when the adapter did not identify a provider, or when the
// body is empty / spilled (only inline bytes are normalized here).
//
// Adapter-type routing: e.Provider carries the traffic adapter's stable
// identifier (e.g. "openai", "anthropic", "gemini"), which is the same
// routing key the registry uses.
func applyNormalize(msg *mq.TrafficEventMessage, e AuditEvent, fn NormalizeFn) {
	if fn == nil || e.Provider == "" {
		return
	}
	adapterType := strings.ToLower(e.Provider)
	contentType := "application/json" // cp inspects JSON bodies post-bump
	stamped := false

	if reqBytes := inlineBodyBytes(e.RequestBody); len(reqBytes) > 0 {
		raw, status, errReason := fn("request", contentType, adapterType, e.Model, e.Path, false, reqBytes)
		if raw != nil || status != "" {
			msg.RequestNormalized = raw
			msg.RequestNormalizeStatus = status
			msg.RequestNormalizeError = errReason
			stamped = true
		}
	}
	if respBytes := inlineBodyBytes(e.ResponseBody); len(respBytes) > 0 {
		// cp captures responses on bumped traffic; SSE streams flow
		// through the streaming path and arrive here as the assembled
		// transcript when the live pipeline materialised one. Treat as
		// non-stream for the registry lookup; the OpenAI / Anthropic /
		// Gemini decoders all handle assembled JSON.
		raw, status, errReason := fn("response", contentType, adapterType, e.Model, e.Path, false, respBytes)
		if raw != nil || status != "" {
			msg.ResponseNormalized = raw
			msg.ResponseNormalizeStatus = status
			msg.ResponseNormalizeError = errReason
			stamped = true
		}
	}
	if stamped {
		msg.NormalizeVersion = normalizeWireVersion
	}
}

// normalizeWireVersion mirrors normalize.SchemaVersion without taking a
// direct dependency on that package — the wire format is intentionally
// a string contract between producer and Hub db-writer.
const normalizeWireVersion = "1"

// inlineBodyBytes returns the inline bytes from an audit.Body container,
// or nil when the body is absent or spilled. Spilled bodies stay raw on
// traffic_event_payload.*_spill_ref; only inline bytes are normalized here.
func inlineBodyBytes(b audit.Body) []byte {
	if b.Kind != audit.BodyInline {
		return nil
	}
	return b.InlineBytes
}
