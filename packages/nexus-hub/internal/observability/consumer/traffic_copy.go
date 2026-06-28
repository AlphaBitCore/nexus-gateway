package consumer

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	json "github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"
)

// traffic_copy.go — the COPY-based batch insert fast path for traffic_event and
// traffic_event_payload. The audit drain is disk-WRITE-bandwidth bound on a
// single box; COPY streams a whole batch into a per-transaction staging temp
// table in one bulk operation (one parse, set-based insert, tighter WAL) instead
// of N pipelined parameterized INSERTs, then folds it into the real table with a
// single `INSERT … SELECT … ON CONFLICT DO NOTHING` so the NATS-redelivery
// idempotency contract is preserved (COPY itself cannot express ON CONFLICT).
//
// Relationship to the existing path: this does NOT change the 3-way-parallel,
// BatchSize-accumulated, duty-cycle-paced drain — it only replaces what runs
// INSIDE flushBatch. On ANY COPY-path error flushBatch returns the error and the
// caller (flush) falls back to per-item reprocessing on the proven pgx.Batch path
// (flushItem), so the poison-isolation + no-strand guarantee is untouched.
//
// The value builders (trafficEventRowValues / payloadRowValues) are shared with
// the pgx.Batch path in traffic_inserts.go so the two paths can never drift in
// column order or null-stripping.

// trafficCopyEnabled gates the COPY fast path. On by default: the COPY staging
// load is the disk-WRITE-bound drain's shipped optimum (set-based insert,
// tighter WAL, less server CPU), and any COPY-path error fails safe to the
// per-item pgx.Batch path (see flush), so default-on carries no
// poison-isolation risk. NEXUS_HUB_TRAFFIC_COPY=0/false/off/no forces the plain
// pgx.Batch path — the kill switch and the A/B control arm. Read once at start.
var trafficCopyEnabled = trafficCopyEnabledFromEnv(os.Getenv("NEXUS_HUB_TRAFFIC_COPY"))

// trafficCopyEnabledFromEnv resolves the COPY gate from a raw env value: empty
// (unset) defaults on; only an explicit falsey token disables it.
func trafficCopyEnabledFromEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// trafficEventColumns is the traffic_event column list in the exact order of
// trafficEventRowValues — the single source of truth for both the COPY staging
// load and the INSERT…SELECT projection. Must stay in lockstep with
// insertTrafficEventSQL's column list (guarded by TestTrafficEventColumnsParity).
var trafficEventColumns = []string{
	"id", "source", "trace_id", "external_request_id", "timestamp",
	"source_ip", "target_host", "method", "path", "status_code", "latency_ms",
	"entity_type", "entity_id", "entity_name", "org_id", "org_name",
	"identity",
	"provider_id", "provider_name", "model_id", "model_name",
	"prompt_tokens", "completion_tokens", "total_tokens", "estimated_cost_usd",
	"cache_status",
	"routed_provider_id", "routed_provider_name", "routed_model_id", "routed_model_name",
	"routing_rule_id", "routing_rule_name",
	"request_hook_decision", "request_hook_reason", "request_hook_reason_code",
	"response_hook_decision", "response_hook_reason", "response_hook_reason_code",
	"compliance_tags", "bump_status",
	"api_key_class", "api_key_fingerprint", "usage_extraction_status",
	"source_process", "action",
	"request_hooks_pipeline", "response_hooks_pipeline",
	"routing_trace", "details",
	"internal_purpose",
	"request_blocking_rule", "response_blocking_rule",
	"origin_tz",
	"error_code", "error_reason",
	"cache_creation_tokens", "cache_read_tokens",
	"normalized_strip_count", "normalized_strip_bytes", "cache_marker_injected",
	"cache_write_cost_usd", "cache_read_savings_usd", "cache_net_savings_usd",
	"gateway_cache_savings_usd",
	"thing_id", "thing_name",
	"credential_id",
	"passthrough_flags", "passthrough_reason",
	"upstream_ttfb_ms", "upstream_total_ms",
	"request_hooks_ms", "response_hooks_ms",
	"latency_breakdown",
	"reasoning_tokens",
	"reasoning_cost_usd",
	"target_method", "target_path",
	"gateway_cache_status", "gateway_cache_skip_reason", "gateway_cache_kind", "provider_cache_status",
	"attestation_verified", "attestation_agent_id",
	"embedding_cost_usd", "embedding_model_id",
	"ai_guard_cost_usd", "internal_ops_breakdown",
	"gateway_cache_l2_entry_key",
	"endpoint_type",
	"ingress_format",
}

// trafficEventRowValues returns the column values for one traffic_event row in
// trafficEventColumns order, with the same NUL-stripping / nil-promotion as the
// pgx.Batch path. Shared by insertTrafficEvents (Batch) and the COPY path.
func trafficEventRowValues(e TrafficEventMessage) []any {
	return appendTrafficEventRow(make([]any, 0, len(trafficEventColumns)), e)
}

