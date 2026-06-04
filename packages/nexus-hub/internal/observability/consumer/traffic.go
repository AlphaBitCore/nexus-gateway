package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TrafficEventWriterConfig holds configuration for the traffic event writer.
type TrafficEventWriterConfig struct {
	BatchSize     int           `yaml:"batchSize"`
	FlushInterval time.Duration `yaml:"flushInterval"`
}

type pendingTrafficMessage struct {
	event TrafficEventMessage
	msg   *mq.Message
}

// PgxPool is the minimum pgx pool surface the writers in this package need
// — only flush() touches the pool directly (Begin tx; the rest of the
// insert path operates on the resulting pgx.Tx, which is already an
// interface in pgx and needs no seam of its own). The concrete
// *pgxpool.Pool satisfies it in production; pgxmock's PgxPoolIface
// satisfies it in tests, letting flush()'s Begin→SendBatch→Commit chain
// be exercised without a live Postgres. Mirrors the PgxPool convention
// from packages/nexus-hub/internal/observability/siem/bridge.go,
// packages/nexus-hub/internal/alerts/engine/store.go, and
// packages/ai-gateway/internal/cache/layer/layer.go.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	// Exec runs a single statement outside any caller-held transaction.
	// Used by the DLQ insert path which must succeed even when the
	// flush tx itself has rolled back.
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// TrafficEventWriter consumes traffic events from 3 MQ queues and batch-inserts
// them into the traffic_event table. Consumer group: "hub-db-writer".
type TrafficEventWriter struct {
	pool   PgxPool // interface seam — *pgxpool.Pool in prod, pgxmock in tests
	mqc    mq.Consumer
	cfg    TrafficEventWriterConfig
	logger *slog.Logger

	// consumed_total / flush_total / traffic_errors_total align with
	// mq.processed_total{stream, status} but the existing label scheme
	// (queue, result, error_type) is more diagnostic for the writer path,
	// so kept verbatim under mq.* dotted names. The error counter is
	// traffic_errors_total (not errors_total) so it doesn't collide with the
	// shared MQ transport layer's unlabeled nexus_mq_errors_total — same
	// per-consumer namespacing the exemption/admin/siem writers use.
	consumedTotal    *opsmetrics.Counter
	flushTotal       *opsmetrics.Counter
	batchSizeHist    *opsmetrics.Histogram
	errorsTotal      *opsmetrics.Counter
	dlqInsertedTotal *opsmetrics.Counter
}

// TrafficQueues lists the 3 MQ queues this consumer reads from.
var TrafficQueues = []string{
	"nexus.event.ai-traffic",
	"nexus.event.compliance",
	"nexus.event.agent",
}

const dbWriterGroup = "hub-db-writer"

