package trafficstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/domain"
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
	UpstreamTtfbMs   *int            `json:"upstreamTtfbMs,omitempty"`
	UpstreamTotalMs  *int            `json:"upstreamTotalMs,omitempty"`
	RequestHooksMs   *int            `json:"requestHooksMs,omitempty"`
	ResponseHooksMs  *int            `json:"responseHooksMs,omitempty"`
	LatencyBreakdown json.RawMessage `json:"latencyBreakdown,omitempty"`
	// Request tracing
	TraceID           *string `json:"traceId,omitempty"`
	ExternalRequestID *string `json:"externalRequestId,omitempty"`
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
	// list payloads to keep them light. Bodies are stored as jsonb but
	// the UI renders them generically (string, array, or object).
	RequestBody  json.RawMessage `json:"requestBody,omitempty"`
	ResponseBody json.RawMessage `json:"responseBody,omitempty"`
	// Spill refs. Non-NULL when the producer wrote the body out-of-band to a
	// SpillStore backend (large payloads ≥ inline threshold). The handler
	// resolves these to actual bytes via spillstore.Get and folds them into
	// RequestBody/ResponseBody before returning to the UI.
	RequestSpillRef  json.RawMessage `json:"requestSpillRef,omitempty"`
	ResponseSpillRef json.RawMessage `json:"responseSpillRef,omitempty"`
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
	ProjectID            string
	VirtualKeyID         string
	ModelUsed            string
	RequestID            string
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
	// in a.error_code. Exact match on the raw stored value (upper-case codes
	// such as "PROVIDER_ERROR"; "no_compatible_capability"-style reason codes
	// live on request_hook_reason_code, not here).
	ErrorCode string
	StartTime *time.Time
	EndTime   *time.Time
	Limit     int
	Offset    int
}

// trafficEventSelectColumns is the canonical SELECT column list for traffic events.
const trafficEventSelectColumns = `
	a.id, a.source, a.timestamp,
	a.source_ip, a.target_host, a.method, a.path,
	a.target_method, a.target_path,
	a.status_code, a.latency_ms,
	a.upstream_ttfb_ms, a.upstream_total_ms,
	a.request_hooks_ms, a.response_hooks_ms,
	a.latency_breakdown,
	a.trace_id, a.external_request_id,
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
		`SELECT %s, p.inline_request_body, p.inline_response_body, p.request_spill_ref, p.response_spill_ref
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
		&a.LatencyBreakdown,
		&a.TraceID, &a.ExternalRequestID,
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
		&a.LatencyBreakdown,
		&a.TraceID, &a.ExternalRequestID,
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

