package trafficstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"
)

// TrafficEvent represents a row from the unified traffic_event table.
// Source is the writer binary: "ai-gateway" | "compliance-proxy" | "agent"
// (enforced by CHECK constraint chk_traffic_event_source). The UI/API
// translates these to product domains (vk|proxy|agent) via the domain
// package.
type TrafficEvent struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	Timestamp  time.Time `json:"timestamp"`
	SourceIP   *string   `json:"sourceIp"`
	TargetHost *string   `json:"targetHost"`
	Method     *string   `json:"method"`
	Path       *string   `json:"path"`
	// HTTP method + path actually sent to upstream provider.
	// May differ from Method/Path on AI-gateway cross-format routes; same
	// as Method/Path for transparent compliance-proxy + agent traffic.
	TargetMethod *string `json:"targetMethod,omitempty"`
	TargetPath   *string `json:"targetPath,omitempty"`
	StatusCode   *int    `json:"statusCode"`
	LatencyMs    *int    `json:"latencyMs"`
	// Phase breakdown — populated by data-plane services on every traffic_event
	// row. NULL on historical rows where upstream_ttfb / hooks could not be
	// reconstructed. The UI computes `ourOverheadMs = latencyMs − upstream_total_ms`
	// at render time.
	UpstreamTtfbMs  *int `json:"upstreamTtfbMs,omitempty"`
	UpstreamTotalMs *int `json:"upstreamTotalMs,omitempty"`
	RequestHooksMs  *int `json:"requestHooksMs,omitempty"`
	ResponseHooksMs *int `json:"responseHooksMs,omitempty"`
	// Microsecond-precision hook aggregates (additive; siblings of the _ms fields).
	RequestHooksUs   *int            `json:"requestHooksUs,omitempty"`
	ResponseHooksUs  *int            `json:"responseHooksUs,omitempty"`
	LatencyBreakdown json.RawMessage `json:"latencyBreakdown,omitempty"`
	// Request tracing
	TraceID           *string `json:"traceId,omitempty"`
	ExternalRequestID *string `json:"externalRequestId,omitempty"`
	// Caller-declared correlation tags. EndUserID is the caller's own
	// customer id (X-Nexus-End-User-Id header or protocol-native user
	// field); SessionID groups a conversation's requests
	// (X-Nexus-Session-Id header only). Both opaque, VK-scoped, never
	// validated or joined to Nexus identities. NULL for compliance-proxy
	// and agent rows.
	EndUserID *string `json:"endUserId,omitempty"`
	SessionID *string `json:"sessionId,omitempty"`
	// Entity attribution
	EntityType *string `json:"entityType,omitempty"` // "user" | "project" (unclassified rows store empty)
	EntityID   *string `json:"entityId,omitempty"`
	EntityName *string `json:"entityName,omitempty"`
	OrgID      *string `json:"orgId,omitempty"`
	OrgName    *string `json:"orgName,omitempty"`
	// Structured identity snapshot
	Identity json.RawMessage `json:"identity,omitempty"`
	// AI/Provider (ID + denormalized name)
	ProviderID       *string  `json:"providerId"`
	ProviderName     *string  `json:"providerName"`
	ModelID          *string  `json:"modelId"`
	ModelName        *string  `json:"modelName"`
	PromptTokens     *int     `json:"promptTokens"`
	CompletionTokens *int     `json:"completionTokens"`
	TotalTokens      *int     `json:"totalTokens"`
	EstimatedCostUsd *float64 `json:"estimatedCostUsd"`
	// Reasoning token metrics — already included in CompletionTokens / EstimatedCostUsd;
	// surfaced separately so the Traffic Audit Drawer can render a "thinking ratio" row.
	ReasoningTokens  *int     `json:"reasoningTokens,omitempty"`
	ReasoningCostUsd *float64 `json:"reasoningCostUsd,omitempty"`
	CacheStatus      *string  `json:"cacheStatus"`
	// Cache detail breakdown: CacheStatus unifies HIT/MISS for filters;
	// these four fields expose the gateway vs provider split surfaced in the
	// audit drawer's CACHE block.
	GatewayCacheStatus     *string `json:"gatewayCacheStatus,omitempty"`
	GatewayCacheSkipReason *string `json:"gatewayCacheSkipReason,omitempty"`
	GatewayCacheKind       *string `json:"gatewayCacheKind,omitempty"`
	// GatewayCacheL2EntryKey is the Redis HASH key of the L2 semantic-cache
	// entry that served the row, format "<redis_index_name>:<sha256(EmbeddingInput)[:16]>".
	// Stamped only when GatewayCacheKind == "semantic"; NULL elsewhere. The
	// audit drawer's "Mark as bad cache hit" action posts this as the poison-list
	// entryKey so the gateway's IsPoisoned check fires on the next FT.SEARCH hit.
	GatewayCacheL2EntryKey *string `json:"gatewayCacheL2EntryKey,omitempty"`
	ProviderCacheStatus    *string `json:"providerCacheStatus,omitempty"`
	// Multimodal audit stamps. ArtifactRefs: JSON-encoded array of artifact
	// references ([{"sha256","sizeBytes","mime"}] byte-bearing, [{"url"}]
	// URL-return — never dereferenced). ComplianceCoverage: request-time
	// record of what scanning actually ran ("prompt-only"/"none"); drives
	// the per-modality coverage badge. Both NULL for non-multimodal rows.
	ArtifactRefs       *string `json:"artifactRefs,omitempty"`
	ComplianceCoverage *string `json:"complianceCoverage,omitempty"`
	// EndpointType is the request modality (chat / image_generation / tts /
	// stt / …) — surfaced as the Traffic list's modality column and filter.
	EndpointType *string `json:"endpointType,omitempty"`
	// Prompt-cache metrics
	CacheCreationTokens    *int     `json:"cacheCreationTokens,omitempty"`
	CacheReadTokens        *int     `json:"cacheReadTokens,omitempty"`
	NormalizedStripCount   *int     `json:"normalizedStripCount,omitempty"`
	NormalizedStripBytes   *int     `json:"normalizedStripBytes,omitempty"`
	CacheMarkerInjected    *int     `json:"cacheMarkerInjected,omitempty"`
	CacheWriteCostUsd      *float64 `json:"cacheWriteCostUsd,omitempty"`
	CacheReadSavingsUsd    *float64 `json:"cacheReadSavingsUsd,omitempty"`
	CacheNetSavingsUsd     *float64 `json:"cacheNetSavingsUsd,omitempty"`
	GatewayCacheSavingsUsd *float64 `json:"gatewayCacheSavingsUsd,omitempty"`
	// Internal-ops cost columns. embeddingCostUsd: L2 lookup's embedding call
	// cost. embeddingModelId: FK to the embedding Model. aiGuardCostUsd: ai-guard
	// classifier LLM call cost (set only on rows where internal_purpose='ai-guard').
	// internalOpsBreakdown: catch-all for hook-type model calls. All NULL on rows
	// that did not trigger the corresponding internal call.
	EmbeddingCostUsd     *float64        `json:"embeddingCostUsd,omitempty"`
	EmbeddingModelID     *string         `json:"embeddingModelId,omitempty"`
	AIGuardCostUsd       *float64        `json:"aiGuardCostUsd,omitempty"`
	InternalOpsBreakdown json.RawMessage `json:"internalOpsBreakdown,omitempty"`
	// Cost-transparency surface: model pricing at drawer-fetch time
	// (LEFT JOIN against Model via routed_model_id). NULL when the model
	// was deleted post-call or routed_model_id is null (passthrough).
	// UI uses these + the *_tokens columns to show the per-row cost breakdown.
	// Historical accuracy is best-effort — prices can drift between request
	// time and drawer view.
	ModelInputPricePerMillion            *float64 `json:"modelInputPricePerMillion,omitempty"`
	ModelOutputPricePerMillion           *float64 `json:"modelOutputPricePerMillion,omitempty"`
	ModelCachedInputReadPricePerMillion  *float64 `json:"modelCachedInputReadPricePerMillion,omitempty"`
	ModelCachedInputWritePricePerMillion *float64 `json:"modelCachedInputWritePricePerMillion,omitempty"`
	RoutedProviderID                     *string  `json:"routedProviderId"`
	RoutedProviderName                   *string  `json:"routedProviderName"`
	RoutedModelID                        *string  `json:"routedModelId"`
	RoutedModelName                      *string  `json:"routedModelName"`
	RoutingRuleID                        *string  `json:"routingRuleId"`
	RoutingRuleName                      *string  `json:"routingRuleName,omitempty"`
	// Compliance — dual pipeline. Each stage records its own decision, reason,
	// reason_code, hooks_pipeline JSONB, and blocking_rule JSONB. A nil pointer
	// / nil JSONB means SQL NULL — the corresponding stage did not run.
	RequestHookDecision    *string         `json:"requestHookDecision"`
	RequestHookReason      *string         `json:"requestHookReason"`
	RequestHookReasonCode  *string         `json:"requestHookReasonCode"`
	RequestBlockingRule    json.RawMessage `json:"requestBlockingRule,omitempty"`
	ResponseHookDecision   *string         `json:"responseHookDecision"`
	ResponseHookReason     *string         `json:"responseHookReason"`
	ResponseHookReasonCode *string         `json:"responseHookReasonCode"`
	ResponseBlockingRule   json.RawMessage `json:"responseBlockingRule,omitempty"`
	ComplianceTags         []string        `json:"complianceTags"`
	BumpStatus             *string         `json:"bumpStatus"`
	// LLM signal extraction
	APIKeyClass           *string `json:"apiKeyClass,omitempty"`
	APIKeyFingerprint     *string `json:"apiKeyFingerprint,omitempty"`
	UsageExtractionStatus *string `json:"usageExtractionStatus,omitempty"`
	// Failure-reason classification. Populated by data-plane writers when the
	// producer classified a non-2xx outcome. Both NULL on success and on raw
	// upstream pass-through.
	ErrorCode   *string `json:"errorCode,omitempty"`
	ErrorReason *string `json:"errorReason,omitempty"`
	// Device / node attribution — set by agent and compliance-proxy when the
	// traffic originates from an identified device (thing).
	ThingID   *string `json:"thingId,omitempty"`
	ThingName *string `json:"thingName,omitempty"`
	// Agent attestation passthrough — populated by compliance-proxy when the
	// inbound CONNECT carried a verified X-Nexus-Attestation header and CP
	// transparently tunneled (skipping MITM + hooks). Both nil on regular MITM
	// rows so analytics can filter the attested slice without a JOIN.
	AttestationVerified *bool   `json:"attestationVerified,omitempty"`
	AttestationAgentID  *string `json:"attestationAgentId,omitempty"`
	// Agent-specific
	SourceProcess *string `json:"sourceProcess"`
	Action        *string `json:"action"`
	// JSONB — dual pipeline.
	RequestHooksPipeline  json.RawMessage `json:"requestHooksPipeline"`
	ResponseHooksPipeline json.RawMessage `json:"responseHooksPipeline"`
	RoutingTrace          json.RawMessage `json:"routingTrace"`
	Details               json.RawMessage `json:"details"`
	CreatedAt             time.Time       `json:"createdAt"`
	// Request / response body — populated only by the detail endpoint
	// (GetTrafficEvent) via JOIN to traffic_event_payload. Omitted from
	// list payloads to keep them light. Bodies are stored as raw bytes in a
	// BYTEA column tagged by the inline_*_encoding discriminator; the handler
	// decodes via the matching *BodyEncoding before rendering, then the UI renders them
	// generically (string, array, or object).
	RequestBody  json.RawMessage `json:"requestBody,omitempty"`
	ResponseBody json.RawMessage `json:"responseBody,omitempty"`
	// Storage encoding ("text" | "base64") of the inline body columns, used by
	// the handler to decode before rendering. Internal — never sent to the UI.
	RequestBodyEncoding  string `json:"-"`
	ResponseBodyEncoding string `json:"-"`
	// Spill refs. Non-NULL when the producer wrote the body out-of-band to a
	// SpillStore backend (large payloads ≥ inline threshold). The handler
	// resolves these to actual bytes via spillstore.Get and folds them into
	// RequestBody/ResponseBody before returning to the UI.
	RequestSpillRef  json.RawMessage `json:"requestSpillRef,omitempty"`
	ResponseSpillRef json.RawMessage `json:"responseSpillRef,omitempty"`
	// Truncation flags + the TRUE captured size, from traffic_event_payload.
	// A body is truncated when it reached the inline-vs-spill cutoff
	// (payload_capture.maxInlineBodyBytes) and no spill backend was configured
	// to take it out of band: the row then holds a PREFIX while *SizeBytes
	// still reports the real captured size (see spillstore.EmitBody). Without
	// both fields the UI renders that prefix as if it were the whole body — a
	// truncated SSE stream is indistinguishable from a response the model
	// never finished.
	RequestBodyTruncated  bool   `json:"requestBodyTruncated"`
	ResponseBodyTruncated bool   `json:"responseBodyTruncated"`
	RequestBodySizeBytes  *int64 `json:"requestBodySizeBytes,omitempty"`
	ResponseBodySizeBytes *int64 `json:"responseBodySizeBytes,omitempty"`
}

