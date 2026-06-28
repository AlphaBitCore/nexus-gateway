package audit

import (
	"github.com/goccy/go-json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	nexushttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

const maxAuditBatchSize = 500

// AgentAuditAPI handles agent-specific audit endpoints.
//
// This handler does NOT decide inline-vs-spill and does NOT call
// SpillStore.Put. The agent makes that decision locally
// (agent/internal/intercept/payload_capture.go) and either:
//
//   - inlines small bodies into PayloadRequest / PayloadResponse
//     (base64-encoded over the JSON wire), or
//   - writes large bodies to its own SpillStore and ships only a SpillRef
//     in RequestSpillRef / ResponseSpillRef.
//
// Hub merely demuxes the two cases into the audit MQ envelope's
// requestBody / responseBody Body discriminator.
type AgentAuditAPI struct {
	MQProducer mq.Producer
	// Normalize, when non-nil, projects agent-captured request/response
	// bytes into the canonical NormalizedPayload shape and stamps the
	// result on the outbound MQ envelope (requestNormalized /
	// responseNormalized / normalizeStatus / normalizeError /
	// normalizeVersion). Wired from cmd/nexus-hub via shared/normalize so
	// agent traffic populates traffic_event_normalized alongside ai-gateway
	// and compliance-proxy. Nil keeps the sidecar row empty for agent traffic.
	Normalize func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (raw json.RawMessage, status, errReason string)
}

// AgentAuditEvent is the event format uploaded by the Agent.
type AgentAuditEvent struct {
	ID            string `json:"id"`
	TraceID       string `json:"traceId,omitempty"`
	Timestamp     string `json:"timestamp"`
	SourceIP      string `json:"sourceIp,omitempty"`
	TargetHost    string `json:"targetHost,omitempty"`
	Method        string `json:"method,omitempty"`
	Path          string `json:"path,omitempty"`
	StatusCode    int    `json:"statusCode,omitempty"`
	LatencyMs     int    `json:"latencyMs,omitempty"`
	SourceProcess string `json:"sourceProcess,omitempty"`
	Action        string `json:"action,omitempty"`
	// wire keys are requestHookDecision/Reason/ReasonCode (agent
	// AuditEventToMap stamps the request-stage hook decision under those
	// keys; emit envelope below also uses the request* prefix for
	// consumer alignment). Old hookDecision tag silently dropped the
	// field on bind → cp-ui Detail showed empty.
	HookDecision   string   `json:"requestHookDecision,omitempty"`
	HookReason     string   `json:"requestHookReason,omitempty"`
	HookReasonCode string   `json:"requestHookReasonCode,omitempty"`
	BumpStatus     string   `json:"bumpStatus,omitempty"`
	ComplianceTags []string `json:"complianceTags,omitempty"`

	// Request-side LLM signals from the agent's traffic adapter.
	ProviderName string `json:"providerName,omitempty"`
	ModelName    string `json:"modelName,omitempty"`
	ApiKeyClass  string `json:"apiKeyClass,omitempty"`
	// The node-asserted attribution fields (apiKeyFingerprint,
	// entityType/entityId/entityName/orgId/orgName, identity) are intentionally
	// NOT decoded here. They drove per-VK/per-org billing, analytics, and SIEM,
	// so trusting a node's self-assertion let any enrolled agent frame a victim
	// VK/org. The Hub stamps these server-side (empty; thing_id is the only
	// authoritative attribution on this path) — see UploadAgentAudit. The agent
	// may still send the JSON keys; Go silently ignores them. Do NOT re-add these
	// struct fields and copy them into the envelope — that reopens the forgery.

	// Response-side usage extracted by the agent's MITM relay.
	PromptTokens          *int   `json:"promptTokens,omitempty"`
	CompletionTokens      *int   `json:"completionTokens,omitempty"`
	UsageExtractionStatus string `json:"usageExtractionStatus,omitempty"`

	// Failure-reason classification.
	ErrorCode   string `json:"errorCode,omitempty"`
	ErrorReason string `json:"errorReason,omitempty"`

	HooksPipeline json.RawMessage `json:"hooksPipeline,omitempty"`
	Details       json.RawMessage `json:"details,omitempty"`
	// entityType/entityId/entityName/orgId/orgName/identity removed:
	// see the note above — node-asserted attribution is never trusted.

	// Captured request/response bodies. Exactly one of
	// {Payload*, *SpillRef} is populated per direction:
	//
	//   - PayloadRequest / PayloadResponse: bytes <= MaxInlineBodyBytes,
	//     base64 inlined into the JSON envelope. Empty when capture is
	//     disabled or the size exceeded the inline cutoff (in which
	//     case *SpillRef is set instead).
	//   - RequestSpillRef / ResponseSpillRef: agent already wrote the
	//     body to its SpillStore backend; Hub forwards the ref into
	//     the MQ envelope unchanged.
	PayloadRequest   []byte                `json:"payloadRequest,omitempty"`
	PayloadResponse  []byte                `json:"payloadResponse,omitempty"`
	RequestSpillRef  *sharedaudit.SpillRef `json:"requestSpillRef,omitempty"`
	ResponseSpillRef *sharedaudit.SpillRef `json:"responseSpillRef,omitempty"`

	// PayloadRequestContentType / PayloadResponseContentType travel only
	// when the agent inlined a body and want the inline MIME type to
	// land on traffic_event_payload.*_content_type. SpillRef cases carry
	// ContentType inside the ref already.
	PayloadRequestContentType  string `json:"payloadRequestContentType,omitempty"`
	PayloadResponseContentType string `json:"payloadResponseContentType,omitempty"`

	// PayloadRequestTruncated / PayloadResponseTruncated propagate the
	// agent's "we capped capture at MaxInlineBodyBytes" / "we capped
	// streaming capture at perObjectCap" decision so traffic_event_payload
	// reflects reality.
	PayloadRequestTruncated  bool `json:"payloadRequestTruncated,omitempty"`
	PayloadResponseTruncated bool `json:"payloadResponseTruncated,omitempty"`

	// Latency phase breakdown. Agent populates these via the shared
	// traffic.PhaseSink wired in cmd/agent/main.go OnFlowComplete; Hub
	// forwards them onto the MQ envelope so the consumer writes the
	// matching columns on traffic_event.
	UpstreamTtfbMs   *int           `json:"upstreamTtfbMs,omitempty"`
	UpstreamTotalMs  *int           `json:"upstreamTotalMs,omitempty"`
	RequestHooksMs   *int           `json:"requestHooksMs,omitempty"`
	ResponseHooksMs  *int           `json:"responseHooksMs,omitempty"`
	LatencyBreakdown map[string]int `json:"latencyBreakdown,omitempty"`

	// NormalizedRequest / NormalizedResponse — the agent's runtime-normalized
	// payload copies, already governed by the hook stage's action
	// (span-redacted, or replaced by the drop-content placeholder). When
	// present they take precedence over Hub-side re-normalization of the
	// raw bytes: under a redact policy the raw copy may have been dropped
	// entirely while the governed normalized copy still carries the
	// redacted content. RequestRedactionSpans / ResponseRedactionSpans are
	// the span offsets relocated to those governed copies; they land on
	// traffic_event_normalized.*_redaction_spans. This is content, not
	// attribution — accepting it from the node is the same trust decision
	// as accepting the payload bytes themselves.
	NormalizedRequest      json.RawMessage `json:"normalizedRequest,omitempty"`
	NormalizedResponse     json.RawMessage `json:"normalizedResponse,omitempty"`
	RequestRedactionSpans  json.RawMessage `json:"requestRedactionSpans,omitempty"`
	ResponseRedactionSpans json.RawMessage `json:"responseRedactionSpans,omitempty"`
}

// UploadAgentAudit handles POST /api/internal/things/agent-audit.
// Accepts a JSON array of audit events, enriches with thingID, publishes to MQ.
func (h *AgentAuditAPI) UploadAgentAudit(c echo.Context) error {
	if h.MQProducer == nil {
		return serviceUnavailable(c, "event queue temporarily unavailable, retry later")
	}

	var events []AgentAuditEvent
	if err := c.Bind(&events); err != nil {
		return badRequest(c, "invalid request body: expected JSON array of events")
	}

	if len(events) == 0 {
		return badRequest(c, "empty event batch")
	}
	if len(events) > maxAuditBatchSize {
		return c.JSON(http.StatusRequestEntityTooLarge, nexushttperr.ErrJSON("batch exceeds maximum size of 500 events", "validation_error", "PAYLOAD_TOO_LARGE"))
	}

	thing := ThingFromContext(c)
	var thingID, thingName string
	if thing != nil {
		thingID = thing.ID
		thingName = thing.Name
	} else {
		// Header fallback (used by callers without mTLS Thing resolution,
		// mostly tests). thingName is left empty in this case — the
		// db-writer will store NULL; analytics can JOIN thing on thing_id
		// to recover the name.
		thingID = c.Request().Header.Get("X-Thing-Id")
	}

	ctx := c.Request().Context()
	accepted := make([]string, 0, len(events))

	for _, evt := range events {
		envelope := map[string]any{
			"id":            evt.ID,
			"traceId":       evt.TraceID,
			"timestamp":     evt.Timestamp,
			"source":        "agent",
			"sourceIp":      evt.SourceIP,
			"targetHost":    evt.TargetHost,
			"method":        evt.Method,
			"path":          evt.Path,
			"statusCode":    evt.StatusCode,
			"latencyMs":     evt.LatencyMs,
			"sourceProcess": evt.SourceProcess,
			"action":        evt.Action,
			// Agent uploads carry only request-stage hook signals.
			"requestHookDecision":   evt.HookDecision,
			"requestHookReason":     evt.HookReason,
			"requestHookReasonCode": evt.HookReasonCode,
			"bumpStatus":            evt.BumpStatus,
			"complianceTags":        evt.ComplianceTags,
			"providerName":          evt.ProviderName,
			"modelName":             evt.ModelName,
			"apiKeyClass":           evt.ApiKeyClass,
			"promptTokens":          evt.PromptTokens,
			"completionTokens":      evt.CompletionTokens,
			"usageExtractionStatus": evt.UsageExtractionStatus,
			"requestHooksPipeline":  evt.HooksPipeline,
			"details":               evt.Details,
			// The per-VK / per-org / per-user attribution columns
			// (entityType/entityId/entityName/orgId/orgName/identity/
			// apiKeyFingerprint) drive billing, analytics, and SIEM. They MUST be
			// server-derived from the authenticated principal — NEVER copied from
			// the node's self-asserted payload, or any enrolled agent (the lowest-
			// trust credential) could attribute its traffic to a victim VK/org and
			// frame them for security events they never generated. The agent path
			// authenticates only the thing identity (mTLS device token), so the
			// only authoritative attribution here is thing_id (stamped below);
			// these fields are left empty and the real user/org is resolved
			// downstream by joining thing_id → DeviceAssignment → user → org.
			// They are deliberately NOT read from evt.* — that would re-open the
			// cross-VK / cross-org forgery.
			"entityType":        "",
			"entityId":          "",
			"entityName":        "",
			"orgId":             "",
			"orgName":           "",
			"identity":          "",
			"apiKeyFingerprint": "",
			"thingId":           thingID,
			"thingName":         thingName,
		}
		if evt.ErrorCode != "" {
			envelope["errorCode"] = evt.ErrorCode
		}
		if evt.ErrorReason != "" {
			envelope["errorReason"] = evt.ErrorReason
		}
		// Forward each non-nil latency phase pointer onto the envelope;
		// the MQ consumer maps these to the matching traffic_event columns.
		if evt.UpstreamTtfbMs != nil {
			envelope["upstreamTtfbMs"] = *evt.UpstreamTtfbMs
		}
		if evt.UpstreamTotalMs != nil {
			envelope["upstreamTotalMs"] = *evt.UpstreamTotalMs
		}
		if evt.RequestHooksMs != nil {
			envelope["requestHooksMs"] = *evt.RequestHooksMs
		}
		if evt.ResponseHooksMs != nil {
			envelope["responseHooksMs"] = *evt.ResponseHooksMs
		}
		if len(evt.LatencyBreakdown) > 0 {
			envelope["latencyBreakdown"] = evt.LatencyBreakdown
		}

		// Demux inline-vs-spill into the audit.Body discriminator.
		// The agent has already made the choice locally — lift each
		// direction into the right Body shape.
		envelope["requestBody"] = buildAgentBody(
			evt.PayloadRequest, evt.RequestSpillRef,
			evt.PayloadRequestContentType, evt.PayloadRequestTruncated)
		envelope["responseBody"] = buildAgentBody(
			evt.PayloadResponse, evt.ResponseSpillRef,
			evt.PayloadResponseContentType, evt.PayloadResponseTruncated)

		// The agent's storage-governed normalized copies are authoritative
		// when uploaded: they are the payloads the agent's hook pipeline
		// saw, with the storage policy already applied and span offsets
		// aligned. Re-deriving from raw bytes would discard that
		// governance — and under a redact policy the raw copy may be
		// absent entirely.
		stamped := false
		if len(evt.NormalizedRequest) > 0 {
			envelope["requestNormalized"] = evt.NormalizedRequest
			envelope["requestNormalizeStatus"] = "ok"
			stamped = true
		}
		if len(evt.NormalizedResponse) > 0 {
			envelope["responseNormalized"] = evt.NormalizedResponse
			envelope["responseNormalizeStatus"] = "ok"
			stamped = true
		}
		if len(evt.RequestRedactionSpans) > 0 {
			envelope["requestRedactionSpans"] = evt.RequestRedactionSpans
		}
		if len(evt.ResponseRedactionSpans) > 0 {
			envelope["responseRedactionSpans"] = evt.ResponseRedactionSpans
		}

		// Directions without an uploaded copy: project agent-captured bytes
		// into the canonical NormalizedPayload shape. Adapter-type routing
		// uses the agent traffic adapter's stable Provider identifier
		// (RequestMeta.Provider) — e.g. "openai" / "anthropic" / "gemini" —
		// which is what the registry expects. Spilled bodies are skipped;
		// Hub does not re-fetch from spill for normalize purposes.
		if h.Normalize != nil && evt.ProviderName != "" {
			adapter := strings.ToLower(evt.ProviderName)
			if len(evt.NormalizedRequest) == 0 && len(evt.PayloadRequest) > 0 {
				ct := evt.PayloadRequestContentType
				if ct == "" {
					ct = "application/json"
				}
				raw, status, errReason := h.Normalize("request", ct, adapter, evt.ModelName, evt.Path, false, evt.PayloadRequest)
				if raw != nil || status != "" {
					envelope["requestNormalized"] = raw
					envelope["requestNormalizeStatus"] = status
					envelope["requestNormalizeError"] = errReason
					stamped = true
				}
			}
			if len(evt.NormalizedResponse) == 0 && len(evt.PayloadResponse) > 0 {
				ct := evt.PayloadResponseContentType
				if ct == "" {
					ct = "application/json"
				}
				stream := strings.Contains(strings.ToLower(ct), "event-stream")
				raw, status, errReason := h.Normalize("response", ct, adapter, evt.ModelName, evt.Path, stream, evt.PayloadResponse)
				if raw != nil || status != "" {
					envelope["responseNormalized"] = raw
					envelope["responseNormalizeStatus"] = status
					envelope["responseNormalizeError"] = errReason
					stamped = true
				}
			}
		}
		if stamped {
			envelope["normalizeVersion"] = normcore.SchemaVersion
		}

		data, err := json.Marshal(envelope)
		if err != nil {
			continue
		}
		if err := h.MQProducer.Enqueue(ctx, "nexus.event.agent", data); err != nil {
			break
		}
		if evt.ID != "" {
			accepted = append(accepted, evt.ID)
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"accepted": accepted,
	})
}

// buildAgentBody folds the agent's per-direction (inline | spill | absent)
// choice into a sharedaudit.Body the traffic consumer can demux. Exactly
// one of the inputs is meaningful per direction; the absent case is
// represented by both zero-length inline and nil ref.
func buildAgentBody(inline []byte, ref *sharedaudit.SpillRef, contentType string, truncated bool) sharedaudit.Body {
	if ref != nil {
		return sharedaudit.NewSpillBody(ref, ref.Size, truncated, ref.ContentType)
	}
	if len(inline) == 0 {
		return sharedaudit.EmptyBody()
	}
	return sharedaudit.NewInlineBody(inline, int64(len(inline)), truncated, contentType)
}
