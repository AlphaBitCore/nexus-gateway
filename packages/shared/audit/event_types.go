package audit

import (
	"context"
	"github.com/goccy/go-json"
	"time"
)

// AuditEvent is the canonical traffic_event record produced by data-plane
// services (compliance-proxy + agent today; ai-gateway likely later) and
// shipped to the Hub for persistence into the platform `traffic_event` +
// `traffic_event_payload` tables.
//
// Only the producer side lives here. Persistence wiring (MQ writer, NDJSON
// fallback, agent SQLite queue) is service-local.
type AuditEvent struct {
	ID            string
	TransactionID string
	ConnectionID  string
	TrafficSource string // COMPLIANCE_PROXY, DNS_TERMINATED, AGENT
	IngressType   string
	BumpStatus    string // BUMP_SUCCESS, BUMP_FAILED_PASSTHROUGH, etc.
	SourceIP      string
	TargetHost    string
	Method        string // may be empty for passthrough
	Path          string // may be empty for passthrough
	StatusCode    *int

	// Dual hook pipeline. Each stage records its own decision +
	// reason + reason_code + executions list (JSON bytes) + blocking_rule
	// (JSON bytes). Empty / nil mirrors NULL in the corresponding column.
	RequestHookDecision    string
	RequestHookReason      *string
	RequestHookReasonCode  *string
	RequestHooksPipeline   []byte
	RequestBlockingRule    []byte `json:"-"`
	ResponseHookDecision   *string
	ResponseHookReason     *string
	ResponseHookReasonCode *string
	ResponseHooksPipeline  []byte
	ResponseBlockingRule   []byte `json:"-"`

	// ComplianceTags is the merged compliance tag set emitted by the hook
	// pipeline. Forwarded into mq.TrafficEventMessage.ComplianceTags; the
	// Hub db-writer persists it on traffic_event.compliance_tags (text[]).
	ComplianceTags      []string
	LatencyMs           int
	Timestamp           time.Time
	SubjectID           *string
	DSARDeleteRequested *bool
	// UserAgent is the first User-Agent header observed inside the
	// CONNECT tunnel after TLS bump succeeds. Empty for passthrough hosts
	// (UA is inside TLS we can't decrypt) and for hosts that never send a
	// UA header. Used by the per-tool auto-discovery dashboard to group
	// audit rows by client tool.
	UserAgent *string

	// TraceID is the X-Nexus-Request-Id header extracted from the intercepted
	// HTTP request after TLS bump. Links this event to agent and AI gateway
	// events for the same request. Empty for passthrough (non-bumped) traffic.
	TraceID string

	// LLM signal extraction. Populated by the Traffic Adapter when the
	// intercepted request matches an AI provider. Empty-string fields mean
	// "unknown" or "not applicable" — serialized as the empty zero-value on
	// the outgoing MQ message and become SQL NULL on the Hub side.
	Provider              string
	Model                 string
	PromptTokens          int64
	CompletionTokens      int64
	TotalTokens           int64
	APIKeyClass           string
	APIKeyFingerprint     string
	UsageExtractionStatus string

	// IngressFormat is the wire-shape key the stored body was captured in.
	// For agent + compliance-proxy this is the domain-matched adapter id
	// (interception_domain.adapter_id, e.g. "openai" / "anthropic"), which
	// the view-time normalize recompute (Control Plane + Agent UI) keys on as
	// the authoritative adapter instead of resolving by path + content sniff.
	// Persisted onto traffic_event.ingress_format. Empty when no domain
	// adapter matched — recompute then falls back to path/sniff (old-row
	// behaviour preserved).
	IngressFormat string

	// Failure-reason classification. Populate when this audit row
	// represents a Nexus-side classified failure (e.g. blocking rule
	// rejection, bump-failed passthrough chosen by policy, internal
	// circuit-breaker open). Leave empty for raw-upstream-error
	// pass-through so analytics can distinguish those via NULL.
	ErrorCode   string
	ErrorReason string

	// RequestBody and ResponseBody are the discriminated body containers.
	// Producers populate via NewInlineBody / NewSpillBody / EmptyBody.
	// Stored on traffic_event_payload (inline columns or *_spill_ref) by the
	// Hub db-writer.
	RequestBody  Body
	ResponseBody Body

	// RequestNormalized / ResponseNormalized — agent-side pre-normalized
	// payload JSON. Stamped by forward_handler after runtimeNormalize so
	// the Agent UI can render a Normalized tab without a Hub round-trip
	// (agent must be self-contained — its data isn't uploaded in real time).
	// Hub also runs normalize on the raw body for compliance-proxy and the
	// traffic_event_normalized sidecar table; the two are independent and
	// may differ in normalizer version. Empty bytes = no AI adapter matched
	// or non-bumped flow.
	RequestNormalized  json.RawMessage
	ResponseNormalized json.RawMessage

	// RequestRedactionSpans / ResponseRedactionSpans carry the redaction
	// spans relocated to their offsets inside the (storage-redacted)
	// normalized payload above, so audit UIs can mark each redaction
	// inline. Populated by the audit emitter only when the storage policy
	// is "redact" and span application succeeded; nil otherwise so the
	// wire and DB columns stay NULL for unredacted rows.
	RequestRedactionSpans  json.RawMessage
	ResponseRedactionSpans json.RawMessage

	// Latency phase breakdown. Populated by forward_handler from a
	// traffic.PhaseSink attached to the upstream call's context and from
	// the hook pipeline's per-hook latencyMs. Nil pointers stay NULL on
	// the wire and on the DB column.
	UpstreamTtfbMs   *int
	UpstreamTotalMs  *int
	RequestHooksMs   *int
	ResponseHooksMs  *int
	LatencyBreakdown map[string]int

	// DomainRuleID is the matched interception_domain.id when the host
	// hit the structured domain table. Empty string means the host did
	// NOT match any interception_domain row — agent's audit.Classify
	// flags such rows as "Untracked" so they stay local-only at the
	// default trafficUploadLevel. cp leaves this empty too when no
	// matchedDomain was resolved (rare given cp only bumps allowlist
	// hosts, but possible during config-reload races).
	// PathAction is the resolved per-path action ("PROCESS" /
	// "PASSTHROUGH" / "BLOCK") that the forward_handler applied.
	// Distinguishes Inspect (matched + PASSTHROUGH) from Processed
	// (matched + PROCESS + hooks ran) on the agent UI.
	DomainRuleID string
	PathAction   string

	// Agent attestation passthrough. Populated by compliance-proxy
	// when an inbound CONNECT carries a verified X-Nexus-Attestation
	// header. AttestationVerified=true means CP transparently tunneled
	// the connection instead of running its own MITM + hook pipeline;
	// AttestationAgentID names the agent whose Ed25519 cert produced
	// the signature. Both empty/false on regular MITM rows so analytics
	// can filter "attested traffic" without a JOIN.
	AttestationVerified bool
	AttestationAgentID  string

	// Process attribution (agent only — cp leaves empty).
	// SourceProcess is the executable name (e.g. "Google Chrome
	// Helper", "node", "curl"). SourceProcessBundle is the macOS
	// bundle identifier (e.g. "com.google.Chrome.helper"). Stamped
	// from NEAppProxyFlow.metaData at flow_new and propagated
	// through the bridge so inspect rows show the originating app
	// in the UI App column instead of "—".
	SourceProcess       string
	SourceProcessBundle string
}

// Writer defines the interface for audit event persistence. Each data-plane
// service provides its own implementation (compliance-proxy: MQ + NDJSON
// fallback; agent: encrypted SQLite queue + Hub HTTP upload).
type Writer interface {
	// Enqueue adds an event to the write queue. Non-blocking; producer-side
	// behavior on overflow is implementation-defined (cp overflows to NDJSON;
	// agent persists synchronously to sqlite).
	Enqueue(event AuditEvent)
	// Flush writes all pending events immediately.
	Flush(ctx context.Context) error
	// Close flushes remaining events and stops the writer.
	Close(ctx context.Context) error
}

// QueueInspector exposes queue depth so health/alerting probes can detect
// backpressure on the audit pipeline. Optional — implementations satisfy
// it where queue depth is meaningful (e.g. cp's MQBatchWriter).
type QueueInspector interface {
	QueueLen() int
	QueueCap() int
}