// TrafficEventListParams holds filter/pagination for traffic events.
type TrafficEventListParams struct {
	// DBSources restricts the query to these traffic_event.source values.
	// Handlers translate product domains (vk|proxy|agent) to the DB values
	// via the domain package. Empty slice = all data-plane sources.
	DBSources  []string
	Provider   string
	EntityID   string
	OrgID      string
	EntityType string // "user" | "project" (unclassified rows store empty)
	// ProjectID / VirtualKeyID select against the structured identity JSON —
	// traffic_event has no project_id / virtual_key_id columns because the
	// identity snapshot varies by source. Matches use
	// `identity->'project'->>'id'` and `identity->'vk'->>'id'`. NOTE:
	// `identity->'apiCredential'` is the UPSTREAM provider's API key,
	// NOT the client's Virtual Key — do not confuse the two.
	ProjectID    string
	VirtualKeyID string
	ModelUsed    string
	// ModelExact matches the SERVED model exactly (same
	// COALESCE(routed_model_name, model_name) expression the
	// error-governance grouping keys on) — the drill-down filter.
	// ModelUsed's substring match cannot express a class boundary
	// (matching "gpt-4o" must not include "gpt-4o-mini"). FilterNone
	// selects rows with no model at all.
	ModelExact string
	// EndpointType filters on the request modality
	// (traffic_event.endpoint_type: chat / embeddings / image_generation /
	// tts / stt / video_generation / rerank / guardrail / realtime / …).
	// Exact match; empty = all modalities.
	EndpointType string
	RequestID    string
	// EndUserID / SessionID filter on the caller-declared correlation
	// columns. Exact match; both ride the [end_user_id, timestamp] /
	// [session_id, timestamp] indexes so a correlation pivot stays cheap
	// at any table size.
	EndUserID            string
	SessionID            string
	HookDecision         string
	ResponseHookDecision string
	StatusCode           *int
	StatusRange          string // 2xx, 4xx, 5xx
	// CacheStatus filters on a.cache_status. Nil = no filter; non-nil
	// = exact match against one of the audit.CacheStatus* values
	// (HIT/HIT_LIVE/MISS/DISABLED/SKIP_NO_CACHE/PASSTHROUGH_SKIP).
	CacheStatus   *string
	TargetHost    string
	Path          string
	SourceProcess string
	BumpStatus    string
	// ComplianceTags filters traffic rows whose compliance_tags array
	// contains ALL of the supplied tag values. Empty slice = no tag
	// filter. Emitted as `AND compliance_tags @> $N::text[]`.
	ComplianceTags        []string
	APIKeyFingerprint     string // exact match on api_key_fingerprint
	UsageExtractionStatus string // exact match on usage_extraction_status
	// ExcludeInternal, when true, hides rows written by internal subsystems
	// (traffic_event.internal_purpose IS NOT NULL / non-empty). The admin
	// traffic handler defaults this to true so AI-Guard classify traffic
	// never leaks into customer billing / cost analytics views unless the
	// caller explicitly opts in via `?excludeInternal=false`.
	ExcludeInternal bool
	// ThingID filters by the device/node (thing) that originated the traffic.
	ThingID string
	// RoutingRuleID filters by the routing rule that was matched for this request.
	// Exact match on a.routing_rule_id.
	RoutingRuleID string
	// ErrorCode filters by the structured failure-reason classification stored
	// in a.error_code. Exact match on the raw stored value, which carries two
	// vocabularies: upper-case codes such as "PROVIDER_UNAVAILABLE" for
	// failures the gateway decided, and the provider's own lower_snake cause
	// such as "auth_failed" for a terminal upstream 4xx. ("no_compatible_
	// capability"-style reason codes live on request_hook_reason_code, not
	// here.) See the error_code comment in schema/traffic.prisma for the full
	// vocabulary and which producer writes which.
	ErrorCode string
	StartTime *time.Time
	EndTime   *time.Time
	Limit     int
	Offset    int
}

