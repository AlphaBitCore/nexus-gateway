package rollup

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
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

const (
	rollup5mJobID          = "rollup-5m"
	rollup5mJobName        = "Traffic Rollup (5 minute)"
	rollup5mJobDescription = "Aggregates traffic_event rows into metric_rollup_5m every minute, catching up from the last committed bucket to the most recent sealed 5-minute bucket."

	watermarkRollup5m = "rollup-5m"
	bucketDuration5m  = 5 * time.Minute
	tableRollup5m     = "metric_rollup_5m"

	// rollup5mInitLookback bounds how far back the job scans on cold start
	// when traffic_event is empty. Matches merge-1h's 6h lookback so the
	// two stages stay roughly aligned on a quiet deployment.
	rollup5mInitLookback = 1 * time.Hour
)

// Rollup5mJob aggregates traffic_event rows into metric_rollup_5m.
// Each run catches up from the persisted watermark to the most recent
// sealed 5-minute bucket; per-bucket writes are idempotent via
// DELETE+INSERT in a single transaction that also advances the watermark,
// so a replica restarting mid-bucket produces the same output.
type Rollup5mJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// per-bucket aggregation transaction is testable via pgxmock.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
	// excludeInternalOpsFromBilled controls whether L2 embedding +
	// ai-guard classifier costs are EXCLUDED from MetricBilledCostUSD.
	// Default false (zero-value): include — internal-ops costs are real
	// money and roll into the quota-bearing billed total by default.
	// Flip true via yaml when the operator absorbs those costs and
	// doesn't want them tightening customer quotas.
	excludeInternalOpsFromBilled bool
}

// NewRollup5m constructs the job. interval defaults to 1 minute.
func NewRollup5m(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger, excludeInternalOpsFromBilled bool) *Rollup5mJob {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Rollup5mJob{
		pool:                         pool,
		interval:                     interval,
		logger:                       logger.With("job", rollup5mJobID),
		excludeInternalOpsFromBilled: excludeInternalOpsFromBilled,
	}
}

func (j *Rollup5mJob) ID() string              { return rollup5mJobID }
func (j *Rollup5mJob) Name() string            { return rollup5mJobName }
func (j *Rollup5mJob) Description() string     { return rollup5mJobDescription }
func (j *Rollup5mJob) Interval() time.Duration { return j.interval }

func (j *Rollup5mJob) Run(ctx context.Context) error {
	watermark, err := rollupstore.GetWatermark(ctx, j.pool, watermarkRollup5m)
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
		j.logger.Info("rollup completed", "buckets", count)
	}
	return nil
}

// coldStartWatermark picks the initial watermark when no row exists in
// rollup_watermark. It returns the earlier of (now - rollup5mInitLookback)
// truncated, and (earliestTrafficEvent - bucketDuration5m) truncated, so
// that when traffic_event already holds events older than the default
// lookback (e.g. after a fresh seed with historical timestamps), the
// aggregator covers them instead of dropping them silently.
func (j *Rollup5mJob) coldStartWatermark(ctx context.Context) time.Time {
	earliest, ok, err := rollupstore.EarliestTrafficEventTimestamp(ctx, j.pool)
	if err != nil {
		j.logger.Warn("earliest traffic_event lookup failed, using default lookback", "error", err)
		return pickColdStartWatermark(time.Now().UTC(), rollup5mInitLookback, bucketDuration5m, time.Time{}, false)
	}
	return pickColdStartWatermark(time.Now().UTC(), rollup5mInitLookback, bucketDuration5m, earliest, ok)
}

// processOneBucket aggregates traffic_event rows for a single 5-minute bucket
// and writes the rollup rows + watermark in a single transaction. Used by the
// live catch-up loop, which must advance the watermark.
func (j *Rollup5mJob) processOneBucket(ctx context.Context, bucketStart time.Time) error {
	return j.processBucket(ctx, bucketStart, true)
}

