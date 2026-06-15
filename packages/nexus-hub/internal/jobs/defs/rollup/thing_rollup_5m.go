package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/rollup"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/domain"
)

// ThingRollup5mJob is the per-Thing sibling of Rollup5mJob: it aggregates
// traffic_event rows with non-null thing_id into thing_metric_rollup_5m,
// keyed by thing_id × metric × dim × sub-dim. Watermark and transaction are
// independent from the fleet rollup so per-Thing recovery is isolated.
// Rows for metrics that evaluate to zero for a given Thing are skipped at
// emit time — empty rows never reach the table.
const (
	thingRollup5mJobID          = "thing-rollup-5m"
	thingRollup5mJobName        = "Per-Thing Traffic Rollup (5 minute)"
	thingRollup5mJobDescription = "Aggregates traffic_event rows with thing_id into thing_metric_rollup_5m every minute, mirroring rollup-5m but keyed by (thing_id, metric, dim, sub-dim)."

	watermarkThingRollup5m = "thing-rollup-5m"
	tableThingRollup5m     = "thing_metric_rollup_5m"
)

type ThingRollup5mJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// per-bucket aggregation transaction is testable via pgxmock.
	pool                         defs.PgxPool
	interval                     time.Duration
	logger                       *slog.Logger
	enableAgentRollup            bool
	excludeInternalOpsFromBilled bool
}

// NewThingRollup5m constructs the job. interval defaults to 1 minute.
// enableAgentRollup gates whether source=agent rows from traffic_event are
// aggregated (defaults to false at fleet scale; see config.SchedulerConfig).
// excludeInternalOpsFromBilled mirrors fleet rollup — when false (default),
// L2 embedding + ai-guard classifier costs roll into MetricBilledCostUSD.
// When true, they stay on the dedicated metric series only.
func NewThingRollup5m(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger, enableAgentRollup, excludeInternalOpsFromBilled bool) *ThingRollup5mJob {
	if interval <= 0 {
		interval = time.Minute
	}
	return &ThingRollup5mJob{
		pool:                         pool,
		interval:                     interval,
		logger:                       logger.With("job", thingRollup5mJobID),
		enableAgentRollup:            enableAgentRollup,
		excludeInternalOpsFromBilled: excludeInternalOpsFromBilled,
	}
}

func (j *ThingRollup5mJob) ID() string              { return thingRollup5mJobID }
func (j *ThingRollup5mJob) Name() string            { return thingRollup5mJobName }
func (j *ThingRollup5mJob) Description() string     { return thingRollup5mJobDescription }
func (j *ThingRollup5mJob) Interval() time.Duration { return j.interval }

func (j *ThingRollup5mJob) Run(ctx context.Context) error {
	watermark, err := rollupstore.GetWatermark(ctx, j.pool, watermarkThingRollup5m)
	if err != nil || watermark.IsZero() {
		watermark = j.coldStartWatermark(ctx)
		j.logger.Info("initializing watermark", "watermark", watermark.Format(time.RFC3339))
	}

	latestSealed := time.Now().UTC().Add(-bucketDuration5m).Truncate(bucketDuration5m)
	if !watermark.Before(latestSealed) {
		return nil
	}

	var count int
	for bucket := watermark.Add(bucketDuration5m); !bucket.After(latestSealed); bucket = bucket.Add(bucketDuration5m) {
		if err := j.processOneBucket(ctx, bucket); err != nil {
			return fmt.Errorf("bucket %s: %w", bucket.Format(time.RFC3339), err)
		}
		count++
	}

	if count > 0 {
		j.logger.Info("thing rollup completed", "buckets", count)
	}
	return nil
}

func (j *ThingRollup5mJob) coldStartWatermark(ctx context.Context) time.Time {
	earliest, ok, err := rollupstore.EarliestTrafficEventTimestamp(ctx, j.pool)
	if err != nil {
		j.logger.Warn("earliest traffic_event lookup failed, using default lookback", "error", err)
		return pickColdStartWatermark(time.Now().UTC(), rollup5mInitLookback, bucketDuration5m, time.Time{}, false)
	}
	return pickColdStartWatermark(time.Now().UTC(), rollup5mInitLookback, bucketDuration5m, earliest, ok)
}