// NewTrafficEventWriter creates a new writer. Call Start(ctx) to begin consuming.
// reg powers both /metrics and the per-tick metrics_sample push; pass nil
// only in test harnesses that do not exercise the metrics path.
func NewTrafficEventWriter(
	pool *pgxpool.Pool,
	mqc mq.Consumer,
	cfg TrafficEventWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *TrafficEventWriter {
	return newTrafficEventWriter(pool, mqc, cfg, logger, reg)
}

// NewTrafficEventWriterWithPgxPool is the test-only constructor accepting any
// PgxPool — production code goes through NewTrafficEventWriter. Lets the
// flush()'s Begin→SendBatch→Commit chain be driven through pgxmock without a
// live Postgres so the error branches (begin failure, insert failure with
// 22021 poison-pill ack vs nakAll, payload failure, normalized warn-and-
// continue, commit failure, ackAll success) are exercised in unit tests.
func NewTrafficEventWriterWithPgxPool(
	pool PgxPool,
	mqc mq.Consumer,
	cfg TrafficEventWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *TrafficEventWriter {
	return newTrafficEventWriter(pool, mqc, cfg, logger, reg)
}

func newTrafficEventWriter(
	pool PgxPool,
	mqc mq.Consumer,
	cfg TrafficEventWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *TrafficEventWriter {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}

	w := &TrafficEventWriter{
		pool:   pool,
		mqc:    mqc,
		cfg:    cfg,
		logger: logger.With("component", "traffic-event-writer"),
	}
	if reg != nil {
		w.consumedTotal = reg.NewCounter("mq.processed_total", []string{"queue"})
		w.flushTotal = reg.NewCounter("mq.batch_flush_total", []string{"result"})
		w.batchSizeHist = reg.NewHistogram("mq.batch_size", nil)
		w.errorsTotal = reg.NewCounter("mq.traffic_errors_total", []string{"error_type"})
		w.dlqInsertedTotal = reg.NewCounter("mq.dlq_inserted_total", []string{"subject"})
	}
	return w
}

// Start begins consuming from all 3 event queues in parallel goroutines.
// Blocks until ctx is cancelled.
func (w *TrafficEventWriter) Start(ctx context.Context) error {
	for _, queue := range TrafficQueues {
		q := queue
		batch := NewBatchAccumulator[pendingTrafficMessage](w.cfg.BatchSize, w.cfg.FlushInterval, func(items []pendingTrafficMessage) error {
			return w.flush(ctx, items)
		})

		go func() {
			defer batch.Stop() //nolint:errcheck

			err := w.mqc.Consume(ctx, q, dbWriterGroup, func(_ context.Context, msg *mq.Message) error {
				return w.handleMessage(q, batch, msg)
			})

			if err != nil && ctx.Err() == nil {
				w.logger.Error("consumer exited with error", "queue", q, "error", err)
			}
		}()
	}

	<-ctx.Done()
	return nil
}

// handleMessage is the per-message handler passed to mq.Consumer.Consume.
// Returns nil if the message is a poison pill (already acked inline); returns
// mq.ErrDeferAck if the message is buffered and will be acked after the batch
// flush; returns a non-sentinel error to trigger auto-nak by the MQ driver.
func (w *TrafficEventWriter) handleMessage(queue string, batch *BatchAccumulator[pendingTrafficMessage], msg *mq.Message) error {
	if w.consumedTotal != nil {
		w.consumedTotal.With(queue).Inc()
	}

	var evt TrafficEventMessage
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		w.logger.Error("deserialize failed, dropping message",
			"queue", queue, "error", err)
		if w.errorsTotal != nil {
			w.errorsTotal.With("deserialize").Inc()
		}
		return msg.Ack()
	}

	if err := batch.Add(pendingTrafficMessage{event: evt, msg: msg}); err != nil {
		// Synchronous flush failure (batch hit maxSize and flush errored).
		// flush already invoked nakAll on this item; returning the error lets
		// the driver log it. The driver's auto-nak is idempotent on NATS/Redis.
		return err
	}
	// Hand ack/nak off to the batch flush path (ackAll / nakAll).
	return mq.ErrDeferAck
}

func (w *TrafficEventWriter) flush(ctx context.Context, items []pendingTrafficMessage) error {
	if w.batchSizeHist != nil {
		w.batchSizeHist.With().Observe(float64(len(items)))
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.logger.Error("flush: begin tx failed", "error", err, "count", len(items))
		w.nakOrDLQ(ctx, items, err)
		if w.flushTotal != nil {
			w.flushTotal.With("error").Inc()
		}
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_begin").Inc()
		}
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := w.insertTrafficEvents(ctx, tx, items); err != nil {
		w.logger.Error("flush: insert traffic_event failed", "error", err, "count", len(items))
		// SQLSTATE 22021 = invalid_character_value_for_cast (null bytes, bad encoding).
		// This is a permanent data error — retrying forever blocks the queue.
		// Ack to discard and move on; the error log is the audit trail.
		if strings.Contains(err.Error(), "22021") {
			w.logger.Warn("flush: permanent encoding error, acking to skip poison batch", "count", len(items))
			w.ackAll(items)
		} else {
			w.nakOrDLQ(ctx, items, err)
		}
		if w.flushTotal != nil {
			w.flushTotal.With("error").Inc()
		}
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_insert").Inc()
		}
		return fmt.Errorf("insert traffic_event: %w", err)
	}

	if err := w.insertPayloads(ctx, tx, items); err != nil {
		w.logger.Error("flush: insert traffic_event_payload failed", "error", err, "count", len(items))
		w.nakOrDLQ(ctx, items, err)
		if w.flushTotal != nil {
			w.flushTotal.With("error").Inc()
		}
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_insert_payload").Inc()
		}
		return fmt.Errorf("insert traffic_event_payload: %w", err)
	}

	// Normalized payloads are an independent sidecar; a failure here does NOT
	// roll the rest of the batch because raw bytes are already persisted on
	// traffic_event_payload. Errors are logged + counted so ops can see drift
	// if the normalize layer regresses.
	if err := w.insertNormalizedPayloads(ctx, tx, items); err != nil {
		w.logger.Warn("flush: insert traffic_event_normalized failed (raw bytes still persisted)",
			"error", err, "count", len(items))
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_insert_normalized").Inc()
		}
	}

	if err := tx.Commit(ctx); err != nil {
		w.logger.Error("flush: commit failed", "error", err, "count", len(items))
		w.nakOrDLQ(ctx, items, err)
		if w.flushTotal != nil {
			w.flushTotal.With("error").Inc()
		}
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_commit").Inc()
		}
		return fmt.Errorf("commit tx: %w", err)
	}

	w.ackAll(items)
	if w.flushTotal != nil {
		w.flushTotal.With("success").Inc()
	}

	w.logger.Debug("flushed traffic events", "count", len(items))
	return nil
}

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
			// Both NULL on regular user-traffic rows.
			e.AIGuardCostUsd,
			nullableJSON(e.InternalOpsBreakdown),
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