// processBucket aggregates one 5-minute bucket inside a single transaction.
// When writeWatermark is false (the correction backfill path) the live
// rollup-5m watermark is left untouched, so re-aggregating historical buckets
// does NOT rewind the live cursor and force the next live tick to re-scan the
// intervening buckets. The DELETE+INSERT still commits, so the
// backfill is durable and idempotent.
func (j *Rollup5mJob) processBucket(ctx context.Context, bucketStart time.Time, writeWatermark bool) error {
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := rollupstore.DeleteRollupBucket(ctx, tx, tableRollup5m, bucketStart); err != nil {
		return err
	}

	bucketEnd := bucketStart.Add(bucketDuration5m)
	rows, err := j.aggregateTrafficEvents(ctx, tx, bucketStart, bucketEnd)
	if err != nil {
		return err
	}

	if err := rollupstore.InsertRollupRows(ctx, tx, tableRollup5m, rows); err != nil {
		return err
	}

	if writeWatermark {
		if err := rollupstore.SetWatermark(ctx, tx, watermarkRollup5m, bucketStart); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// accKey5m is the composite key for value, histogram, and timestamp accumulators.
type accKey5m struct {
	metricName   string
	dimensionKey string
	subDimension string
}

// distinctKey5m is the composite key for distinct-count tracking.
type distinctKey5m struct {
	metricName   string
	dimensionKey string
	subDimension string
}

// aggregateTrafficEvents queries traffic_event for [start, end) and expands
// each row into rollup rows across all dimensions and sub-dimensions.
func (j *Rollup5mJob) aggregateTrafficEvents(ctx context.Context, tx pgx.Tx, start, end time.Time) ([]metrics.RollupRow, error) {
	// Dimension values are stable identifiers (UUIDs / opaque IDs), never
	// display names, so analytics queries survive renames and slug changes.
	// Display labels are joined in by the read-side handler at response time
	// (see admin_analytics_rollup.go).
	const q = `
		SELECT
			source, provider_id, model_id,
			entity_id, entity_type, org_id,
			routed_provider_id,
			routing_rule_id, target_host, source_ip,
			status_code, latency_ms,
			-- cache_hit_count reflects gateway-cache hits only via gateway_cache_status
			-- (not the legacy cache_status column which conflated gateway-cache hits
			-- with provider prompt-cache discounts, causing ~118x over-count).
			-- Provider prompt-cache discounts are tracked separately via
			-- requests_with_provider_prompt_cache_hit + cache_read_savings_usd.
			(gateway_cache_status IN ('hit', 'hit_inflight')) AS cache_hit,
			prompt_tokens, completion_tokens, total_tokens, estimated_cost_usd, gateway_cache_savings_usd,
			-- Dual-pipeline: hook_decision is split into request_hook_decision
			-- and response_hook_decision. Aggregate to a single "effective"
			-- decision in code: REJECT_HARD > BLOCK_SOFT > ERROR > APPROVE.
			request_hook_decision, response_hook_decision, bump_status,
			routed_model_id, model_id AS original_model_id,
			(CASE WHEN status_code < 400 AND details ? 'qualitySignals' THEN true ELSE false END) AS has_quality_signals,
			-- identity.vk is the Virtual Key; identity.apiCredential (NOT
			-- "credential") is the upstream provider's API key. An earlier
			-- producer-side rename moved VK info from identity.credential
			-- → identity.vk; this aggregator was missed in that sweep, so
			-- per-VK rollups were silently bucketed under NULL virtual_key_id.
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
	`

	rows, err := tx.Query(ctx, q, start, end)
	if err != nil {
		return nil, fmt.Errorf("query traffic_event: %w", err)
	}
	defer rows.Close()

	accValues := make(map[accKey5m]float64)
	accHisto := make(map[accKey5m]metrics.Histogram)
	accTimestamp := make(map[accKey5m]metrics.TimestampMeta)

	distinctEntities := make(map[distinctKey5m]map[string]struct{})
	distinctOrgs := make(map[distinctKey5m]map[string]struct{})
	distinctIPs := make(map[distinctKey5m]map[string]struct{})

	for rows.Next() {
		var (
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
			// Internal-ops costs (embedding L2 lookup + ai-guard classifier).
			// Rolled up as MetricEmbeddingCostUSD / MetricAIGuardCostUSD; not
			// added to MetricBilledCostUSD because internal ops are NOT
			// customer-billable.
			embeddingCostUsd *float64
			aiGuardCostUsd   *float64
		)

		if err := rows.Scan(
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
			return nil, fmt.Errorf("scan traffic_event: %w", err)
		}

		// Emit SubDimension using product-domain values (vk|proxy|agent) so
		// analytics queries that filter by UI tab match directly. Rows whose
		// DB source is outside the data-plane mapping (legacy admin, device
		// lifecycle) are skipped — they don't belong in data-plane rollups.
		srcDomain, ok := domain.DBSourceToDomain(deref5m(source))
		if !ok {
			continue
		}
		srcVal := string(srcDomain)

		// Feed the model dimension from routed_model_id, NOT model_id.
		// model_id is the REQUESTED side — the Model UUID the client asked
		// for, populated only when the request pinned a specific catalog
		// model and empty for "auto" / OpenAI-style. Analytics groups by the
		// model that actually HANDLED the call, so read the routed column to
		// keep the model= dimension fed with a stable UUID (the served Model
		// row) — the same answer every analytics surface wants. providerID /
		// modelID below the dim emission are kept only so the
		// latency_sum-by-dim path preserves its existing keying.
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
			j.emitEventMetrics(
				accValues, accHisto, accTimestamp,
				distinctEntities, distinctOrgs, distinctIPs,
				dimKey, subDim,
				statusCode, latencyMs, cacheHit,
				promptTokens, completionTokens, totalTokens, estimatedCost, gatewayCacheSavings,
				worstHookDecision(requestHookDecision, responseHookDecision), bumpStatus,
				providerID, routedProviderID, routedModelID, originalModelID,
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
		return nil, fmt.Errorf("iterate traffic_event: %w", err)
	}

	rollupRows := j.assembleRollupRows(start, accValues, accHisto, accTimestamp,
		distinctEntities, distinctOrgs, distinctIPs)
	return deduplicateRows5m(rollupRows), nil
}

// dimPair is one (name, value) dimension entry assembled for a single
// traffic_event row. Empty name represents the global dimension.
type dimPair struct {
	name  string
	value string
}

// buildEventDims assembles the dimension list for one traffic_event row.
// The first entry is always the global dimension (empty name/value). Typed
// dimensions are emitted only when their source value is non-empty.
//
// Every typed dimension stores a stable identifier — UUID where the source
// table has one (Provider, Model, Organization, Project, RoutingRule),
// opaque slug-as-id otherwise (NexusUser.id, target_host). Display names are
// looked up at read time by the analytics handler so a rename in the source
// row never invalidates historical rollup buckets.
//
// `project` comes from identity.project.id (request-context facet, like
// virtual_key), not from entity_type, so it fires even when the primary
// entity is a user or device.
// normalizeHookDecision collapses the worst of request/response hook decision
// values into a stable analytics bucket. Returns "" when neither side has a
// decision (so callers can skip emitting the dimension entirely).
//
// The five buckets ("allow"/"deny"/"error"/"unknown"/"") align with the
// per-decision counters in emitEventMetrics + emitThingEventMetrics so a
// dashboard reading request_count{dim=hook_decision} matches the same total
// the KPI counters would surface.
func normalizeHookDecision(req, resp string) string {
	pick := func(s string) string {
		switch s {
		case "APPROVE", "allow":
			return "allow"
		case "REJECT_HARD", "BLOCK_SOFT", "reject":
			return "deny"
		case "ERROR", "error":
			return "error"
		case "":
			return ""
		default:
			return "unknown"
		}
	}
	// `deny` wins over `error` wins over `unknown` wins over `allow` —
	// surface the worst outcome so a single allow followed by a deny is
	// counted under deny in the breakdown table.
	rank := map[string]int{"deny": 4, "error": 3, "unknown": 2, "allow": 1, "": 0}
	a, b := pick(req), pick(resp)
	if rank[a] >= rank[b] {
		return a
	}
	return b
}

func buildEventDims(
	providerID, modelID,
	entityID, entityType,
	orgID, routedProviderID,
	routingRuleID, targetHost,
	virtualKeyID, projectID,
	hookDecision string,
) []dimPair {
	dims := []dimPair{{name: "", value: ""}}
	// `provider` (the requested provider) used to be emitted here, but
	// OpenAI-style clients have no field for pinning a provider, so the
	// column on traffic_event is now always empty. Emitting an empty
	// provider= dimension produces no-op rows and confuses readers that
	// preferred provider-keyed series over the global total. The provider
	// that actually handled the call is captured by `routed_provider`
	// below — for analytics that is the canonical (and only) provider
	// dimension.
	if modelID != "" {
		dims = append(dims, dimPair{"model", modelID})
	}
	if entityID != "" {
		dims = append(dims, dimPair{"entity", entityID})
		switch entityType {
		case "user":
			dims = append(dims, dimPair{"user", entityID})
		case "project":
			dims = append(dims, dimPair{"project", entityID})
		case "device":
			dims = append(dims, dimPair{"device", entityID})
		}
	}
	if orgID != "" {
		dims = append(dims, dimPair{"organization", orgID})
	}
	if routedProviderID != "" {
		dims = append(dims, dimPair{"routed_provider", routedProviderID})
	}
	if routingRuleID != "" {
		dims = append(dims, dimPair{"routing_rule", routingRuleID})
	}
	if targetHost != "" {
		dims = append(dims, dimPair{"target_host", targetHost})
	}
	if virtualKeyID != "" {
		dims = append(dims, dimPair{"virtual_key", virtualKeyID})
	}
	if projectID != "" {
		dims = append(dims, dimPair{"project", projectID})
	}
	if hookDecision != "" {
		// Normalized to allow/deny/error/unknown by callers via
		// normalizeHookDecision. Powers the per-Thing Stats "Hook decisions"
		// breakdown on compliance-proxy nodes.
		dims = append(dims, dimPair{"hook_decision", hookDecision})
	}
	return dims
}

// deduplicateRows5m ensures no two rows share the same (metricName, dimensionKey, subDimension).
// Later rows overwrite earlier ones for the same key.
func deduplicateRows5m(rows []metrics.RollupRow) []metrics.RollupRow {
	type key struct{ m, d, s string }
	seen := make(map[key]int, len(rows))
	out := make([]metrics.RollupRow, 0, len(rows))
	for _, r := range rows {
		k := key{r.MetricName, r.DimensionKey, r.SubDimension}
		if idx, ok := seen[k]; ok {
			out[idx] = r
		} else {
			seen[k] = len(out)
			out = append(out, r)
		}
	}
	return out
}

// emitEventMetrics increments all relevant metric accumulators for one event
// in one dimension × sub-dimension cell.
func (j *Rollup5mJob) emitEventMetrics(
	accValues map[accKey5m]float64,
	accHisto map[accKey5m]metrics.Histogram,
	accTimestamp map[accKey5m]metrics.TimestampMeta,
	distinctEntities, distinctOrgs, distinctIPs map[distinctKey5m]map[string]struct{},
	dimKey, subDim string,
	statusCode *int, latencyMs *int, cacheHit *bool,
	promptTokens, completionTokens, totalTokens *int,
	estimatedCost *float64,
	gatewayCacheSavings *float64,
	hookDecision, bumpStatus *string,
	providerID, routedProviderID, routedModelID, originalModelID *string,
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
		accValues[accKey5m{metric, dimKey, subDim}] += v
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

	// GROSS metrics — every traffic_event row contributes.
	add(metrics.MetricPromptTokens, float64(derefInt5m(promptTokens)))
	add(metrics.MetricCompletionTokens, float64(derefInt5m(completionTokens)))
	add(metrics.MetricTotalTokens, float64(derefInt5m(totalTokens)))

	cost := derefFloat5m(estimatedCost)
	add(metrics.MetricEstimatedCostUSD, cost)
	if sc >= 400 {
		add(metrics.MetricWastedCostUSD, cost)
	}

	// BILLED metrics — only successful + non-cache-hit. quota.threshold and
	// ai-gateway usage_cache.Backfill read these.
	isSuccess := sc >= 200 && sc < 300 && deref5m(errorCode) == ""
	if isSuccess && !cacheHitVal {
		billed := cost
		// excludeInternalOpsFromBilled defaults false (include in billed).
		// Operator flips true to exclude — useful when the operator absorbs
		// those costs separately and doesn't want them tightening customer quotas.
		if !j.excludeInternalOpsFromBilled {
			billed += derefFloat5m(embeddingCostUsd) + derefFloat5m(aiGuardCostUsd)
		}
		add(metrics.MetricBilledCostUSD, billed)
		add(metrics.MetricBilledTokens, float64(derefInt5m(totalTokens)))
	}

	// Internal-ops cost: embedding (L2 lookup) + AI-Guard classifier.
	// Gross metrics — every contributing row counted. Always emitted as
	// separate dashboard series regardless of the billed-cost toggle above.
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

		hk := accKey5m{metrics.MetricLatencyHistogram, dimKey, subDim}
		h := accHisto[hk]
		idx := metrics.BucketForLatency(float64(lat))
		h[idx]++
		accHisto[hk] = h
	}

	// Latency phase aggregates. Mirrors the agent-local rollup pattern so
	// both Hub-side and agent-local consumers can compute averages as sum/count.
	// our_overhead = latency_ms - upstream_total_ms, clamped non-negative.
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

	// MetricRoutingFallback (routed_provider != provider) and
	// MetricModelShiftCount (routed_model != original_model) were both
	// originally emitted to flag remaps, but both comparisons now
	// degenerate: traffic_event.provider_id is always empty since the
	// requested-vs-routed split, and original_model_id (= model_id, the
	// literal client request) carries a code string that never matches
	// the resolved Model UUID in routed_model_id. The counters would
	// fire on every successful request, silently inflating dashboards.
	// Operators that need a "routing remap happened" signal can read
	// `routing_rule_name != 'passthrough-fallback'` off traffic_event
	// instead — the rule name is authoritative for that question.
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
		// No hook decision — skip.
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

	// L4 provider prompt cache metrics.
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

	// Normalisation pipeline metrics.
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
		tsKey := accKey5m{metrics.MetricFirstSeen, dimKey, subDim}
		existing := accTimestamp[tsKey]
		incoming := metrics.TimestampMeta{FirstSeen: tsStr, LastSeen: tsStr}
		accTimestamp[tsKey] = metrics.MergeTimestampMeta(existing, incoming)
	}

	trackDistinct := func(m map[distinctKey5m]map[string]struct{}, metricName, val string) {
		if val == "" {
			return
		}
		dk := distinctKey5m{metricName, dimKey, subDim}
		if m[dk] == nil {
			m[dk] = make(map[string]struct{})
		}
		m[dk][val] = struct{}{}
	}

	trackDistinct(distinctEntities, metrics.MetricActiveEntities, deref5m(entityID))
	trackDistinct(distinctOrgs, metrics.MetricActiveOrganizations, deref5m(orgID))
	trackDistinct(distinctIPs, metrics.MetricDistinctSources, deref5m(sourceIP))
}

// assembleRollupRows converts all accumulators into a flat slice of RollupRow.
func (j *Rollup5mJob) assembleRollupRows(
	bucketStart time.Time,
	accValues map[accKey5m]float64,
	accHisto map[accKey5m]metrics.Histogram,
	accTimestamp map[accKey5m]metrics.TimestampMeta,
	distinctEntities, distinctOrgs, distinctIPs map[distinctKey5m]map[string]struct{},
) []metrics.RollupRow {
	var out []metrics.RollupRow

	for k, v := range accValues {
		if v == 0 {
			continue
		}
		out = append(out, metrics.RollupRow{
			BucketStart:  bucketStart,
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
		out = append(out, metrics.RollupRow{
			BucketStart:  bucketStart,
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
		out = append(out, metrics.RollupRow{
			BucketStart:  bucketStart,
			MetricName:   k.metricName,
			DimensionKey: k.dimensionKey,
			SubDimension: k.subDimension,
			Metadata:     data,
		})
	}

	emitDistinct := func(m map[distinctKey5m]map[string]struct{}) {
		for dk, set := range m {
			if len(set) == 0 {
				continue
			}
			out = append(out, metrics.RollupRow{
				BucketStart:  bucketStart,
				MetricName:   dk.metricName,
				DimensionKey: dk.dimensionKey,
				SubDimension: dk.subDimension,
				Value:        float64(len(set)),
			})
		}
	}
	emitDistinct(distinctEntities)
	emitDistinct(distinctOrgs)
	emitDistinct(distinctIPs)

	return out
}

// Pointer deref helpers, scoped to this file to avoid polluting the jobs
// package (other jobs may ship their own if they grow similar scan helpers).

func deref5m(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt5m(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefFloat5m(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefBool5m(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func derefInt645m(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// worstHookDecision collapses request- and response-stage decisions into a
// single effective decision for rollup hook-counter buckets. Priority is
// REJECT_HARD > BLOCK_SOFT > ERROR > APPROVE > "" (no hook ran). Returns a
// pointer so the caller can pass it through the existing *string interface.
func worstHookDecision(req, resp *string) *string {
	rank := func(s string) int {
		switch s {
		case "REJECT_HARD":
			return 4
		case "BLOCK_SOFT":
			return 3
		case "ERROR":
			return 2
		case "APPROVE":
			return 1
		default:
			return 0
		}
	}
	a := deref5m(req)
	b := deref5m(resp)
	if rank(a) >= rank(b) {
		if a == "" {
			return nil
		}
		return &a
	}
	if b == "" {
		return nil
	}
	return &b
}
