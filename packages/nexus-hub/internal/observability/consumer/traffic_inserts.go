package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (w *TrafficEventWriter) insertTrafficEvents(ctx context.Context, tx pgx.Tx, items []pendingTrafficMessage) error {
	batch := &pgx.Batch{}
	for _, pm := range items {
		e := pm.event
		// traffic_event.compliance_tags is NOT NULL DEFAULT ARRAY[]::TEXT[];
		// pgx encodes a nil []string as SQL NULL which would violate the
		// constraint, so promote absent tag sets to an empty array here.
		tags := e.ComplianceTags
		if tags == nil {
			tags = []string{}
		}
		// Strip PostgreSQL-illegal null bytes (\x00) from all string and JSON
		// fields. Providers like ChatGPT can include null bytes in SSE responses
		// that propagate into path/reason/details fields (SQLSTATE 22021).
		for i, t := range tags {
			tags[i] = stripNul(t)
		}
		batch.Queue(insertTrafficEventSQL,
			stripNul(e.ID), stripNul(e.Source), stripNulPtr(e.TraceID), stripNulPtr(e.ExternalRequestID), e.Timestamp,
			stripNulPtr(e.SourceIP), stripNulPtr(e.TargetHost), stripNulPtr(e.Method), stripNulPtr(e.Path), e.StatusCode, e.LatencyMs,
			stripNulPtr(e.EntityType), stripNulPtr(e.EntityID), stripNulPtr(e.EntityName), stripNulPtr(e.OrgID), stripNulPtr(e.OrgName),
			nullableJSON(stripNulJSON(e.Identity)),
			stripNulPtr(e.ProviderID), stripNulPtr(e.ProviderName), stripNulPtr(e.ModelID), stripNulPtr(e.ModelName),
			e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.EstimatedCostUSD,
			stripNulPtr(e.CacheStatus),
			stripNulPtr(e.RoutedProviderID), stripNulPtr(e.RoutedProviderName), stripNulPtr(e.RoutedModelID), stripNulPtr(e.RoutedModelName),
			stripNulPtr(e.RoutingRuleID), stripNulPtr(e.RoutingRuleName),
			stripNulPtr(e.RequestHookDecision), stripNulPtr(e.RequestHookReason), stripNulPtr(e.RequestHookReasonCode),
			stripNulPtr(e.ResponseHookDecision), stripNulPtr(e.ResponseHookReason), stripNulPtr(e.ResponseHookReasonCode),
			tags, stripNulPtr(e.BumpStatus),
			stripNulPtr(e.APIKeyClass), stripNulPtr(e.APIKeyFingerprint), stripNulPtr(e.UsageExtractionStatus),
			stripNulPtr(e.SourceProcess), stripNulPtr(e.Action),
			nullableJSON(stripNulJSON(e.RequestHooksPipeline)), nullableJSON(stripNulJSON(e.ResponseHooksPipeline)),
			nullableJSON(stripNulJSON(e.RoutingTrace)), nullableJSON(stripNulJSON(e.Details)),
			stripNulPtr(e.InternalPurpose),
			nullableJSON(stripNulJSON(e.RequestBlockingRule)), nullableJSON(stripNulJSON(e.ResponseBlockingRule)),
			stripNulPtr(e.OriginTZ),
			stripNulPtr(e.ErrorCode), stripNulPtr(e.ErrorReason),
			// Prompt cache metrics ($56–$64).
			e.CacheCreationTokens, e.CacheReadTokens,
			e.NormalizedStripCount, e.NormalizedStripBytes, e.CacheMarkerInjected,
			e.CacheWriteCostUsd, e.CacheReadSavingsUsd, e.CacheNetSavingsUsd,
			e.GatewayCacheSavingsUsd,
			// Thing attribution ($65–$66).
			stripNulPtr(e.ThingID), stripNulPtr(e.ThingName),
			// Provider credential attribution ($67).
			stripNulPtr(e.CredentialID),
			// Passthrough audit ($68, $69). PassthroughFlags is []string
			// (PostgreSQL text[]); pgx binds the slice natively. Nil slice →
			// SQL NULL; audit Writer only populates when AnyBypassActive
			// (non-empty by contract).
			passthroughFlagsParam(e.PassthroughFlags),
			passthroughReasonParam(e.PassthroughReason),
			// Latency phase breakdown ($70-$74). Producers omit fields they did
			// not measure; passing the *int pointer binds SQL NULL on nil.
			e.UpstreamTtfbMs, e.UpstreamTotalMs,
			e.RequestHooksMs, e.ResponseHooksMs,
			nullableJSON(stripNulJSON(e.LatencyBreakdown)),
			// reasoning_tokens ($75). Older publishers omit this field (writes
			// 0); for true NULL semantics query WHERE reasoning_tokens > 0 or
			// IS NOT NULL.
			e.ReasoningTokens,
			// reasoning_cost_usd ($76). Cost subset for reasoning_tokens; 0
			// when reasoning is 0.
			e.ReasoningCostUsd,
			// target_method + target_path ($77, $78). What was actually sent
			// to upstream (differs from method/path when AI gateway re-routes).
			stripNulPtr(e.TargetMethod), stripNulPtr(e.TargetPath),
			// Cache detail columns ($79-$82). Unified cache_status is already
			// stamped at $26. These four columns are detail-only; the audit drawer
			// renders them via the three layouts in cost-estimation-architecture.md § 6.4.
			stripNulPtr(e.GatewayCacheStatus),
			stripNulPtr(e.GatewayCacheSkipReason),
			stripNulPtr(e.GatewayCacheKind),
			stripNulPtr(e.ProviderCacheStatus),
			// Agent attestation passthrough ($83, $84). Both NULL on regular
			// MITM rows so analytics can filter the attested slice without a
			// JOIN. Producer is compliance-proxy's EmitAttestationPassthrough.
			e.AttestationVerified,
			stripNulPtr(e.AttestationAgentID),
			// Internal-ops cost: embedding_cost_usd + embedding_model_id
			// ($85, $86). Populated on every L2-touching row (both miss and
			// hit). Empty string EmbeddingModelID is stored as NULL.
			e.EmbeddingCostUsd,
			func() any {
				s := stripNul(e.EmbeddingModelID)
				if s == "" {
					return nil
				}
				return s
			}(),
			// AI-guard classifier cost ($87) + hook-cost breakdown JSONB ($88).
			// stripNulJSON guards this jsonb column against a raw \x00 byte
			// (SQLSTATE 22021) or the 6-char NUL escape (SQLSTATE 22P05) in the
			// gateway-internal cost payload. Without it this was the SOLE
			// un-stripped JSON column, so a poisoned breakdown aborted the whole
			// batch and the poison-pill path acked + dropped up to 100 events
			// permanently.
			e.AIGuardCostUsd,
			nullableJSON(stripNulJSON(e.InternalOpsBreakdown)),
			// L2 semantic-cache entry key ($89). Redis HASH key of the entry
			// that served the row; NULL on non-semantic-hit rows. Posted by the
			// audit drawer thumbs-down as the poison-list entryKey.
			func() any {
				s := stripNul(e.GatewayCacheL2EntryKey)
				if s == "" {
					return nil
				}
				return s
			}(),
			// endpoint_type ($90). Canonical typology.EndpointKind stamped by
			// the producer; '' for non-AI forwards. Column is NOT NULL DEFAULT
			// '', so bind the string directly (empty is valid, never NULL).
			stripNul(e.EndpointType),
		)
	}

	br := tx.SendBatch(ctx, batch)
	defer br.Close() //nolint:errcheck

	for range items {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("exec batch insert: %w", err)
		}
	}
	return nil
}

