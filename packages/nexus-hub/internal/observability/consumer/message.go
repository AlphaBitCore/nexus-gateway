package consumer

import (
	"github.com/goccy/go-json"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// TrafficEventMessage is the consumer-side deserialization struct for traffic
// events on MQ. Uses pointer types for nullable DB columns — nil values
// become SQL NULL. All three producers (ai-gateway, compliance-proxy, agent
// via Hub) serialize events into this struct.
//
// MUST stay JSON-tag-aligned with the producer/wire mq.TrafficEventMessage
// (packages/shared/transport/mq/messages.go). The two are separate structs by
// design (pointer-for-NULL here vs value+omitempty there), but a tag present
// on the producer and missing here is silently dropped on unmarshal. Enforced
// by TestTrafficEventMessage_NoStructDrift.
type TrafficEventMessage struct {
	ID                string    `json:"id"`
	Source            string    `json:"source"`
	TraceID           *string   `json:"traceId,omitempty"`
	ExternalRequestID *string   `json:"externalRequestId,omitempty"`
	Timestamp         time.Time `json:"timestamp"`

	SourceIP   *string `json:"sourceIp,omitempty"`
	TargetHost *string `json:"targetHost,omitempty"`
	Method     *string `json:"method,omitempty"`
	Path       *string `json:"path,omitempty"`
	// EndpointType is the canonical typology.EndpointKind ("chat",
	// "embeddings", ...) stamped by the producer (ai-gateway) onto the wire.
	// Empty for non-classified forwards (compliance-proxy / agent). Persisted
	// verbatim onto traffic_event.endpoint_type by the db-writer.
	EndpointType string `json:"endpointType,omitempty"`
	// IngressFormat is the client-facing wire shape (provcore.Format key:
	// "openai" / "openai-responses" / "anthropic" / "gemini" / …) stamped by the
	// producer (ai-gateway). The stored request + response bodies are in this
	// frame, so the control-plane view-time normalize decodes them with it.
	// Empty for compliance-proxy / agent transparent forwards. Persisted verbatim
	// onto traffic_event.ingress_format by the db-writer.
	IngressFormat string `json:"ingressFormat,omitempty"`
	// TargetMethod / TargetPath are the HTTP method and path actually sent to
	// upstream. Differs from Method/Path on AI gateway cross-format routes;
	// same as Method/Path for compliance-proxy and agent transparent traffic.
	TargetMethod *string `json:"targetMethod,omitempty"`
	TargetPath   *string `json:"targetPath,omitempty"`
	StatusCode   *int    `json:"statusCode,omitempty"`
	LatencyMs    *int    `json:"latencyMs,omitempty"`

	EntityType *string `json:"entityType,omitempty"`
	EntityID   *string `json:"entityId,omitempty"`
	EntityName *string `json:"entityName,omitempty"`
	OrgID      *string `json:"orgId,omitempty"`
	OrgName    *string `json:"orgName,omitempty"`

	Identity json.RawMessage `json:"identity,omitempty"`

	ProviderID       *string `json:"providerId,omitempty"`
	ProviderName     *string `json:"providerName,omitempty"`
	ModelID          *string `json:"modelId,omitempty"`
	ModelName        *string `json:"modelName,omitempty"`
	PromptTokens     *int    `json:"promptTokens,omitempty"`
	CompletionTokens *int    `json:"completionTokens,omitempty"`
	TotalTokens      *int    `json:"totalTokens,omitempty"`
	// ReasoningTokens are chain-of-thought / thinking tokens from the provider.
	// Pointer so absent (older publisher) → NULL in
	// traffic_event.reasoning_tokens; 0 written explicitly means "upstream
	// reported 0".
	ReasoningTokens *int `json:"reasoningTokens,omitempty"`
	// ReasoningCostUsd is the cost subset attributable to ReasoningTokens.
	// Already included inside EstimatedCostUSD; surfaced separately for
	// the Cost dashboard reasoning-ratio widget.
	ReasoningCostUsd *float64 `json:"reasoningCostUsd,omitempty"`
	EstimatedCostUSD *float64 `json:"estimatedCostUsd,omitempty"`
	// CacheStatus is the UNIFIED rollup (HIT | MISS) per cost-estimation-architecture.md § 6.4.
	// The four GatewayCache*/ProviderCacheStatus fields below are detail-only; they feed
	// the drawer's three layouts. CacheStatus is derived at the ai-gateway audit-writer
	// via audit.DeriveCacheStatus(GatewayCacheStatus, ProviderCacheStatus).
	CacheStatus            *string `json:"cacheStatus,omitempty"`
	GatewayCacheStatus     *string `json:"gatewayCacheStatus,omitempty"`
	GatewayCacheSkipReason *string `json:"gatewayCacheSkipReason,omitempty"`
	GatewayCacheKind       *string `json:"gatewayCacheKind,omitempty"`
	// GatewayCacheL2EntryKey is the Redis HASH key of the L2 semantic-cache
	// entry that served this row, format "<redis_index_name>:<sha256(EmbeddingInput)[:16]>".
	// Stamped only when GatewayCacheKind == "semantic"; nil elsewhere.
	// Producer at packages/shared/transport/mq/messages.go; persisted to
	// traffic_event.gateway_cache_l2_entry_key. Surfaced to the audit drawer
	// so "Mark as bad cache hit" posts the key that the gateway's IsPoisoned
	// check actually consults.
	GatewayCacheL2EntryKey string  `json:"gatewayCacheL2EntryKey,omitempty"`
	ProviderCacheStatus    *string `json:"providerCacheStatus,omitempty"`
	OriginTZ               *string `json:"originTz,omitempty"`
	RoutedProviderID       *string `json:"routedProviderId,omitempty"`
	RoutedProviderName     *string `json:"routedProviderName,omitempty"`
	RoutedModelID          *string `json:"routedModelId,omitempty"`
	RoutedModelName        *string `json:"routedModelName,omitempty"`
	RoutingRuleID          *string `json:"routingRuleId,omitempty"`
	RoutingRuleName        *string `json:"routingRuleName,omitempty"`

	// Dual hook pipeline. Each stage records its own decision +
	// reason + reason_code; persisted on traffic_event.{request,response}_hook_*.
	RequestHookDecision    *string  `json:"requestHookDecision,omitempty"`
	RequestHookReason      *string  `json:"requestHookReason,omitempty"`
	RequestHookReasonCode  *string  `json:"requestHookReasonCode,omitempty"`
	ResponseHookDecision   *string  `json:"responseHookDecision,omitempty"`
	ResponseHookReason     *string  `json:"responseHookReason,omitempty"`
	ResponseHookReasonCode *string  `json:"responseHookReasonCode,omitempty"`
	ComplianceTags         []string `json:"complianceTags,omitempty"`
	BumpStatus             *string  `json:"bumpStatus,omitempty"`

	// PassthroughFlags carries the canonical-order slice from
	// passthrough.Config.Flags() — {bypassHooks, bypassCache, bypassNormalize}.
	// Empty/absent when no bypass fired. PassthroughReason is the
	// operator-supplied justification from the most-specific active tier.
	PassthroughFlags  []string `json:"passthroughFlags,omitempty"`
	PassthroughReason string   `json:"passthroughReason,omitempty"`

	// Gateway response cache savings (USD). Non-nil on HIT/HIT_LIVE.
	GatewayCacheSavingsUsd *float64 `json:"gatewayCacheSavingsUsd,omitempty"`

	// Provider-side prompt cache metrics.
	CacheCreationTokens *int64   `json:"cacheCreationTokens,omitempty"`
	CacheReadTokens     *int64   `json:"cacheReadTokens,omitempty"`
	CacheWriteCostUsd   *float64 `json:"cacheWriteCostUsd,omitempty"`
	CacheReadSavingsUsd *float64 `json:"cacheReadSavingsUsd,omitempty"`
	CacheNetSavingsUsd  *float64 `json:"cacheNetSavingsUsd,omitempty"`

	// Normaliser audit metrics.
	NormalizedStripCount *int `json:"normalizedStripCount,omitempty"`
	NormalizedStripBytes *int `json:"normalizedStripBytes,omitempty"`
	CacheMarkerInjected  *int `json:"cacheMarkerInjected,omitempty"`

	// LLM signal extraction. See shared/mq/messages.go for scope.
	APIKeyClass           *string `json:"apiKeyClass,omitempty"`
	APIKeyFingerprint     *string `json:"apiKeyFingerprint,omitempty"`
	UsageExtractionStatus *string `json:"usageExtractionStatus,omitempty"`

	// Failure-reason classification. Any producer that classifies a
	// non-2xx outcome may populate these with its own enum vocabulary.
	// ai-gateway uses RATE_LIMITED / QUOTA_EXCEEDED / ROUTING_NO_MATCH /
	// AUTH_INVALID / AUTH_KEY_EXPIRED (sourced from writeDetailedErr's
	// `code` and `message` arguments). compliance-proxy may use its own
	// codes for hook reject / bump failure / inspection block. agent may
	// use codes for local interception decisions. nil when the producer
	// chose not to classify (e.g. raw upstream 5xx propagated unchanged —
	// leaving nil keeps upstream-error alerts distinguishable from
	// Nexus-classified failures).
	ErrorCode   *string `json:"errorCode,omitempty"`
	ErrorReason *string `json:"errorReason,omitempty"`

	SourceProcess *string `json:"sourceProcess,omitempty"`
	Action        *string `json:"action,omitempty"`

	RequestHooksPipeline  json.RawMessage `json:"requestHooksPipeline,omitempty"`
	ResponseHooksPipeline json.RawMessage `json:"responseHooksPipeline,omitempty"`
	RoutingTrace          json.RawMessage `json:"routingTrace,omitempty"`
	Details               json.RawMessage `json:"details,omitempty"`

	// Captured request/response bodies. Body is a discriminated container;
	// Hub demuxes onto traffic_event_payload.inline_*_body or *_spill_ref
	// columns based on Body.Kind.
	RequestBody  audit.Body `json:"requestBody,omitempty"`
	ResponseBody audit.Body `json:"responseBody,omitempty"`

	// Normalized representation produced by shared/normalize at capture time.
	// Persisted on traffic_event_normalized; consumed by the UI Normalized tab
	// and analytics needing protocol-agnostic access to the captured payload.
	// Empty when capture was off or the protocol could not be normalized.
	RequestNormalized       json.RawMessage `json:"requestNormalized,omitempty"`
	ResponseNormalized      json.RawMessage `json:"responseNormalized,omitempty"`
	RequestNormalizeStatus  string          `json:"requestNormalizeStatus,omitempty"`
	ResponseNormalizeStatus string          `json:"responseNormalizeStatus,omitempty"`
	RequestNormalizeError   string          `json:"requestNormalizeError,omitempty"`
	ResponseNormalizeError  string          `json:"responseNormalizeError,omitempty"`
	NormalizeVersion        string          `json:"normalizeVersion,omitempty"`

	// Transform spans relocated to their offsets in the post-redact normalized
	// payload, so the UI can mark each redaction inline. Set only when
	// action=="redact"; persisted on traffic_event_normalized.
	RequestRedactionSpans  json.RawMessage `json:"requestRedactionSpans,omitempty"`
	ResponseRedactionSpans json.RawMessage `json:"responseRedactionSpans,omitempty"`

	// InternalPurpose tags events written by internal subsystems (e.g.
	// "ai-guard"). Persisted on traffic_event.internal_purpose so the
	// admin UI can hide them from customer billing views by default.
	InternalPurpose *string `json:"internalPurpose,omitempty"`

	// Dual blocking-rule, one per pipeline stage. Persisted on
	// traffic_event.{request,response}_blocking_rule (JSONB).
	RequestBlockingRule  json.RawMessage `json:"requestBlockingRule,omitempty"`
	ResponseBlockingRule json.RawMessage `json:"responseBlockingRule,omitempty"`

	// CredentialID is the provider credential top-level column on traffic_event
	// for windowed group-by in credential-health-rollup; also available via
	// identity->'apiCredential'->>'id'.
	CredentialID *string `json:"credentialId,omitempty"`

	// Thing attribution. Identifies the Thing instance that emitted this
	// traffic_event. Semantic depends on `source`:
	//   source=agent             → originating agent device (Hub fills from
	//                              mTLS-resolved Thing context)
	//   source=ai-gateway        → the gateway instance that processed the
	//                              request (producer fills from its cfg)
	//   source=compliance-proxy  → the proxy instance that processed the
	//                              request (producer fills from its cfg)
	// Persisted to traffic_event.thing_id / thing_name.
	ThingID   *string `json:"thingId,omitempty"`
	ThingName *string `json:"thingName,omitempty"`

	// Latency phase breakdown. Producers omit fields they did not measure.
	// See docs/developers/architecture "Latency Phase Taxonomy" for the
	// per-source population matrix and JSONB key schema.
	UpstreamTtfbMs  *int `json:"upstreamTtfbMs,omitempty"`
	UpstreamTotalMs *int `json:"upstreamTotalMs,omitempty"`
	RequestHooksMs  *int `json:"requestHooksMs,omitempty"`
	ResponseHooksMs *int `json:"responseHooksMs,omitempty"`
	// Microsecond-precision siblings (additive; the _ms fields are unchanged).
	RequestHooksUs   *int            `json:"requestHooksUs,omitempty"`
	ResponseHooksUs  *int            `json:"responseHooksUs,omitempty"`
	LatencyBreakdown json.RawMessage `json:"latencyBreakdown,omitempty"`

	// Agent attestation passthrough. Compliance-proxy sets both when a verified
	// X-Nexus-Attestation header allowed transparent CONNECT tunnelling. Both
	// nil on regular MITM rows; nullable so the partial index
	// idx_traffic_event_attestation_verified stays compact.
	AttestationVerified *bool   `json:"attestationVerified,omitempty"`
	AttestationAgentID  *string `json:"attestationAgentId,omitempty"`

	// Internal-ops cost stamped by ai-gateway on every L2-touching row
	// (embedding lookup — both hit and miss). Producer side at
	// packages/shared/transport/mq/messages.go.
	EmbeddingCostUsd *float64 `json:"embeddingCostUsd,omitempty"`
	EmbeddingModelID string   `json:"embeddingModelId,omitempty"`

	// AI-guard classifier cost and catch-all internal-ops breakdown.
	// Producer at packages/shared/transport/mq/messages.go.
	// Persisted to traffic_event.ai_guard_cost_usd and
	// traffic_event.internal_ops_breakdown.
	AIGuardCostUsd       *float64        `json:"aiGuardCostUsd,omitempty"`
	InternalOpsBreakdown json.RawMessage `json:"internalOpsBreakdown,omitempty"`
}

// nullableJSON returns nil if the raw JSON is empty/null, otherwise returns
// it as-is. pgx stores nil as SQL NULL.
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}