// appendTrafficEventRow appends one traffic_event row's column values (in
// trafficEventColumns order) onto dst and returns the extended slice. Factored out
// of trafficEventRowValues so the COPY path can fill a pooled, pre-sized backing
// buffer (one []any for the whole batch, sub-sliced per row) instead of allocating
// a fresh per-row []any per record — that per-row slice was the single largest
// allocation on the hub COPY hot path. The boxed values themselves are unchanged.
func appendTrafficEventRow(dst []any, e TrafficEventMessage) []any {
	// compliance_tags is NOT NULL DEFAULT ARRAY[]::TEXT[]; pgx encodes a nil
	// []string as SQL NULL which would violate the constraint, so promote an absent
	// tag set to an empty array. NUL-strip each tag (SQLSTATE 22021).
	tags := e.ComplianceTags
	if tags == nil {
		tags = []string{}
	}
	for i, t := range tags {
		tags[i] = stripNul(t)
	}
	embeddingModelID := func() any {
		s := stripNul(e.EmbeddingModelID)
		if s == "" {
			return nil
		}
		return s
	}()
	l2EntryKey := func() any {
		s := stripNul(e.GatewayCacheL2EntryKey)
		if s == "" {
			return nil
		}
		return s
	}()
	return append(dst,
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
		e.CacheCreationTokens, e.CacheReadTokens,
		e.NormalizedStripCount, e.NormalizedStripBytes, e.CacheMarkerInjected,
		e.CacheWriteCostUsd, e.CacheReadSavingsUsd, e.CacheNetSavingsUsd,
		e.GatewayCacheSavingsUsd,
		stripNulPtr(e.ThingID), stripNulPtr(e.ThingName),
		stripNulPtr(e.CredentialID),
		passthroughFlagsParam(e.PassthroughFlags), passthroughReasonParam(e.PassthroughReason),
		e.UpstreamTtfbMs, e.UpstreamTotalMs,
		e.RequestHooksMs, e.ResponseHooksMs,
		nullableJSON(stripNulJSON(e.LatencyBreakdown)),
		e.ReasoningTokens,
		e.ReasoningCostUsd,
		stripNulPtr(e.TargetMethod), stripNulPtr(e.TargetPath),
		stripNulPtr(e.GatewayCacheStatus), stripNulPtr(e.GatewayCacheSkipReason), stripNulPtr(e.GatewayCacheKind), stripNulPtr(e.ProviderCacheStatus),
		e.AttestationVerified, stripNulPtr(e.AttestationAgentID),
		e.EmbeddingCostUsd, embeddingModelID,
		e.AIGuardCostUsd, nullableJSON(stripNulJSON(e.InternalOpsBreakdown)),
		l2EntryKey,
		stripNul(e.EndpointType),
		stripNul(e.IngressFormat),
	)
}

// payloadColumns is the traffic_event_payload column list in the order of
// payloadRowValues, matching insertPayloadSQL.
var payloadColumns = []string{
	"traffic_event_id",
	"inline_request_body", "inline_response_body",
	"request_spill_ref", "response_spill_ref",
	"request_size_bytes", "response_size_bytes",
	"request_truncated", "response_truncated",
	"request_content_type", "response_content_type",
	"inline_request_encoding", "inline_response_encoding",
}

// payloadRowValues returns the traffic_event_payload column values for one event
// in payloadColumns order, or ok=false when both bodies are absent (no row to
// write). Mirrors the inline/spill demux in insertPayloads (the Batch path);
// TestPayloadColumnsParity guards their column lists against drift. The inline
// body is the BYTEA column form from Body.ColumnPayload ([]byte raw bytes / raw
// compressed frame — no base64).
func payloadRowValues(e TrafficEventMessage) (vals []any, ok bool) {
	return appendPayloadRow(make([]any, 0, len(payloadColumns)), e)
}

