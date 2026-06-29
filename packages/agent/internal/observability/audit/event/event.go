// Package event defines the agent's internal audit event type that maps
// to traffic_event columns in the unified schema.
package event

import (
	"github.com/goccy/go-json"
	"time"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// Event is the Agent's internal representation of a captured traffic event.
// Maps to traffic_event columns in the unified schema.
type Event struct {
	ID            string    `json:"id"`
	TraceID       string    `json:"traceId"`
	Timestamp     time.Time `json:"timestamp"`
	SourceIP      string    `json:"sourceIp"`
	TargetHost    string    `json:"targetHost"`
	DestIP        string    `json:"destIp,omitempty"`
	DestPort      int       `json:"destPort,omitempty"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	StatusCode    int       `json:"statusCode"`
	LatencyMs     int       `json:"latencyMs"`
	BytesIn       int64     `json:"bytesIn,omitempty"`
	BytesOut      int64     `json:"bytesOut,omitempty"`
	SourceProcess string    `json:"sourceProcess,omitempty"`
	OSUser        string    `json:"osUser,omitempty"`
	Action        string    `json:"action"`

	HookDecision   string `json:"hookDecision,omitempty"`
	HookReason     string `json:"hookReason,omitempty"`
	HookReasonCode string `json:"hookReasonCode,omitempty"`
	// ComplianceTags is the merged compliance tag set emitted by the
	// agent's local hook pipeline. Persisted on traffic_event.compliance_tags
	// at the Hub; encoded on the SQLite queue as a JSON text column via
	// encodeTags / decodeTags so it round-trips the offline queue.
	ComplianceTags []string `json:"complianceTags,omitempty"`
	BumpStatus     string   `json:"bumpStatus,omitempty"`

	// Request-side LLM signals populated by the agent's traffic adapter.
	// Empty when the request did not match any provider adapter — the Hub
	// translator maps empty strings to SQL NULL on the wire.
	ProviderName      string `json:"providerName,omitempty"`
	ModelName         string `json:"modelName,omitempty"`
	ApiKeyClass       string `json:"apiKeyClass,omitempty"`
	ApiKeyFingerprint string `json:"apiKeyFingerprint,omitempty"`

	// IngressFormat is the domain-matched adapter id (interception_domain.
	// adapter_id, e.g. "openai" / "anthropic") the captured body is in.
	// Stored as TEXT in the SQLite queue, uploaded to Hub, and used by the
	// detail drawer's view-time recompute as the authoritative normalize
	// adapter. Empty when no domain adapter matched → recompute falls back to
	// path + content sniff.
	IngressFormat string `json:"ingressFormat,omitempty"`

	// Response-side usage populated by the agent's MITM relay after the
	// adapter's DetectResponseUsage (non-streaming) or UsageAccumulator (SSE).
	// Token pointers are nil when usage was unavailable; UsageExtractionStatus
	// describes why (one of traffic.UsageStatus values).
	PromptTokens          *int   `json:"promptTokens,omitempty"`
	CompletionTokens      *int   `json:"completionTokens,omitempty"`
	UsageExtractionStatus string `json:"usageExtractionStatus,omitempty"`

	HooksPipeline json.RawMessage `json:"hooksPipeline,omitempty"`
	Details       json.RawMessage `json:"details,omitempty"`

	// Failure-reason classification. Populate when the agent classifies a
	// non-2xx outcome itself (e.g. local interception rejection, mTLS
	// handshake failed, internal queue overflow). Leave empty for
	// raw-upstream-error pass-through. ErrorCode is a structured enum
	// (recommended: AGENT_INTERCEPT_BLOCKED, AGENT_MTLS_FAILED, etc.);
	// ErrorReason is the human-readable text.
	ErrorCode   string `json:"errorCode,omitempty"`
	ErrorReason string `json:"errorReason,omitempty"`

	PolicyRuleID string `json:"policyRuleId,omitempty"`

	// Classification inputs. DomainRuleID is the matched
	// interception_domain.id (empty when host wasn't configured to be
	// intercepted at all). PathAction is the resolved
	// PROCESS|PASSTHROUGH|BLOCK from the per-path rule (or
	// domain.default_path_action when no path matched). Together they
	// drive classify(): empty DomainRuleID = Untracked; PASSTHROUGH +
	// no hooks = Inspect; PROCESS + hooks ran = Processed/Blocked.
	DomainRuleID string `json:"domainRuleId,omitempty"`
	PathAction   string `json:"pathAction,omitempty"`

	EntityType string          `json:"entityType,omitempty"`
	EntityID   string          `json:"entityId,omitempty"`
	EntityName string          `json:"entityName,omitempty"`
	OrgID      string          `json:"orgId,omitempty"`
	OrgName    string          `json:"orgName,omitempty"`
	Identity   json.RawMessage `json:"identity,omitempty"`

	// PayloadRequest and PayloadResponse are the pre-hook request and
	// post-adapter response body bytes captured when the admin has
	// enabled the corresponding flag in
	// system_metadata["payload_capture.config"]. Nil when capture is
	// disabled for that stage. Bounded by the per-flow inspectBodyCap
	// (default 256 MiB).
	//
	// JSON-marshalling a []byte produces a base64 string, so the wire
	// format to Hub's /things/agent-audit endpoint is a base64-encoded
	// blob per field. Hub's AgentAuditEvent mirrors the []byte type on
	// the receive side and demuxes inline-vs-spill into the
	// requestBody / responseBody discriminator before publishing to MQ.
	// Populate RequestSpillRef / ResponseSpillRef to skip the inline
	// base64 hop when using spill storage.
	PayloadRequest   []byte                `json:"payloadRequest,omitempty"`
	PayloadResponse  []byte                `json:"payloadResponse,omitempty"`
	RequestSpillRef  *sharedaudit.SpillRef `json:"requestSpillRef,omitempty"`
	ResponseSpillRef *sharedaudit.SpillRef `json:"responseSpillRef,omitempty"`

	// NormalizedRequest / NormalizedResponse — pre-normalized
	// payload JSON, stamped by forward_handler after runtimeNormalize.
	// Stored as TEXT in the agent SQLite queue, sent up to Hub on
	// upload, and rendered by the Agent UI Event Details "Normalized"
	// tab. Empty / nil when no AI adapter matched (non-LLM traffic
	// or passthrough flow).
	NormalizedRequest  json.RawMessage `json:"normalizedRequest,omitempty"`
	NormalizedResponse json.RawMessage `json:"normalizedResponse,omitempty"`

	// RequestRedactionSpans / ResponseRedactionSpans — redaction spans
	// relocated to their offsets inside the (storage-redacted) normalized
	// payloads above. Stamped by the shared audit emitter when a hook's
	// action=redact governed the copies; nil for unredacted rows.
	// Stored as TEXT alongside the normalized columns and uploaded to Hub
	// so traffic_event_normalized.*_redaction_spans populate for agent rows.
	RequestRedactionSpans  json.RawMessage `json:"requestRedactionSpans,omitempty"`
	ResponseRedactionSpans json.RawMessage `json:"responseRedactionSpans,omitempty"`

	// Latency phase breakdown. Populated by cmd/agent/main.go's
	// OnFlowComplete from a traffic.PhaseSink and from the hook pipeline's
	// per-hook latency. Nil pointers stay NULL on the wire / DB column;
	// LatencyBreakdown serializes as a JSON map[string]int (per the
	// shared/traffic schema), or nil when no long-tail phase was recorded.
	UpstreamTtfbMs   *int           `json:"upstreamTtfbMs,omitempty"`
	UpstreamTotalMs  *int           `json:"upstreamTotalMs,omitempty"`
	RequestHooksMs   *int           `json:"requestHooksMs,omitempty"`
	ResponseHooksMs  *int           `json:"responseHooksMs,omitempty"`
	LatencyBreakdown map[string]int `json:"latencyBreakdown,omitempty"`
}

// (EntityBinding + SetEntityBinding were removed as dead code — no production
// caller; deleted alongside their unit tests rather than carried as legacy.)

// BuildDetails constructs the details JSONB from non-column fields.
func (e *Event) BuildDetails() {
	d := map[string]any{}
	if e.DestIP != "" {
		d["destIp"] = e.DestIP
	}
	if e.DestPort != 0 {
		d["destPort"] = e.DestPort
	}
	if e.BytesIn != 0 {
		d["bytesIn"] = e.BytesIn
	}
	if e.BytesOut != 0 {
		d["bytesOut"] = e.BytesOut
	}
	if e.PolicyRuleID != "" {
		d["policyRuleId"] = e.PolicyRuleID
	}
	if e.OSUser != "" {
		d["osUser"] = e.OSUser
	}
	if e.SourceProcess != "" {
		d["process"] = e.SourceProcess
	}
	if len(d) > 0 {
		e.Details, _ = json.Marshal(d)
	}
}