// insertNormalizedPayloads writes the sidecar rows for events whose
// producer attached at least one normalized field (request or response).
// Rows with no normalize fields at all are skipped — the FK to
// traffic_event guarantees a parent row exists, but there is no value
// in storing an all-null sidecar.
func (w *TrafficEventWriter) insertNormalizedPayloads(ctx context.Context, tx pgx.Tx, items []pendingTrafficMessage) error {
	batch := &pgx.Batch{}
	count := 0

	for _, pm := range items {
		e := pm.event
		if len(e.RequestNormalized) == 0 && len(e.ResponseNormalized) == 0 &&
			e.RequestNormalizeStatus == "" && e.ResponseNormalizeStatus == "" {
			continue
		}
		ver := e.NormalizeVersion
		if ver == "" {
			ver = "1"
		}
		batch.Queue(insertNormalizedSQL,
			e.ID,
			nullableJSON(stripNulJSON(e.RequestNormalized)),
			nullableJSON(stripNulJSON(e.ResponseNormalized)),
			nilIfEmpty(e.RequestNormalizeStatus),
			nilIfEmpty(e.ResponseNormalizeStatus),
			nilIfEmpty(e.RequestNormalizeError),
			nilIfEmpty(e.ResponseNormalizeError),
			ver,
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
			return fmt.Errorf("exec normalized insert: %w", err)
		}
	}
	return nil
}

const insertNormalizedSQL = `
INSERT INTO traffic_event_normalized (
    traffic_event_id,
    request_normalized, response_normalized,
    request_status, response_status,
    request_error_reason, response_error_reason,
    normalize_version
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (traffic_event_id) DO NOTHING
`