// ErrorCodeUnclassified is the `?errorCode=` sentinel selecting rows whose
// producer did not classify the failure (error_code NULL or empty) — the
// error-governance view's "(unclassified)" classes. Double-underscore
// delimiters keep it collision-free with both real error_code vocabularies
// (SCREAMING_SNAKE gateway codes and lower_snake provider causes).
const ErrorCodeUnclassified = "__unclassified__"

// FilterNone is the `?provider=` / `?modelExact=` sentinel selecting rows
// where the dimension is absent (both the routed and requested columns NULL
// or empty) — early rejections that never resolved a provider/model. The
// error-governance drill-down needs it because a class keyed on an empty
// provider/model would otherwise match every value of that dimension.
const FilterNone = "__none__"

// trafficEventSelectColumns is the canonical SELECT column list for traffic events.
const trafficEventSelectColumns = `
	a.id, a.source, a.timestamp,
	a.source_ip, a.target_host, a.method, a.path,
	a.target_method, a.target_path,
	a.status_code, a.latency_ms,
	a.upstream_ttfb_ms, a.upstream_total_ms,
	a.request_hooks_ms, a.response_hooks_ms,
	a.request_hooks_us, a.response_hooks_us,
	a.latency_breakdown,
	a.trace_id, a.external_request_id,
	a.end_user_id, a.session_id,
	a.entity_type, a.entity_id, a.entity_name,
	a.org_id, a.org_name, a.identity,
	a.provider_id, a.provider_name,
	a.model_id, a.model_name,
	a.prompt_tokens, a.completion_tokens, a.total_tokens,
	a.reasoning_tokens, a.reasoning_cost_usd,
	a.estimated_cost_usd, a.cache_status,
	a.gateway_cache_status, a.gateway_cache_skip_reason, a.gateway_cache_kind,
	a.gateway_cache_l2_entry_key,
	a.provider_cache_status, a.gateway_cache_savings_usd,
	a.artifact_refs, a.compliance_coverage, a.endpoint_type,
	a.routed_provider_id, a.routed_provider_name,
	a.routed_model_id, a.routed_model_name,
	a.routing_rule_id, a.routing_rule_name,
	a.request_hook_decision, a.request_hook_reason, a.request_hook_reason_code,
	a.request_blocking_rule,
	a.response_hook_decision, a.response_hook_reason, a.response_hook_reason_code,
	a.response_blocking_rule,
	a.compliance_tags, a.bump_status,
	a.api_key_class, a.api_key_fingerprint, a.usage_extraction_status,
	a.source_process, a.action,
	a.request_hooks_pipeline, a.response_hooks_pipeline,
	a.routing_trace, a.details, a.created_at,
	a.error_code, a.error_reason,
	a.cache_creation_tokens, a.cache_read_tokens,
	a.normalized_strip_count, a.normalized_strip_bytes, a.cache_marker_injected,
	a.cache_write_cost_usd, a.cache_read_savings_usd, a.cache_net_savings_usd,
	a.thing_id, a.thing_name,
	a.attestation_verified, a.attestation_agent_id,
	a.embedding_cost_usd, a.embedding_model_id,
	a.ai_guard_cost_usd, a.internal_ops_breakdown,
	-- Per-million pricing the drawer uses to render the cost breakdown.
	-- Cast NUMERIC(65,30) to float8 so the pgx Scan targets *float64
	-- without overflow checks.
	m."inputPricePerMillion"::float8       AS model_input_price_per_m,
	m."outputPricePerMillion"::float8      AS model_output_price_per_m,
	m."cachedInputReadPricePerMillion"::float8  AS model_cached_in_read_price_per_m,
	m."cachedInputWritePricePerMillion"::float8 AS model_cached_in_write_price_per_m`