// processOneBucket aggregates one per-Thing 5-minute bucket and advances the
// live watermark. Used by the live catch-up loop.
func (j *ThingRollup5mJob) processOneBucket(ctx context.Context, bucketStart time.Time) error {
	return j.processBucket(ctx, bucketStart, true)
}

// processBucket aggregates one per-Thing 5-minute bucket inside a single
// transaction. When writeWatermark is false (the correction backfill path) the
// live thing-rollup-5m watermark is left untouched so re-aggregating historical
// buckets does not rewind the live cursor.
func (j *ThingRollup5mJob) processBucket(ctx context.Context, bucketStart time.Time, writeWatermark bool) error {
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := rollupstore.DeleteThingRollupBucket(ctx, tx, tableThingRollup5m, bucketStart); err != nil {
		return err
	}

	bucketEnd := bucketStart.Add(bucketDuration5m)
	rows, err := j.aggregateThingEvents(ctx, tx, bucketStart, bucketEnd)
	if err != nil {
		return err
	}

	if err := rollupstore.InsertThingRollupRows(ctx, tx, tableThingRollup5m, rows); err != nil {
		return err
	}

	if writeWatermark {
		if err := rollupstore.SetWatermark(ctx, tx, watermarkThingRollup5m, bucketStart); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// thingAccKey keys per-Thing value, histogram, and timestamp accumulators.
type thingAccKey struct {
	thingID, metricName, dimensionKey, subDimension string
}

// thingDistinctKey keys per-Thing distinct-count tracking.
type thingDistinctKey struct {
	thingID, metricName, dimensionKey, subDimension string
}

// aggregateThingEvents scans traffic_event rows with non-null thing_id for
// [start, end) and produces thing-keyed rollup rows. Mirrors
// Rollup5mJob.aggregateTrafficEvents but partitions every accumulator by
// thing_id and skips rows lacking it (those land in the fleet rollup only).
func (j *ThingRollup5mJob) aggregateThingEvents(ctx context.Context, tx pgx.Tx, start, end time.Time) ([]metrics.ThingRollupRow, error) {
	// agentClause excludes source=agent rows when the toggle is OFF (default).
	// Agents compute their own rollups locally at fleet scale.
	agentClause := ""
	if !j.enableAgentRollup {
		agentClause = " AND source != 'agent'"
	}
	q := `
		SELECT
			thing_id,
			source, provider_id, model_id,
			entity_id, entity_type, org_id,
			routed_provider_id,
			routing_rule_id, target_host, source_ip,
			status_code, latency_ms,
			-- cache_hit_count uses gateway_cache_status so it reflects
			-- gateway-cache hits only (not provider prompt-cache discounts).
			-- Mirror of rollup_5m.go.
			(gateway_cache_status IN ('hit', 'hit_inflight')) AS cache_hit,
			prompt_tokens, completion_tokens, total_tokens, estimated_cost_usd, gateway_cache_savings_usd,
			request_hook_decision, response_hook_decision, bump_status,
			routed_model_id, model_id AS original_model_id,
			(CASE WHEN status_code < 400 AND details ? 'qualitySignals' THEN true ELSE false END) AS has_quality_signals,
			-- identity.vk is the Virtual Key (see rollup_5m.go for full
			-- rationale of why this is NOT "credential").
			identity->'vk'->>'id' AS virtual_key_id,
			identity->'project'->>'id' AS project_id,
			error_code,
			timestamp,
			cache_write_cost_usd, cache_read_savings_usd, cache_net_savings_usd,
			cache_creation_tokens, cache_read_tokens,
			(cache_read_tokens IS NOT NULL AND cache_read_tokens > 0) AS l4_cache_hit,
			normalized_strip_count, normalized_strip_bytes, cache_marker_injected,
			-- Latency phase fields.
			upstream_ttfb_ms, upstream_total_ms, request_hooks_ms, response_hooks_ms,
			-- Internal-ops cost rollup fields.
			embedding_cost_usd, ai_guard_cost_usd
		FROM traffic_event
		WHERE timestamp >= $1 AND timestamp < $2
		  AND thing_id IS NOT NULL` + agentClause

	rows, err := tx.Query(ctx, q, start, end)
	if err != nil {
		return nil, fmt.Errorf("query traffic_event (thing): %w", err)
	}
	defer rows.Close()

	accValues := make(map[thingAccKey]float64)
	accHisto := make(map[thingAccKey]metrics.Histogram)
	accTimestamp := make(map[thingAccKey]metrics.TimestampMeta)
	distinctEntities := make(map[thingDistinctKey]map[string]struct{})
	distinctOrgs := make(map[thingDistinctKey]map[string]struct{})
	distinctIPs := make(map[thingDistinctKey]map[string]struct{})

	for rows.Next() {
		var (
			thingID              *string
			source               *string
			providerID           *string
			modelID              *string
			entityID             *string
			entityType           *string
			orgID                *string
			routedProviderID     *string
			routingRuleID        *string
			targetHost           *string
			sourceIP             *string
			statusCode           *int
			latencyMs            *int
			cacheHit             *bool
			promptTokens         *int
			completionTokens     *int
			totalTokens          *int
			estimatedCost        *float64
			gatewayCacheSavings  *float64
			requestHookDecision  *string
			responseHookDecision *string
			bumpStatus           *string
			routedModelID        *string
			originalModelID      *string
			hasQualitySignals    *bool
			virtualKeyID         *string
			projectID            *string
			errorCode            *string
			timestamp            time.Time
			cacheWriteCost       *float64
			cacheReadSavings     *float64
			cacheNetSavings      *float64
			cacheCreationTokens  *int64
			cacheReadTokens      *int64
			l4CacheHit           *bool
			normStripCount       *int64
			normStripBytes       *int64
			cacheMarkersInj      *int64
			// Latency phase fields.
			upstreamTtfbMs  *int
			upstreamTotalMs *int
			requestHooksMs  *int
			responseHooksMs *int
			// Internal-ops costs.
			embeddingCostUsd *float64
			aiGuardCostUsd   *float64
		)

		if err := rows.Scan(
			&thingID,
			&source, &providerID, &modelID,
			&entityID, &entityType, &orgID,
			&routedProviderID,
			&routingRuleID, &targetHost, &sourceIP,
			&statusCode, &latencyMs, &cacheHit,
			&promptTokens, &completionTokens, &totalTokens, &estimatedCost, &gatewayCacheSavings,
			&requestHookDecision, &responseHookDecision, &bumpStatus,
			&routedModelID, &originalModelID,
			&hasQualitySignals, &virtualKeyID, &projectID,
			&errorCode,
			&timestamp,
			&cacheWriteCost, &cacheReadSavings, &cacheNetSavings,
			&cacheCreationTokens, &cacheReadTokens, &l4CacheHit,
			&normStripCount, &normStripBytes, &cacheMarkersInj,
			&upstreamTtfbMs, &upstreamTotalMs, &requestHooksMs, &responseHooksMs,
			&embeddingCostUsd, &aiGuardCostUsd,
		); err != nil {
			return nil, fmt.Errorf("scan traffic_event (thing): %w", err)
		}

		// thing_id was filtered IS NOT NULL but be defensive.
		tid := deref5m(thingID)
		if tid == "" {
			continue
		}

		// Same source filtering as the fleet rollup.
		srcDomain, ok := domain.DBSourceToDomain(deref5m(source))
		if !ok {
			continue
		}
		srcVal := string(srcDomain)

		dims := buildEventDims(
			deref5m(providerID), deref5m(routedModelID),
			deref5m(entityID), deref5m(entityType),
			deref5m(orgID), deref5m(routedProviderID),
			deref5m(routingRuleID), deref5m(targetHost),
			deref5m(virtualKeyID), deref5m(projectID),
			normalizeHookDecision(deref5m(requestHookDecision), deref5m(responseHookDecision)),
		)
		subDim := metrics.BuildSubDimension(srcVal, "")

		for _, dim := range dims {
			dimKey := metrics.BuildDimensionKey(dim.name, dim.value)
			j.emitThingEventMetrics(
				accValues, accHisto, accTimestamp,
				distinctEntities, distinctOrgs, distinctIPs,
				tid, dimKey, subDim,
				statusCode, latencyMs, cacheHit,
				promptTokens, completionTokens, totalTokens, estimatedCost, gatewayCacheSavings,
				worstHookDecision(requestHookDecision, responseHookDecision), bumpStatus,
				routingRuleID,
				entityID, orgID, sourceIP,
				timestamp,
				hasQualitySignals,
				errorCode,
				cacheWriteCost, cacheReadSavings, cacheNetSavings,
				cacheCreationTokens, cacheReadTokens, l4CacheHit,
				normStripCount, normStripBytes, cacheMarkersInj,
				upstreamTtfbMs, upstreamTotalMs, requestHooksMs, responseHooksMs,
				embeddingCostUsd, aiGuardCostUsd,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate traffic_event (thing): %w", err)
	}

	return j.assembleThingRollupRows(start, accValues, accHisto, accTimestamp,
		distinctEntities, distinctOrgs, distinctIPs), nil
}

// emitThingEventMetrics is the thing-keyed twin of emitEventMetrics.
func (j *ThingRollup5mJob) emitThingEventMetrics(
	accValues map[thingAccKey]float64,
	accHisto map[thingAccKey]metrics.Histogram,
	accTimestamp map[thingAccKey]metrics.TimestampMeta,
	distinctEntities, distinctOrgs, distinctIPs map[thingDistinctKey]map[string]struct{},
	thingID, dimKey, subDim string,
	statusCode *int, latencyMs *int, cacheHit *bool,
	promptTokens, completionTokens, totalTokens *int,
	estimatedCost *float64,
	gatewayCacheSavings *float64,
	hookDecision, bumpStatus *string,
	routingRuleID *string,
	entityID, orgID, sourceIP *string,
	timestamp time.Time,
	hasQualitySignals *bool,
	errorCode *string,
	cacheWriteCost, cacheReadSavings, cacheNetSavings *float64,
	cacheCreationTokens, cacheReadTokens *int64,
	l4CacheHit *bool,
	normStripCount, normStripBytes, cacheMarkersInj *int64,
	upstreamTtfbMs, upstreamTotalMs, requestHooksMs, responseHooksMs *int,
	embeddingCostUsd, aiGuardCostUsd *float64,
) {
	add := func(metric string, v float64) {
		accValues[thingAccKey{thingID, metric, dimKey, subDim}] += v
	}

	add(metrics.MetricRequestCount, 1)

	sc := derefInt5m(statusCode)
	switch {
	case sc >= 200 && sc < 300:
		add(metrics.MetricStatus2xxCount, 1)
	case sc >= 400 && sc < 500:
		add(metrics.MetricStatus4xxCount, 1)
	case sc >= 500:
		add(metrics.MetricStatus5xxCount, 1)
	}

	cacheHitVal := derefBool5m(cacheHit)
	if cacheHitVal {
		add(metrics.MetricCacheHitCount, 1)
	}
	if v := derefFloat5m(gatewayCacheSavings); v != 0 {
		add(metrics.MetricCacheSavedCostUSD, v)
	}

	add(metrics.MetricPromptTokens, float64(derefInt5m(promptTokens)))
	add(metrics.MetricCompletionTokens, float64(derefInt5m(completionTokens)))
	add(metrics.MetricTotalTokens, float64(derefInt5m(totalTokens)))

	cost := derefFloat5m(estimatedCost)
	add(metrics.MetricEstimatedCostUSD, cost)
	if sc >= 400 {
		add(metrics.MetricWastedCostUSD, cost)
	}

	isSuccess := sc >= 200 && sc < 300 && deref5m(errorCode) == ""
	if isSuccess && !cacheHitVal {
		billed := cost
		// excludeInternalOpsFromBilled mirrors fleet rollup; default false
		// (include internal-ops in billed total). Flip true to exclude.
		if !j.excludeInternalOpsFromBilled {
			billed += derefFloat5m(embeddingCostUsd) + derefFloat5m(aiGuardCostUsd)
		}
		add(metrics.MetricBilledCostUSD, billed)
		add(metrics.MetricBilledTokens, float64(derefInt5m(totalTokens)))
	}

	// Internal-ops cost. Always emitted as separate dashboard series
	// regardless of the billed-cost toggle above.
	if v := derefFloat5m(embeddingCostUsd); v != 0 {
		add(metrics.MetricEmbeddingCostUSD, v)
	}
	if v := derefFloat5m(aiGuardCostUsd); v != 0 {
		add(metrics.MetricAIGuardCostUSD, v)
	}

	lat := derefInt5m(latencyMs)
	if lat > 0 {
		add(metrics.MetricLatencySum, float64(lat))
		add(metrics.MetricLatencyCount, 1)
		hk := thingAccKey{thingID, metrics.MetricLatencyHistogram, dimKey, subDim}
		h := accHisto[hk]
		idx := metrics.BucketForLatency(float64(lat))
		h[idx]++
		accHisto[hk] = h
	}

	// Latency phase aggregates — per-Thing twin of the fleet rollup_5m emission.
	if upstreamTtfbMs != nil && *upstreamTtfbMs > 0 {
		add(metrics.MetricLatencyUpstreamTtfbSum, float64(*upstreamTtfbMs))
		add(metrics.MetricLatencyUpstreamTtfbCount, 1)
	}
	if upstreamTotalMs != nil && *upstreamTotalMs > 0 {
		add(metrics.MetricLatencyUpstreamSum, float64(*upstreamTotalMs))
		add(metrics.MetricLatencyUpstreamCount, 1)
		if lat > 0 {
			us := lat - *upstreamTotalMs
			if us < 0 {
				us = 0
			}
			add(metrics.MetricLatencyUsSum, float64(us))
			add(metrics.MetricLatencyUsCount, 1)
		}
	}
	hooksTotal := 0
	if requestHooksMs != nil {
		hooksTotal += *requestHooksMs
	}
	if responseHooksMs != nil {
		hooksTotal += *responseHooksMs
	}
	if hooksTotal > 0 {
		add(metrics.MetricLatencyHooksSum, float64(hooksTotal))
		add(metrics.MetricLatencyHooksCount, 1)
	}

	if deref5m(routingRuleID) != "" {
		add(metrics.MetricRoutingRuleHit, 1)
	}

	switch hd := deref5m(hookDecision); hd {
	case "APPROVE", "allow":
		add(metrics.MetricHookAllowCount, 1)
	case "REJECT_HARD", "BLOCK_SOFT", "reject":
		add(metrics.MetricHookDenyCount, 1)
		add(metrics.MetricRejectCount, 1)
	case "ERROR", "error":
		add(metrics.MetricHookErrorCount, 1)
	case "":
		// no hook decision
	default:
		add(metrics.MetricHookUnknownCount, 1)
	}

	switch bs := deref5m(bumpStatus); {
	case bs == "BUMP_SUCCESS":
		add(metrics.MetricBumpSuccessCount, 1)
	case bs == "BUMP_FAILED_PASSTHROUGH":
		add(metrics.MetricBumpFailedCount, 1)
	case strings.HasPrefix(bs, "BUMP_EXEMPT"):
		add(metrics.MetricBumpExemptCount, 1)
	case strings.HasPrefix(bs, "BUMP_DISABLED"):
		add(metrics.MetricBumpDisabledCount, 1)
	}

	if derefBool5m(hasQualitySignals) {
		add(metrics.MetricQualityAnomalyCount, 1)
	}

	if v := derefFloat5m(cacheWriteCost); v != 0 {
		add(metrics.MetricCacheWriteCostUSD, v)
	}
	if v := derefFloat5m(cacheReadSavings); v != 0 {
		add(metrics.MetricCacheReadSavingsUSD, v)
	}
	if v := derefFloat5m(cacheNetSavings); v != 0 {
		add(metrics.MetricCacheNetSavingsUSD, v)
	}
	if v := derefInt645m(cacheCreationTokens); v != 0 {
		add(metrics.MetricCacheCreationTokens, float64(v))
	}
	if v := derefInt645m(cacheReadTokens); v != 0 {
		add(metrics.MetricCacheReadTokens, float64(v))
	}
	if derefBool5m(l4CacheHit) {
		add(metrics.MetricRequestsWithProviderPromptCacheHit, 1)
	}

	if v := derefInt645m(normStripCount); v != 0 {
		add(metrics.MetricNormalisedStripCount, float64(v))
	}
	if v := derefInt645m(normStripBytes); v != 0 {
		add(metrics.MetricNormalisedStripBytes, float64(v))
	}
	if v := derefInt645m(cacheMarkersInj); v != 0 {
		add(metrics.MetricCacheMarkersInjected, float64(v))
	}

	if dimKey != "" {
		tsStr := timestamp.UTC().Format(time.RFC3339)
		tsKey := thingAccKey{thingID, metrics.MetricFirstSeen, dimKey, subDim}
		existing := accTimestamp[tsKey]
		incoming := metrics.TimestampMeta{FirstSeen: tsStr, LastSeen: tsStr}
		accTimestamp[tsKey] = metrics.MergeTimestampMeta(existing, incoming)
	}

	track := func(m map[thingDistinctKey]map[string]struct{}, metric, val string) {
		if val == "" {
			return
		}
		dk := thingDistinctKey{thingID, metric, dimKey, subDim}
		if m[dk] == nil {
			m[dk] = make(map[string]struct{})
		}
		m[dk][val] = struct{}{}
	}
	track(distinctEntities, metrics.MetricActiveEntities, deref5m(entityID))
	track(distinctOrgs, metrics.MetricActiveOrganizations, deref5m(orgID))
	track(distinctIPs, metrics.MetricDistinctSources, deref5m(sourceIP))
}

func (j *ThingRollup5mJob) assembleThingRollupRows(
	bucketStart time.Time,
	accValues map[thingAccKey]float64,
	accHisto map[thingAccKey]metrics.Histogram,
	accTimestamp map[thingAccKey]metrics.TimestampMeta,
	distinctEntities, distinctOrgs, distinctIPs map[thingDistinctKey]map[string]struct{},
) []metrics.ThingRollupRow {
	var out []metrics.ThingRollupRow

	for k, v := range accValues {
		if v == 0 {
			continue
		}
		out = append(out, metrics.ThingRollupRow{
			BucketStart:  bucketStart,
			ThingID:      k.thingID,
			MetricName:   k.metricName,
			DimensionKey: k.dimensionKey,
			SubDimension: k.subDimension,
			Value:        v,
		})
	}

	for k, h := range accHisto {
		data, err := json.Marshal(h)
		if err != nil {
			j.logger.Error("marshal histogram", "error", err)
			continue
		}
		out = append(out, metrics.ThingRollupRow{
			BucketStart:  bucketStart,
			ThingID:      k.thingID,
			MetricName:   k.metricName,
			DimensionKey: k.dimensionKey,
			SubDimension: k.subDimension,
			Metadata:     data,
		})
	}

	for k, ts := range accTimestamp {
		data, err := json.Marshal(ts)
		if err != nil {
			j.logger.Error("marshal timestamp meta", "error", err)
			continue
		}
		out = append(out, metrics.ThingRollupRow{
			BucketStart:  bucketStart,
			ThingID:      k.thingID,
			MetricName:   k.metricName,
			DimensionKey: k.dimensionKey,
			SubDimension: k.subDimension,
			Metadata:     data,
		})
	}

	emit := func(m map[thingDistinctKey]map[string]struct{}) {
		for dk, set := range m {
			if len(set) == 0 {
				continue
			}
			out = append(out, metrics.ThingRollupRow{
				BucketStart:  bucketStart,
				ThingID:      dk.thingID,
				MetricName:   dk.metricName,
				DimensionKey: dk.dimensionKey,
				SubDimension: dk.subDimension,
				Value:        float64(len(set)),
			})
		}
	}
	emit(distinctEntities)
	emit(distinctOrgs)
	emit(distinctIPs)

	return out
}