// appendPayloadRow appends one traffic_event_payload row's values onto dst, or
// returns ok=false (dst unchanged) when both bodies are absent. Append-into-dst
// form so the COPY path can fill a pooled backing instead of a per-row []any.
func appendPayloadRow(dst []any, e TrafficEventMessage) (vals []any, ok bool) {
	if e.RequestBody.Kind == "absent" && e.ResponseBody.Kind == "absent" {
		return dst, false
	}
	var (
		inlineReq, inlineResp     any
		reqEnc, respEnc           *string
		reqSpillRef, respSpillRef any
		reqSize, respSize         *int64
		reqTrunc, respTrunc       bool
		reqCT, respCT             *string
	)
	if e.RequestBody.Kind == "inline" {
		payload, enc := e.RequestBody.ColumnPayload()
		inlineReq = payload
		reqEnc = &enc
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
		payload, enc := e.ResponseBody.ColumnPayload()
		inlineResp = payload
		respEnc = &enc
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
	return append(dst,
		stripNul(e.ID),
		inlineReq, inlineResp,
		reqSpillRef, respSpillRef,
		reqSize, respSize,
		reqTrunc, respTrunc,
		reqCT, respCT,
		reqEnc, respEnc,
	), true
}

// copyUpsert is the COPY→staging→INSERT…SELECT…ON CONFLICT idempotent bulk insert
// used by the COPY fast path. The staging table is created from `LIKE target` but
// WITHOUT its constraints/indexes (only INCLUDING DEFAULTS), so the COPY accepts
// duplicate keys and the `ON CONFLICT (conflictKey) DO NOTHING` on the fold-in
// step is what enforces idempotency against both already-committed rows and
// intra-batch duplicates. `ON COMMIT DROP` scopes the temp table to this tx, so a
// pooled connection reused by the next flush starts clean.
func copyUpsert(ctx context.Context, tx pgx.Tx, table string, cols []string, rows [][]any, conflictKey string) error {
	staging := "_copy_" + table
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`CREATE TEMP TABLE %s (LIKE %s INCLUDING DEFAULTS) ON COMMIT DROP`, staging, table)); err != nil {
		return fmt.Errorf("create staging %s: %w", staging, err)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{staging}, cols, pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("copy into %s: %w", staging, err)
	}
	colList := strings.Join(cols, ", ")
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (%s) SELECT %s FROM %s ON CONFLICT (%s) DO NOTHING`,
		table, colList, colList, staging, conflictKey)); err != nil {
		return fmt.Errorf("insert-select %s: %w", table, err)
	}
	return nil
}

// trafficCopyBuf pools the per-batch COPY backing: one flat []any holding every
// row's column values contiguously (sub-sliced per row) plus the [][]any row
// index handed to pgx.CopyFromRows. Reusing both across batches keeps the hot
// COPY path from allocating a fresh per-row []any per record (the single largest
// allocation on this path) or the outer slice in steady state.
type trafficCopyBuf struct {
	flat []any
	rows [][]any
}

var trafficCopyBufPool = sync.Pool{New: func() any { return new(trafficCopyBuf) }}

// reset prepares the buffer to hold up to nRows rows of cols columns. flat is
// pre-grown to the full nRows*cols so the per-row sub-slices taken during fill
// never re-back — an append-driven realloc mid-loop would orphan earlier rows'
// views into the old backing.
func (b *trafficCopyBuf) reset(nRows, cols int) {
	need := nRows * cols
	if cap(b.flat) < need {
		b.flat = make([]any, 0, need)
	}
	b.flat = b.flat[:0]
	if cap(b.rows) < nRows {
		b.rows = make([][]any, 0, nRows)
	}
	b.rows = b.rows[:0]
}

// release clears retained references (boxed pointers into the just-written events)
// so the pooled buffer can't pin event data past the flush, then returns it.
func (b *trafficCopyBuf) release() {
	for i := range b.flat {
		b.flat[i] = nil
	}
	b.flat = b.flat[:0]
	for i := range b.rows {
		b.rows[i] = nil
	}
	b.rows = b.rows[:0]
	trafficCopyBufPool.Put(b)
}

// insertTrafficEventsCopy is the COPY variant of insertTrafficEvents. Each row's
// values are appended into one pooled, pre-sized flat backing and sub-sliced, so a
// steady-state batch allocates no per-row []any.
func (w *TrafficEventWriter) insertTrafficEventsCopy(ctx context.Context, tx pgx.Tx, items []pendingTrafficMessage) error {
	cols := len(trafficEventColumns)
	buf := trafficCopyBufPool.Get().(*trafficCopyBuf)
	defer buf.release()
	buf.reset(len(items), cols)
	for _, pm := range items {
		start := len(buf.flat)
		buf.flat = appendTrafficEventRow(buf.flat, pm.event)
		buf.rows = append(buf.rows, buf.flat[start:len(buf.flat):len(buf.flat)])
	}
	return copyUpsert(ctx, tx, "traffic_event", trafficEventColumns, buf.rows, "id")
}

// insertPayloadsCopy is the COPY variant of insertPayloads. Rows where both
// bodies are absent are skipped, exactly like the Batch path. Uses the same pooled
// flat backing; skipped records simply don't advance flat (appendPayloadRow
// returns dst unchanged on ok=false).
func (w *TrafficEventWriter) insertPayloadsCopy(ctx context.Context, tx pgx.Tx, items []pendingTrafficMessage) error {
	cols := len(payloadColumns)
	buf := trafficCopyBufPool.Get().(*trafficCopyBuf)
	defer buf.release()
	buf.reset(len(items), cols)
	for _, pm := range items {
		start := len(buf.flat)
		next, ok := appendPayloadRow(buf.flat, pm.event)
		if !ok {
			continue
		}
		buf.flat = next
		buf.rows = append(buf.rows, buf.flat[start:len(buf.flat):len(buf.flat)])
	}
	if len(buf.rows) == 0 {
		return nil
	}
	return copyUpsert(ctx, tx, "traffic_event_payload", payloadColumns, buf.rows, "traffic_event_id")
}