// trafficEventFromClause is the canonical FROM clause for traffic events.
// LEFT JOIN Model so the drawer can show per-million pricing alongside the
// stamped cost — best-effort historical (prices may have drifted post-call).
const trafficEventFromClause = `
	FROM traffic_event a
	LEFT JOIN "Model" m ON m.id = a.routed_model_id`

// ListTrafficEvents returns traffic events with filtering.
func (store *Store) ListTrafficEvents(ctx context.Context, p TrafficEventListParams) ([]TrafficEvent, int, error) {
	where, args, argIdx := buildTrafficEventWhere(p)

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM traffic_event a %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count traffic events: %w", err)
	}

	q := fmt.Sprintf(`SELECT %s %s %s ORDER BY a.timestamp DESC, a.id DESC LIMIT $%d OFFSET $%d`,
		trafficEventSelectColumns, trafficEventFromClause, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list traffic events: %w", err)
	}
	defer rows.Close()

	events, _, err := scanTrafficEventRows(rows)
	if err != nil {
		return nil, 0, err
	}
	return events, total, nil
}

// GetTrafficEvent returns a single traffic event by its ID (any source).
// LEFT JOINs traffic_event_payload so the response includes request_body
// and response_body when the data plane captured them. List endpoints
// deliberately skip the JOIN to keep payloads light; only detail does.
func (store *Store) GetTrafficEvent(ctx context.Context, id string) (*TrafficEvent, error) {
	q := fmt.Sprintf(
		`SELECT %s, p.inline_request_body, p.inline_response_body, p.request_spill_ref, p.response_spill_ref,
		        COALESCE(p.inline_request_encoding, ''), COALESCE(p.inline_response_encoding, ''),
		        COALESCE(p.request_truncated, false), COALESCE(p.response_truncated, false),
		        p.request_size_bytes, p.response_size_bytes
		 %s
		 LEFT JOIN traffic_event_payload p ON p.traffic_event_id = a.id
		 WHERE a.id = $1`,
		trafficEventSelectColumns, trafficEventFromClause,
	)
	row := store.pool.QueryRow(ctx, q, id)

	var a TrafficEvent
	err := row.Scan(
		&a.ID, &a.Source, &a.Timestamp,
		&a.SourceIP, &a.TargetHost, &a.Method, &a.Path,
		&a.TargetMethod, &a.TargetPath,
		&a.StatusCode, &a.LatencyMs,
		&a.UpstreamTtfbMs, &a.UpstreamTotalMs,
		&a.RequestHooksMs, &a.ResponseHooksMs,
		&a.RequestHooksUs, &a.ResponseHooksUs,
		&a.LatencyBreakdown,
		&a.TraceID, &a.ExternalRequestID,
		&a.EndUserID, &a.SessionID,
		&a.EntityType, &a.EntityID, &a.EntityName,
		&a.OrgID, &a.OrgName, &a.Identity,
		&a.ProviderID, &a.ProviderName,
		&a.ModelID, &a.ModelName,
		&a.PromptTokens, &a.CompletionTokens, &a.TotalTokens,
		&a.ReasoningTokens, &a.ReasoningCostUsd,
		&a.EstimatedCostUsd, &a.CacheStatus,
		&a.GatewayCacheStatus, &a.GatewayCacheSkipReason, &a.GatewayCacheKind,
		&a.GatewayCacheL2EntryKey,
		&a.ProviderCacheStatus, &a.GatewayCacheSavingsUsd,
		&a.ArtifactRefs, &a.ComplianceCoverage, &a.EndpointType,
		&a.RoutedProviderID, &a.RoutedProviderName,
		&a.RoutedModelID, &a.RoutedModelName,
		&a.RoutingRuleID, &a.RoutingRuleName,
		&a.RequestHookDecision, &a.RequestHookReason, &a.RequestHookReasonCode,
		&a.RequestBlockingRule,
		&a.ResponseHookDecision, &a.ResponseHookReason, &a.ResponseHookReasonCode,
		&a.ResponseBlockingRule,
		&a.ComplianceTags, &a.BumpStatus,
		&a.APIKeyClass, &a.APIKeyFingerprint, &a.UsageExtractionStatus,
		&a.SourceProcess, &a.Action,
		&a.RequestHooksPipeline, &a.ResponseHooksPipeline,
		&a.RoutingTrace, &a.Details, &a.CreatedAt,
		&a.ErrorCode, &a.ErrorReason,
		&a.CacheCreationTokens, &a.CacheReadTokens,
		&a.NormalizedStripCount, &a.NormalizedStripBytes, &a.CacheMarkerInjected,
		&a.CacheWriteCostUsd, &a.CacheReadSavingsUsd, &a.CacheNetSavingsUsd,
		&a.ThingID, &a.ThingName,
		&a.AttestationVerified, &a.AttestationAgentID,
		&a.EmbeddingCostUsd, &a.EmbeddingModelID,
		&a.AIGuardCostUsd, &a.InternalOpsBreakdown,
		&a.ModelInputPricePerMillion, &a.ModelOutputPricePerMillion,
		&a.ModelCachedInputReadPricePerMillion, &a.ModelCachedInputWritePricePerMillion,
		&a.RequestBody, &a.ResponseBody,
		&a.RequestSpillRef, &a.ResponseSpillRef,
		&a.RequestBodyEncoding, &a.ResponseBodyEncoding,
		&a.RequestBodyTruncated, &a.ResponseBodyTruncated,
		&a.RequestBodySizeBytes, &a.ResponseBodySizeBytes,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get traffic event: %w", err)
	}
	return &a, nil
}