const insertTrafficEventSQL = `
INSERT INTO traffic_event (
    id, source, trace_id, external_request_id, timestamp,
    source_ip, target_host, method, path, status_code, latency_ms,
    entity_type, entity_id, entity_name, org_id, org_name,
    identity,
    provider_id, provider_name, model_id, model_name,
    prompt_tokens, completion_tokens, total_tokens, estimated_cost_usd,
    cache_status,
    routed_provider_id, routed_provider_name, routed_model_id, routed_model_name,
    routing_rule_id, routing_rule_name,
    request_hook_decision, request_hook_reason, request_hook_reason_code,
    response_hook_decision, response_hook_reason, response_hook_reason_code,
    compliance_tags, bump_status,
    api_key_class, api_key_fingerprint, usage_extraction_status,
    source_process, action,
    request_hooks_pipeline, response_hooks_pipeline,
    routing_trace, details,
    internal_purpose,
    request_blocking_rule, response_blocking_rule,
    origin_tz,
    error_code, error_reason,
    cache_creation_tokens, cache_read_tokens,
    normalized_strip_count, normalized_strip_bytes, cache_marker_injected,
    cache_write_cost_usd, cache_read_savings_usd, cache_net_savings_usd,
    gateway_cache_savings_usd,
    thing_id, thing_name,
    credential_id,
    passthrough_flags, passthrough_reason,
    upstream_ttfb_ms, upstream_total_ms,
    request_hooks_ms, response_hooks_ms,
    latency_breakdown,
    reasoning_tokens,
    reasoning_cost_usd,
    target_method, target_path,
    -- Cache detail columns. Unified cache_status is already at column 26.
    gateway_cache_status, gateway_cache_skip_reason, gateway_cache_kind, provider_cache_status,
    -- Agent attestation passthrough.
    attestation_verified, attestation_agent_id,
    -- Internal-ops cost: L2 lookup's embedding call cost + model id.
    embedding_cost_usd, embedding_model_id,
    -- Internal-ops cost: ai-guard classifier call cost + open-ended
    -- breakdown JSONB for future hook-type internal model calls.
    ai_guard_cost_usd, internal_ops_breakdown,
    -- Redis HASH key of the L2 semantic-cache entry that served the row.
    -- Stamped only when gateway_cache_kind='semantic'.
    -- Surfaces on the audit drawer "Mark as bad cache hit" thumbs-down so
    -- the poison-list POST carries the key the Reader's IsPoisoned check
    -- actually consults; before this column the UI posted traffic_event.id
    -- which never matched, making negative-feedback a silent no-op.
    gateway_cache_l2_entry_key,
    -- Canonical request modality (typology.EndpointKind), stamped by the
    -- producing service. '' for non-classified (compliance-proxy / agent).
    endpoint_type
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10, $11,
    $12, $13, $14, $15, $16,
    $17,
    $18, $19, $20, $21,
    $22, $23, $24, $25,
    $26,
    $27, $28, $29, $30,
    $31, $32,
    $33, $34, $35,
    $36, $37, $38,
    $39, $40,
    $41, $42, $43,
    $44, $45,
    $46, $47,
    $48, $49,
    $50,
    $51, $52,
    $53,
    $54, $55,
    $56, $57,
    $58, $59, $60,
    $61, $62, $63,
    $64,
    $65, $66,
    $67,
    $68, $69,
    $70, $71,
    $72, $73,
    $74,
    $75,
    $76,
    $77, $78,
    $79, $80, $81, $82,
    $83, $84,
    $85, $86,
    $87, $88,
    $89,
    $90
) ON CONFLICT (id) DO NOTHING
`