func buildTrafficEventWhere(p TrafficEventListParams) (string, []any, int) {
	args := []any{}
	argIdx := 1

	// Source filter — handler supplies DB values already mapped from UI domain.
	// Empty = all data-plane sources (every CHECK-allowed value).
	sources := p.DBSources
	if len(sources) == 0 {
		sources = domain.AllDataPlaneDBSources()
	}
	placeholders := make([]string, len(sources))
	for i, s := range sources {
		placeholders[i] = fmt.Sprintf("$%d", argIdx)
		args = append(args, s)
		argIdx++
	}
	where := fmt.Sprintf("WHERE a.source IN (%s)", strings.Join(placeholders, ","))

	if p.Provider != "" {
		// Filter by the provider that actually SERVED the request (routed),
		// falling back to the requested provider for non-ai-gateway rows. The
		// requested provider_name is NULL for OpenAI-style / "auto" traffic, so
		// matching it would drop exactly the rows the served provider handled —
		// and it would disagree with the analytics layer, which attributes by
		// routed_provider.
		where += fmt.Sprintf(` AND COALESCE(a.routed_provider_name, a.provider_name) = $%d`, argIdx)
		args = append(args, p.Provider)
		argIdx++
	}
	if p.EntityID != "" {
		where += fmt.Sprintf(` AND a.entity_id = $%d`, argIdx)
		args = append(args, p.EntityID)
		argIdx++
	}
	if p.OrgID != "" {
		where += fmt.Sprintf(` AND a.org_id = $%d`, argIdx)
		args = append(args, p.OrgID)
		argIdx++
	}
	if p.EntityType != "" {
		where += fmt.Sprintf(` AND a.entity_type = $%d`, argIdx)
		args = append(args, p.EntityType)
		argIdx++
	}
	if p.ProjectID != "" {
		where += fmt.Sprintf(` AND a.identity->'project'->>'id' = $%d`, argIdx)
		args = append(args, p.ProjectID)
		argIdx++
	}
	if p.VirtualKeyID != "" {
		// identity.vk.id is the Virtual Key the client presented (what the
		// `?virtualKeyId=` filter is meant to match). identity.apiCredential
		// is something else entirely — the upstream provider's API key
		// Nexus used to make the upstream call (real OpenAI / Anthropic
		// token, totally different identifier). Producers renamed the
		// VK key from "credential" to "vk" in an earlier sprint (see
		// ai-gateway audit_test.go assertion 'Identity.credential should
		// not exist — renamed to identity.vk'); this query was missed in
		// that rename, so filtering by virtualKeyId silently returned
		// zero rows. Fix: query the right JSON path.
		where += fmt.Sprintf(` AND a.identity->'vk'->>'id' = $%d`, argIdx)
		args = append(args, p.VirtualKeyID)
		argIdx++
	}
	if p.ModelUsed != "" {
		// Match the served model first (what cost/usage attribute to), falling
		// back to the requested literal so a search still finds rows where
		// routing did not substitute.
		where += fmt.Sprintf(` AND COALESCE(a.routed_model_name, a.model_name) ILIKE $%d`, argIdx)
		args = append(args, "%"+escapeILIKE(p.ModelUsed)+"%")
		argIdx++
	}
	if p.RequestID != "" {
		where += fmt.Sprintf(` AND a.id = $%d`, argIdx)
		args = append(args, p.RequestID)
		argIdx++
	}
	if p.HookDecision != "" {
		where += fmt.Sprintf(` AND a.request_hook_decision = $%d`, argIdx)
		args = append(args, p.HookDecision)
		argIdx++
	}
	if p.ResponseHookDecision != "" {
		where += fmt.Sprintf(` AND a.response_hook_decision = $%d`, argIdx)
		args = append(args, p.ResponseHookDecision)
		argIdx++
	}
	if p.StatusCode != nil {
		where += fmt.Sprintf(` AND a.status_code = $%d`, argIdx)
		args = append(args, *p.StatusCode)
		argIdx++
	} else if p.StatusRange != "" {
		switch p.StatusRange {
		case "2xx":
			where += ` AND a.status_code >= 200 AND a.status_code <= 299`
		case "4xx":
			where += ` AND a.status_code >= 400 AND a.status_code <= 499`
		case "5xx":
			where += ` AND a.status_code >= 500 AND a.status_code <= 599`
		}
	}
	if p.CacheStatus != nil {
		where += fmt.Sprintf(` AND a.cache_status = $%d`, argIdx)
		args = append(args, *p.CacheStatus)
		argIdx++
	}
	if p.TargetHost != "" {
		where += fmt.Sprintf(` AND a.target_host ILIKE $%d`, argIdx)
		args = append(args, "%"+escapeILIKE(p.TargetHost)+"%")
		argIdx++
	}
	if p.Path != "" {
		where += fmt.Sprintf(` AND a.path ILIKE $%d`, argIdx)
		args = append(args, "%"+escapeILIKE(p.Path)+"%")
		argIdx++
	}
	if p.SourceProcess != "" {
		where += fmt.Sprintf(` AND a.source_process ILIKE $%d`, argIdx)
		args = append(args, "%"+escapeILIKE(p.SourceProcess)+"%")
		argIdx++
	}
	if p.BumpStatus != "" {
		where += fmt.Sprintf(` AND a.bump_status = $%d`, argIdx)
		args = append(args, p.BumpStatus)
		argIdx++
	}
	if len(p.ComplianceTags) > 0 {
		// compliance_tags @> $N::text[] matches rows whose tag array
		// contains every supplied tag. Callers pass repeated `?tag=...`
		// query params — the filter behaves as AND across tags, so a
		// row must carry all of them to match.
		where += fmt.Sprintf(` AND a.compliance_tags @> $%d::text[]`, argIdx)
		args = append(args, p.ComplianceTags)
		argIdx++
	}
	if p.APIKeyFingerprint != "" {
		where += fmt.Sprintf(` AND a.api_key_fingerprint = $%d`, argIdx)
		args = append(args, p.APIKeyFingerprint)
		argIdx++
	}
	if p.UsageExtractionStatus != "" {
		where += fmt.Sprintf(` AND a.usage_extraction_status = $%d`, argIdx)
		args = append(args, p.UsageExtractionStatus)
		argIdx++
	}
	if p.ThingID != "" {
		where += fmt.Sprintf(` AND a.thing_id = $%d`, argIdx)
		args = append(args, p.ThingID)
		argIdx++
	}
	if p.RoutingRuleID != "" {
		where += fmt.Sprintf(` AND a.routing_rule_id = $%d`, argIdx)
		args = append(args, p.RoutingRuleID)
		argIdx++
	}
	if p.ErrorCode != "" {
		where += fmt.Sprintf(` AND a.error_code = $%d`, argIdx)
		args = append(args, p.ErrorCode)
		argIdx++
	}
	if p.ExcludeInternal {
		// internal_purpose is nullable; treat empty strings as "not internal"
		// too so a buggy producer that sends '' instead of omitting the field
		// still routes into the customer view.
		where += ` AND (a.internal_purpose IS NULL OR a.internal_purpose = '')`
	}
	if p.StartTime != nil {
		where += fmt.Sprintf(` AND a.timestamp >= $%d`, argIdx)
		args = append(args, *p.StartTime)
		argIdx++
	}
	if p.EndTime != nil {
		where += fmt.Sprintf(` AND a.timestamp <= $%d`, argIdx)
		args = append(args, *p.EndTime)
		argIdx++
	}

	return where, args, argIdx
}
