package audit

import (
	"github.com/goccy/go-json"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
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
		ID:     e.ID,
		Source: "compliance-proxy",
		// SourceProcess + Action persist onto traffic_event.source_process
		// / .action; the consumer reads via stripNulPtr(e.SourceProcess) /
		// stripNulPtr(e.Action). Static values per emitter — Action carries
		// the role of the event in the proxy pipeline.
		SourceProcess: "compliance-proxy",
		Action:        "compliance-traffic",
		TraceID:       e.TraceID,
		Timestamp:     e.Timestamp,
		SourceIP:      e.SourceIP,
		Identity:      identity,
		TargetHost:    e.TargetHost,
		Method:        e.Method,
		Path:          e.Path,
		// The compliance-proxy is a transparent forwarder — the upstream
		// path equals the client-requested path, so target_path mirrors
		// path 1:1 and target_method mirrors method.
		TargetMethod:          e.Method,
		TargetPath:            e.Path,
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

// applyNormalize populates msg.RequestNormalized / ResponseNormalized
// (and the status / error / version / redaction-spans columns).
//
// The emitter-supplied normalized copies (e.RequestNormalized /
// e.ResponseNormalized) are authoritative when present: they are the
// payloads the hook pipeline actually saw, already governed by the
// stage's resolved action (span-redacted, or replaced by the drop-content
// placeholder), with the relocated span offsets riding alongside on
// e.*RedactionSpans. Re-deriving from raw bytes would discard that
// governance and mis-align the span offsets.
//
// Directions without a runtime copy fall back to invoking the wired
// shared/normalize closure against the captured raw bytes — which the
// emitter has already storage-governed, so the derived copy cannot leak.
// The fallback is skipped when fn is nil, when the adapter did not
// identify a provider, or when the body is empty / spilled (only inline
// bytes are normalized here).
//
// Adapter-type routing: e.Provider carries the traffic adapter's stable
// identifier (e.g. "openai", "anthropic", "gemini"), which is the same
// routing key the registry uses.
func applyNormalize(msg *mq.TrafficEventMessage, e AuditEvent, fn NormalizeFn) {
	stamped := false

	if len(e.RequestNormalized) > 0 {
		msg.RequestNormalized = e.RequestNormalized
		msg.RequestNormalizeStatus = "ok"
		stamped = true
	}
	if len(e.ResponseNormalized) > 0 {
		msg.ResponseNormalized = e.ResponseNormalized
		msg.ResponseNormalizeStatus = "ok"
		stamped = true
	}
	if len(e.RequestRedactionSpans) > 0 {
		msg.RequestRedactionSpans = e.RequestRedactionSpans
	}
	if len(e.ResponseRedactionSpans) > 0 {
		msg.ResponseRedactionSpans = e.ResponseRedactionSpans
	}

	if fn != nil && e.Provider != "" {
		adapterType := strings.ToLower(e.Provider)
		contentType := "application/json" // cp inspects JSON bodies post-bump

		if reqBytes := inlineBodyBytes(e.RequestBody); msg.RequestNormalized == nil && len(reqBytes) > 0 {
			raw, status, errReason := fn("request", contentType, adapterType, e.Model, e.Path, false, reqBytes)
			if raw != nil || status != "" {
				msg.RequestNormalized = raw
				msg.RequestNormalizeStatus = status
				msg.RequestNormalizeError = errReason
				stamped = true
			}
		}
		if respBytes := inlineBodyBytes(e.ResponseBody); msg.ResponseNormalized == nil && len(respBytes) > 0 {
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
	}
	if stamped {
		// Must be the schema version the registry stamps inside the
		// payloads themselves: any divergent value makes the hub backfill
		// treat every fresh proxy row as a version-mismatch candidate and
		// re-normalize it pointlessly.
		msg.NormalizeVersion = normcore.SchemaVersion
	}
}

// inlineBodyBytes returns the in-memory bytes from an audit.Body
// container, or nil when the body is absent. Spilled containers carry
// their bytes in memory too (the emitter re-attaches them after the
// spill decision; InlineBytes never serializes for non-inline kinds),
// so spill-destined bodies normalize live instead of waiting for the
// hub backfill to fetch the spill object.
func inlineBodyBytes(b audit.Body) []byte {
	return b.InlineBytes
}