func (w *TrafficEventWriter) insertPayloads(ctx context.Context, tx pgx.Tx, items []pendingTrafficMessage) error {
	batch := &pgx.Batch{}
	count := 0

	for _, pm := range items {
		e := pm.event
		if e.RequestBody.Kind == "absent" && e.ResponseBody.Kind == "absent" {
			continue
		}
		// Demux Body discriminator into inline vs spill columns.
		// inline_*_body holds the JSONB body; *_spill_ref holds {backend,
		// key, size, sha256, contentType}. Exactly one is set per direction.
		var (
			inlineReq, inlineResp     any
			reqSpillRef, respSpillRef any
			reqSize, respSize         *int64
			reqTrunc, respTrunc       bool
			reqCT, respCT             *string
		)
		if e.RequestBody.Kind == "inline" {
			// Use json.Marshal(body) so the envelope {kind, encoding, inlineBytes, ...}
			// is stored as JSONB. InlineBytes may be non-JSON (SSE/binary) when
			// Encoding == "base64"; casting raw bytes to json.RawMessage would fail
			// PostgreSQL's JSONB validation.
			if b, err := json.Marshal(e.RequestBody); err == nil {
				inlineReq = json.RawMessage(b)
			}
			s := e.RequestBody.SizeBytes
			reqSize = &s
			reqTrunc = e.RequestBody.Truncated
			if e.RequestBody.ContentType != "" {
				ct := e.RequestBody.ContentType
				reqCT = &ct
			}
		} else if e.RequestBody.Kind == "spill" && e.RequestBody.SpillRef != nil {
			b, _ := json.Marshal(e.RequestBody.SpillRef)
			reqSpillRef = json.RawMessage(b)
			s := e.RequestBody.SpillRef.Size
			reqSize = &s
			reqTrunc = e.RequestBody.Truncated
			if e.RequestBody.ContentType != "" {
				ct := e.RequestBody.ContentType
				reqCT = &ct
			}
		}
		if e.ResponseBody.Kind == "inline" {
			// Same as request: marshal full envelope to avoid JSONB failures on
			// non-JSON bodies (streaming SSE, binary).
			if b, err := json.Marshal(e.ResponseBody); err == nil {
				inlineResp = json.RawMessage(b)
			}
			s := e.ResponseBody.SizeBytes
			respSize = &s
			respTrunc = e.ResponseBody.Truncated
			if e.ResponseBody.ContentType != "" {
				ct := e.ResponseBody.ContentType
				respCT = &ct
			}
		} else if e.ResponseBody.Kind == "spill" && e.ResponseBody.SpillRef != nil {
			b, _ := json.Marshal(e.ResponseBody.SpillRef)
			respSpillRef = json.RawMessage(b)
			s := e.ResponseBody.SpillRef.Size
			respSize = &s
			respTrunc = e.ResponseBody.Truncated
			if e.ResponseBody.ContentType != "" {
				ct := e.ResponseBody.ContentType
				respCT = &ct
			}
		}
		batch.Queue(insertPayloadSQL,
			e.ID,
			inlineReq, inlineResp,
			reqSpillRef, respSpillRef,
			reqSize, respSize,
			reqTrunc, respTrunc,
			reqCT, respCT,
		)
		count++
	}

	if count == 0 {
		return nil
	}

	br := tx.SendBatch(ctx, batch)
	defer br.Close() //nolint:errcheck

	for range count {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("exec payload insert: %w", err)
		}
	}
	return nil
}

const insertPayloadSQL = `
INSERT INTO traffic_event_payload (
    traffic_event_id,
    inline_request_body, inline_response_body,
    request_spill_ref, response_spill_ref,
    request_size_bytes, response_size_bytes,
    request_truncated, response_truncated,
    request_content_type, response_content_type
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (traffic_event_id) DO NOTHING
`

// passthroughFlagsParam binds a []string slice as a PostgreSQL text[]
// parameter. Nil / empty slice becomes SQL NULL so the partial index
// `traffic_event_passthrough_active_idx` stays compact.
// Each element is stripped of NUL bytes to avoid SQLSTATE 22021.
func passthroughFlagsParam(flags []string) any {
	if len(flags) == 0 {
		return nil
	}
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = stripNul(f)
	}
	return out
}

// passthroughReasonParam normalises an empty string to SQL NULL so the
// column stays NULL when no bypass fired (no operator reason on file).
// Non-empty values are NUL-byte-stripped.
func passthroughReasonParam(reason string) any {
	if reason == "" {
		return nil
	}
	s := stripNul(reason)
	return &s
}
