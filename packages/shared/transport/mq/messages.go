package mq

import (
	"github.com/goccy/go-json"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// TrafficEventMessage is the canonical wire format for traffic events on MQ.
// Published by AI Gateway to "nexus.event.ai-traffic".
// Consumers (hub-db-writer, hub-alerting) deserialize from this.
//
// MUST stay JSON-tag-aligned with the Hub's consumer.TrafficEventMessage
// (packages/nexus-hub/internal/observability/consumer/message.go). The two are
// separate structs by design (value+omitempty here vs pointer-for-NULL there),
// but a tag added here without a matching tag there is silently dropped on the
// Hub. Enforced by TestTrafficEventMessage_NoStructDrift in that package.
type TrafficEventMessage struct {
	ID                string    `json:"id"`
	Source            string    `json:"source"`
	TraceID           string    `json:"traceId,omitempty"`
	ExternalRequestID string    `json:"externalRequestId,omitempty"`
	Timestamp         time.Time `json:"timestamp"`

	SourceIP   string `json:"sourceIp,omitempty"`
	TargetHost string `json:"targetHost,omitempty"`
	Method     string `json:"method,omitempty"`
	Path       string `json:"path,omitempty"`
	// TargetMethod and TargetPath are the HTTP method + path actually sent to
	// the upstream provider. Differ from Method/Path when the AI gateway
	// re-routes across wire formats; identical for compliance-proxy + agent
	// transparent traffic.
	TargetMethod string `json:"targetMethod,omitempty"`
	TargetPath   string `json:"targetPath,omitempty"`
	StatusCode   int    `json:"statusCode,omitempty"`
	// LatencyMs is emitted without omitempty: the Hub-side struct stores
	// this as *int, so a missing wire field becomes NULL in the DB.
	// That silently masked sub-millisecond cache hits (gateway truncated
	// time.Since().Milliseconds() to 0 then the marshaller dropped the
	// field). Senders should clamp real measurements to ≥1; 0 on the wire
	// unambiguously means "not measured".
	LatencyMs int `json:"latencyMs"`

	EntityType string `json:"entityType,omitempty"`
	EntityID   string `json:"entityId,omitempty"`
	EntityName string `json:"entityName,omitempty"`
	OrgID      string `json:"orgId,omitempty"`
	OrgName    string `json:"orgName,omitempty"`

	// Identity is the producer-built "who made this call" JSONB object. Typed as
	// any so producers can supply either a typed struct (ai-gateway emits a
	// compiled-encoder identityWire to avoid map reflection on the hot path) or a
	// plain map[string]any (compliance-proxy). The wire encoding is unchanged JSON
	// either way; this message is marshal-only (the consumer decodes into a
	// separate consumer.TrafficEventMessage), so the field type is producer-local.
	Identity any `json:"identity"`

	// EndpointType classifies the request modality. Values are the
	// canonical typology.EndpointKind strings ("chat", "embeddings",
	// "stt", "tts", "image_generation", "batch") — see
	// shared/transport/typology/endpointkind.go. Stamped from
	// audit.Record.EndpointType at recordToMessage time; persisted onto
	// traffic_event.endpoint_type; consumed by the cost-formula registry
	// in ai-gateway/internal/execution/estimator/.
	EndpointType string `json:"endpointType,omitempty"`

	// IngressFormat is the wire shape the CLIENT used on the request — the
	// provcore.Format key of the ingress endpoint that was hit ("openai",
	// "openai-responses", "anthropic", "gemini", …), post any
	// x-nexus-aigw-body-format override. Stamped from audit.Record.IngressFormat;
	// persisted onto traffic_event.ingress_format. The captured request AND
	// response bodies are stored in THIS frame (client-facing), so the
	// control-plane view-time normalize must decode them with this format, not
	// the upstream provider's adapter_type (which describes the bytes we sent
	// upstream — a body we never store). Empty for compliance-proxy + agent
	// traffic (transparent forwarding; ingress == upstream).
	IngressFormat string `json:"ingressFormat,omitempty"`

	ProviderID       string `json:"providerId,omitempty"`
	ProviderName     string `json:"providerName,omitempty"`
	ModelID          string `json:"modelId,omitempty"`
	ModelName        string `json:"modelName,omitempty"`
	PromptTokens     int64  `json:"promptTokens,omitempty"`
	CompletionTokens int64  `json:"completionTokens,omitempty"`
	TotalTokens      int64  `json:"totalTokens,omitempty"`
	// ReasoningTokens are chain-of-thought / thinking tokens reported by the
	// provider. omitempty keeps older publishers wire-compatible (consumer
	// treats absent as 0 → NULL in traffic_event.reasoning_tokens).
	ReasoningTokens int64 `json:"reasoningTokens,omitempty"`
	// ReasoningCostUsd is the cost subset attributable to ReasoningTokens.
	// Already counted inside EstimatedCostUsd; surfaced for cost breakdown.
	ReasoningCostUsd float64 `json:"reasoningCostUsd,omitempty"`
	EstimatedCostUsd float64 `json:"estimatedCostUsd,omitempty"`
	// CacheStatus is the UNIFIED rollup ("HIT" | "MISS") per
	// cost-estimation-architecture.md § 6.4. Producer derives via
	// audit.DeriveCacheStatus(GatewayCacheStatus, ProviderCacheStatus).
	// The four detail columns below are drill-down only; the audit drawer
	// renders them via the three layouts in § 6.4 — never as filter values.
	CacheStatus            string `json:"cacheStatus,omitempty"`
	GatewayCacheStatus     string `json:"gatewayCacheStatus,omitempty"`
	GatewayCacheSkipReason string `json:"gatewayCacheSkipReason,omitempty"`
	GatewayCacheKind       string `json:"gatewayCacheKind,omitempty"`
	// GatewayCacheL2EntryKey is the Redis HASH key of the L2 semantic-cache
	// entry that served this row, format "<redis_index_name>:<sha256(EmbeddingInput)[:16]>".
	// Stamped only on rows where GatewayCacheKind == "semantic"; empty
	// elsewhere. The admin UI's "Mark as bad cache hit" thumbs-down posts
	// this string as the poison-list entryKey; before this field the UI
	// posted traffic_event.id, which never matched the gateway's
	// IsPoisoned check.
	GatewayCacheL2EntryKey string  `json:"gatewayCacheL2EntryKey,omitempty"`
	ProviderCacheStatus    string  `json:"providerCacheStatus,omitempty"`
	OriginTZ               *string `json:"originTz,omitempty"`
	RoutedProviderID       string  `json:"routedProviderId,omitempty"`
	RoutedProviderName     string  `json:"routedProviderName,omitempty"`
	RoutedModelID          string  `json:"routedModelId,omitempty"`
	RoutedModelName        string  `json:"routedModelName,omitempty"`
	RoutingRuleID          string  `json:"routingRuleId,omitempty"`
	RoutingRuleName        string  `json:"routingRuleName,omitempty"`

	// Dual hook pipeline — each stage (request + response) records its own
	// decision, reason, reason_code, executions, and blocking rule.
	RequestHookDecision    string   `json:"requestHookDecision,omitempty"`
	RequestHookReason      string   `json:"requestHookReason,omitempty"`
	RequestHookReasonCode  string   `json:"requestHookReasonCode,omitempty"`
	ResponseHookDecision   string   `json:"responseHookDecision,omitempty"`
	ResponseHookReason     string   `json:"responseHookReason,omitempty"`
	ResponseHookReasonCode string   `json:"responseHookReasonCode,omitempty"`
	ComplianceTags         []string `json:"complianceTags,omitempty"`

	// Gateway response cache savings (USD). Non-nil on HIT/HIT_LIVE.
	GatewayCacheSavingsUsd *float64 `json:"gatewayCacheSavingsUsd,omitempty"`

	// EmbeddingCostUsd is the L2 semantic cache embedding cost stamped on every
	// L1 miss that triggered an embedding call. Non-nil only when an embedding
	// call ran; absent on L1 HIT, skip, or error.
	// EmbeddingModelID is the fleet-wide embedding model ID used for the call.
	EmbeddingCostUsd *float64 `json:"embeddingCostUsd,omitempty"`
	EmbeddingModelID string   `json:"embeddingModelId,omitempty"`

	// AIGuardCostUsd is the ai-guard classifier cost. Stamped on rows where
	// internal_purpose='ai-guard'; NULL on user-traffic rows.
	AIGuardCostUsd *float64 `json:"aiGuardCostUsd,omitempty"`

	// InternalOpsBreakdown is a catch-all for hook-type internal model calls
	// (prompt-shield, custom hooks invoking an LLM). Persisted to
	// traffic_event.internal_ops_breakdown JSONB. The producer decides the shape;
	// the UI renders key/value pairs.
	InternalOpsBreakdown json.RawMessage `json:"internalOpsBreakdown,omitempty"`

	// Provider-side prompt cache metrics.
	CacheCreationTokens *int64   `json:"cacheCreationTokens,omitempty"`
	CacheReadTokens     *int64   `json:"cacheReadTokens,omitempty"`
	CacheWriteCostUsd   *float64 `json:"cacheWriteCostUsd,omitempty"`
	CacheReadSavingsUsd *float64 `json:"cacheReadSavingsUsd,omitempty"`
	CacheNetSavingsUsd  *float64 `json:"cacheNetSavingsUsd,omitempty"`

	// Normalizer audit fields: how many bytes were stripped from the request
	// before upstream, and how many cache_control markers were injected.
	NormalizedStripCount *int `json:"normalizedStripCount,omitempty"`
	NormalizedStripBytes *int `json:"normalizedStripBytes,omitempty"`
	CacheMarkerInjected  *int `json:"cacheMarkerInjected,omitempty"`

	// APIKeyClass is a provider-prefix fragment (e.g. "sk-ant-", "AIza"),
	// non-reversible; NULL when the caller had no API key.
	// APIKeyFingerprint is SHA256(key)[:8] hex (16 chars). For ai-gateway this
	// is the caller's virtual key; for compliance-proxy and agent it is the
	// caller's real provider key.
	// UsageExtractionStatus is the extraction tier tag for token counts. The
	// full set (enforced by chk_traffic_event_usage_extraction_status on
	// traffic_event) is:
	//   ok | streaming_reported | streaming_estimated | streaming_unavailable |
	//   streaming_error | estimated | truncated | parse_failed | no_body |
	//   non_llm. "truncated" means token counts were parsed from a
	//   body clamped at the response-size cap — do not trust as confirmed.
	//   Empty = unknown/not applicable.
	APIKeyClass           string `json:"apiKeyClass,omitempty"`
	APIKeyFingerprint     string `json:"apiKeyFingerprint,omitempty"`
	UsageExtractionStatus string `json:"usageExtractionStatus,omitempty"`

	// ErrorCode and ErrorReason classify non-2xx outcomes with producer-specific
	// enum vocabulary (e.g. RATE_LIMITED, QUOTA_EXCEEDED, AUTH_INVALID). nil
	// when the producer did not classify the failure — that NULL is itself a
	// signal: "not Nexus-classified".
	ErrorCode   *string `json:"errorCode,omitempty"`
	ErrorReason *string `json:"errorReason,omitempty"`

	RequestHooksPipeline  any `json:"requestHooksPipeline,omitempty"`
	ResponseHooksPipeline any `json:"responseHooksPipeline,omitempty"`
	RoutingTrace          any `json:"routingTrace,omitempty"`
	Details               any `json:"details,omitempty"`

	// RequestBody and ResponseBody carry captured request/response bytes. Each
	// is a discriminated container ({kind: absent|inline|spill, ...}); see
	// packages/shared/audit/body.go. Hub db-writer demuxes onto
	// traffic_event_payload (inline_*_body OR *_spill_ref).
	RequestBody  audit.Body `json:"requestBody,omitempty"`
	ResponseBody audit.Body `json:"responseBody,omitempty"`

	// RequestNormalized and ResponseNormalized are the canonical normalized
	// representations of the captured bodies, produced by shared/normalize at
	// recordToMessage time. Persisted on traffic_event_normalized by the Hub
	// db-writer alongside the raw bytes on traffic_event_payload. Marshalled as
	// json.RawMessage to avoid a shared/normalize dependency here and to let
	// the wire format stay schema-stable across normalize schema bumps.
	RequestNormalized       json.RawMessage `json:"requestNormalized,omitempty"`
	ResponseNormalized      json.RawMessage `json:"responseNormalized,omitempty"`
	RequestNormalizeStatus  string          `json:"requestNormalizeStatus,omitempty"` // "ok" | "partial" | "failed"
	ResponseNormalizeStatus string          `json:"responseNormalizeStatus,omitempty"`
	RequestNormalizeError   string          `json:"requestNormalizeError,omitempty"`
	ResponseNormalizeError  string          `json:"responseNormalizeError,omitempty"`
	NormalizeVersion        string          `json:"normalizeVersion,omitempty"`

	// Request/ResponseRedactionSpans carry the transform spans relocated to
	// their offsets in the (post-redact) normalized payload, so the audit UI
	// can mark each redaction inline. Only set when the action is "redact";
	// nil otherwise. Marshalled as json.RawMessage for the same schema-stability
	// reason as the normalized payloads above.
	RequestRedactionSpans  json.RawMessage `json:"requestRedactionSpans,omitempty"`
	ResponseRedactionSpans json.RawMessage `json:"responseRedactionSpans,omitempty"`

	// BumpStatus is compliance-proxy specific (TLS bump outcome). Other
	// producers leave it empty.
	BumpStatus string `json:"bumpStatus,omitempty"`

	// PassthroughFlags carries the canonical-order bypass flags from the
	// emergency-passthrough config: one or more of {bypassHooks, bypassCache,
	// bypassNormalize}. Nil/empty when no bypass fired (the vast majority of
	// traffic). PassthroughReason is the operator's justification from the
	// most-specific active tier; non-empty only when PassthroughFlags is non-empty.
	PassthroughFlags  []string `json:"passthroughFlags,omitempty"`
	PassthroughReason string   `json:"passthroughReason,omitempty"`

	// InternalPurpose flags events written by internal subsystems so analytics
	// can filter them out of customer billing views. Known values: "ai-guard".
	// Nil for customer-facing traffic.
	InternalPurpose *string `json:"internalPurpose,omitempty"`

	// RequestBlockingRule and ResponseBlockingRule carry the (pack, pack_version,
	// rule_id) attribution for each pipeline stage. JSON tags match the
	// wire and DB column names (request_blocking_rule / response_blocking_rule).
	RequestBlockingRule  *json.RawMessage `json:"requestBlockingRule,omitempty"`
	ResponseBlockingRule *json.RawMessage `json:"responseBlockingRule,omitempty"`

	// CredentialID is the ai-gateway credential row that handled the upstream
	// call. Surfaces as traffic_event.credential_id so Hub credential-health
	// rollup jobs can group-by without JSONB extraction.
	CredentialID string `json:"credentialId,omitempty"`

	// ThingID and ThingName identify the Thing instance that emitted this event:
	//   - source="agent"            → originating agent device
	//   - source="ai-gateway"       → gateway instance that processed the request
	//   - source="compliance-proxy" → proxy instance that processed the request
	// ThingName is a denormalized snapshot of thing.name at emit time.
	ThingID   string `json:"thingId,omitempty"`
	ThingName string `json:"thingName,omitempty"`

	// Latency phase breakdown. Populated by all three data-plane services via
	// the shared/traffic PhaseTimer helper.
	//
	//   UpstreamTtfbMs   — TTFB from the upstream destination (first SSE chunk for streaming).
	//   UpstreamTotalMs  — Full upstream round-trip (header + body / stream close).
	//   RequestHooksMs   — Aggregate request-side hook latency.
	//   ResponseHooksMs  — Aggregate response-side hook latency.
	//   LatencyBreakdown — Per-source phase durations (ms); key set is closed per source.
	//
	// our_overhead_ms is not on the wire — derived as LatencyMs - UpstreamTotalMs (≥0) at read time.
	UpstreamTtfbMs   *int           `json:"upstreamTtfbMs,omitempty"`
	UpstreamTotalMs  *int           `json:"upstreamTotalMs,omitempty"`
	RequestHooksMs   *int           `json:"requestHooksMs,omitempty"`
	ResponseHooksMs  *int           `json:"responseHooksMs,omitempty"`
	LatencyBreakdown map[string]int `json:"latencyBreakdown,omitempty"`

	// AttestationVerified and AttestationAgentID are set by the compliance-proxy
	// when a CONNECT carries a verified X-Nexus-Attestation header and the proxy
	// tunneled transparently (skipping MITM + hook pipeline). Both absent on
	// regular MITM rows.
	AttestationVerified bool   `json:"attestationVerified,omitempty"`
	AttestationAgentID  string `json:"attestationAgentId,omitempty"`

	// SourceProcess is the short binary name of the emitting service
	// ("ai-gateway", "compliance-proxy", "agent"). Distinct from `source`
	// (which classifies how the row was captured — proxy / gateway / agent
	// path) only in that this column tracks the actual binary in case the
	// taxonomy grows. Persisted on traffic_event.source_process.
	SourceProcess string `json:"sourceProcess,omitempty"`
	// Action is the verb-ish role of the event in the emitting service's
	// vocabulary ("traffic" for ai-gateway, "compliance-traffic" for the
	// compliance-proxy, and one of "passthrough" / "inspect" / "deny" for the
	// agent — the agent stamps its per-flow policy decision here via
	// deriveAction, see packages/agent/internal/observability/audit/queue/writer_adapter.go).
	// Surfaced verbatim onto traffic_event.action. Useful when filtering rows
	// by emitter intent rather than the catch-all `source` enum.
	Action string `json:"action,omitempty"`
}

// AdminAuditMessage is the MQ wire format for admin audit log events.
// Published by Control Plane to "nexus.event.admin-audit".
// Consumers (hub-db-writer, hub-alerting) deserialize from this.
//
// The hash chain (previousHash / integrityHash) is computed Hub-side in
// packages/nexus-hub/internal/observability/audit/chain.go; sending hashes on the wire
// would let any CP replica fork the chain. The CP just formats + publishes.
type AdminAuditMessage struct {
	ID             string    `json:"id"`
	Timestamp      time.Time `json:"timestamp"`
	ActorID        string    `json:"actorId"`
	ActorLabel     string    `json:"actorLabel"`
	ActorRole      string    `json:"actorRole"`
	SourceIP       string    `json:"sourceIp,omitempty"`
	Action         string    `json:"action"`
	EntityType     string    `json:"entityType"`
	EntityID       string    `json:"entityId"`
	BeforeState    any       `json:"beforeState,omitempty"`
	AfterState     any       `json:"afterState,omitempty"`
	NexusRequestID string    `json:"nexusRequestId,omitempty"`
	// Via records the channel that initiated the mutation — "assistant" for an
	// AI-initiated admin write performed by the web assistant, empty for a direct
	// human/UI action. The Hub consumer feeds it into the audit hash chain so the
	// AI-attribution marker is tamper-evident.
	Via string `json:"via,omitempty"`
}