// stripNul removes PostgreSQL-illegal null bytes (\x00) from a string.
// PostgreSQL UTF-8 columns reject \x00 with SQLSTATE 22021.
func stripNul(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

func stripNulPtr(p *string) *string {
	if p == nil {
		return nil
	}
	s := stripNul(*p)
	return &s
}

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

func stripNulJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	if !strings.ContainsRune(string(raw), 0) {
		return raw
	}
	return json.RawMessage(strings.ReplaceAll(string(raw), "\x00", ""))
}

func (w *TrafficEventWriter) ackAll(items []pendingTrafficMessage) {
	for _, pm := range items {
		if err := pm.msg.Ack(); err != nil {
			w.logger.Warn("ack failed", "error", err)
		}
	}
}

// redeliveryThresholdAttempts caps the number of times a single message can
// be redelivered before the consumer ACKs it and writes the raw bytes to
// traffic_event_dlq instead. Picked so a transient DB blip
// (~30s of ack-wait redeliveries) recovers normally while a permanent
// DB-side error (constraint, missing column, bad enum cast) hits the cap
// within ~2.5 minutes — fast enough that JetStream's 6h MaxAge eviction
// no longer pins the stream waiting for a fix.
const redeliveryThresholdAttempts = 5

// nakOrDLQ routes each item to either Nak (let the broker redeliver) or
// ACK + DLQ insertion (when the message has hit the redelivery cap). lastErr
// is stamped on the DLQ row's last_error column so operators see why the
// row got there without grep'ing logs by event ID. ctx is the parent flush
// ctx; the DLQ insert reuses it for cancellation semantics.
//
// When the DLQ insert itself fails (e.g. DB is down), the message falls
// back to Nak so the broker re-attempts — better to retry forever than
// silently drop a message we don't even know about. Operators see the
// dlq_insert_failed counter spike.
func (w *TrafficEventWriter) nakOrDLQ(ctx context.Context, items []pendingTrafficMessage, lastErr error) {
	for _, pm := range items {
		if pm.msg.NumDelivered >= redeliveryThresholdAttempts {
			if err := w.insertDLQ(ctx, pm, lastErr); err != nil {
				w.logger.Error("dlq insert failed; falling back to nak",
					"msgId", pm.event.ID, "subject", pm.msg.Subject, "error", err)
				if w.errorsTotal != nil {
					w.errorsTotal.With("dlq_insert").Inc()
				}
				if nakErr := pm.msg.Nak(); nakErr != nil {
					w.logger.Warn("nak failed after dlq insert failure", "error", nakErr)
				}
				continue
			}
			if err := pm.msg.Ack(); err != nil {
				w.logger.Warn("dlq ack failed", "error", err)
			}
			if w.dlqInsertedTotal != nil {
				w.dlqInsertedTotal.With(pm.msg.Subject).Inc()
			}
			w.logger.Warn("message moved to traffic_event_dlq after redelivery cap",
				"msgId", pm.event.ID, "subject", pm.msg.Subject,
				"deliveries", pm.msg.NumDelivered, "lastError", errString(lastErr))
			continue
		}
		if err := pm.msg.Nak(); err != nil {
			w.logger.Warn("nak failed", "error", err)
		}
	}
}

// insertDLQ writes a single message to traffic_event_dlq. Best-effort: the
// caller decides what to do with the error (typically: fall back to Nak so
// the broker keeps trying instead of silently dropping the message).
func (w *TrafficEventWriter) insertDLQ(ctx context.Context, pm pendingTrafficMessage, lastErr error) error {
	const sql = `
INSERT INTO traffic_event_dlq
    (msg_id, subject, payload, delivery_count, last_error)
VALUES ($1, $2, $3, $4, $5)
`
	var errPtr *string
	if lastErr != nil {
		s := lastErr.Error()
		errPtr = &s
	}
	_, err := w.pool.Exec(ctx, sql,
		pm.event.ID,
		pm.msg.Subject,
		pm.msg.Data,
		int(pm.msg.NumDelivered),
		errPtr,
	)
	return err
}

// errString returns "" for nil errors so structured logs stay clean.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