func scanOneTrafficEvent(row interface{ Scan(dest ...any) error }, a *TrafficEvent) error {
	return row.Scan(
		&a.ID, &a.Source, &a.Timestamp,
		&a.SourceIP, &a.TargetHost, &a.Method, &a.Path,
		&a.TargetMethod, &a.TargetPath,
		&a.StatusCode, &a.LatencyMs,
		&a.UpstreamTtfbMs, &a.UpstreamTotalMs,
		&a.RequestHooksMs, &a.ResponseHooksMs,
		&a.RequestHooksUs, &a.ResponseHooksUs,
		&a.LatencyBreakdown,
		&a.TraceID, &a.ExternalRequestID,
		&a.EndUserID, &a.SessionID,
		&a.EntityType, &a.EntityID, &a.EntityName,
		&a.OrgID, &a.OrgName, &a.Identity,
		&a.ProviderID, &a.ProviderName,
		&a.ModelID, &a.ModelName,
		&a.PromptTokens, &a.CompletionTokens, &a.TotalTokens,
		&a.ReasoningTokens, &a.ReasoningCostUsd,
		&a.EstimatedCostUsd, &a.CacheStatus,
		&a.GatewayCacheStatus, &a.GatewayCacheSkipReason, &a.GatewayCacheKind,
		&a.GatewayCacheL2EntryKey,
		&a.ProviderCacheStatus, &a.GatewayCacheSavingsUsd,
		&a.ArtifactRefs, &a.ComplianceCoverage, &a.EndpointType,
		&a.RoutedProviderID, &a.RoutedProviderName,
		&a.RoutedModelID, &a.RoutedModelName,
		&a.RoutingRuleID, &a.RoutingRuleName,
		&a.RequestHookDecision, &a.RequestHookReason, &a.RequestHookReasonCode,
		&a.RequestBlockingRule,
		&a.ResponseHookDecision, &a.ResponseHookReason, &a.ResponseHookReasonCode,
		&a.ResponseBlockingRule,
		&a.ComplianceTags, &a.BumpStatus,
		&a.APIKeyClass, &a.APIKeyFingerprint, &a.UsageExtractionStatus,
		&a.SourceProcess, &a.Action,
		&a.RequestHooksPipeline, &a.ResponseHooksPipeline,
		&a.RoutingTrace, &a.Details, &a.CreatedAt,
		&a.ErrorCode, &a.ErrorReason,
		&a.CacheCreationTokens, &a.CacheReadTokens,
		&a.NormalizedStripCount, &a.NormalizedStripBytes, &a.CacheMarkerInjected,
		&a.CacheWriteCostUsd, &a.CacheReadSavingsUsd, &a.CacheNetSavingsUsd,
		&a.ThingID, &a.ThingName,
		&a.AttestationVerified, &a.AttestationAgentID,
		&a.EmbeddingCostUsd, &a.EmbeddingModelID,
		&a.AIGuardCostUsd, &a.InternalOpsBreakdown,
		&a.ModelInputPricePerMillion, &a.ModelOutputPricePerMillion,
		&a.ModelCachedInputReadPricePerMillion, &a.ModelCachedInputWritePricePerMillion,
	)
}

func scanTrafficEventRows(rows pgx.Rows) ([]TrafficEvent, int, error) {
	events := []TrafficEvent{}
	for rows.Next() {
		var a TrafficEvent
		if err := scanOneTrafficEvent(rows, &a); err != nil {
			return nil, 0, fmt.Errorf("scan traffic event: %w", err)
		}
		events = append(events, a)
	}
	return events, len(events), rows.Err()
}
