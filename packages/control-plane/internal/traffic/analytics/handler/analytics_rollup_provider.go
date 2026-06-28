package analytics

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"sort"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/analytics/analyticsstore"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// metricsAggRow is the response row format for /metrics/aggregates,
// matching the legacy MetricAggregateRow shape expected by the Metrics UI.
type metricsAggRow struct {
	BucketStart  time.Time       `json:"bucketStart"`
	MetricName   string          `json:"metricName"`
	DimensionKey string          `json:"dimensionKey"`
	Dimensions   json.RawMessage `json:"dimensions,omitempty"`
	Value        string          `json:"value"`
}

// tryRollupByProvider attempts to serve AnalyticsByProvider from rollup data.
//
// "Provider" here means the provider that actually handled the call —
// queried off the `routed_provider=...` dimension. The legacy `provider=...`
// dimension was the requested provider, which OpenAI-style traffic never
// fills, so reading from it returns frozen pre-fix data and undercounts.
func (h *Handler) tryRollupByProvider(c echo.Context) bool {
	startP, endP := parseTimeRange(c)
	start, end := rollupDefaultTimeRange(startP, endP, tzLoc(c))

	q := metrics.MetricsQuery{
		Metrics: []string{
			metrics.MetricRequestCount, metrics.MetricTotalTokens,
			metrics.MetricEstimatedCostUSD, metrics.MetricLatencySum, metrics.MetricLatencyCount,
		},
		DimensionKey: "routed_provider",
		SubDimension: sourceSubDimension(c),
		StartTime:    start,
		EndTime:      end,
	}
	result, _ := h.queryMetricsOrFallback(c.Request().Context(), q)
	if result == nil {
		return false
	}

	groups := h.rollupGroupsToGroupByResults(c.Request().Context(), "provider", result.Groups, "provider")
	// Per-provider phase P95 backfill from traffic_event directly, since the
	// rollup pipeline doesn't carry per-phase histograms yet.
	phaseByProvider := h.queryByProviderPhasePercentiles(c.Request().Context(), start, end, sourceFilterParam(c))
	mapped := make([]map[string]any, len(groups))
	for i, d := range groups {
		row := map[string]any{
			"provider":              d.Group,
			"providerLabel":         d.GroupLabel,
			"requestCount":          d.RequestCount,
			"avgLatencyMs":          d.AvgLatencyMs,
			"totalTokens":           d.TotalTokens,
			"totalEstimatedCostUsd": d.TotalEstimatedCostUsd,
		}
		if p, ok := phaseByProvider[d.GroupLabel]; ok {
			if p.UsP95 != nil {
				row["usOverheadP95Ms"] = *p.UsP95
			}
			if p.TtfbP95 != nil {
				row["upstreamTtfbP95Ms"] = *p.TtfbP95
			}
			if p.TotalP95 != nil {
				row["upstreamTotalP95Ms"] = *p.TotalP95
			}
		}
		mapped[i] = row
	}

	_ = c.JSON(http.StatusOK, map[string]any{"data": mapped})
	return true
}

// byProviderPhase carries the three phase P95s per provider for the
// AnalyticsByProvider response. Keyed by provider name (the same string
// the rollup pipeline emits as group_label).
type byProviderPhase struct {
	UsP95    *int
	TtfbP95  *int
	TotalP95 *int
}

// queryByProviderPhasePercentiles runs a single GROUP BY scan against
// traffic_event to compute the three phase P95s per routed_provider_name.
// Used as a side-channel augmentation for tryRollupByProvider until the
// rollup pipeline carries per-phase histograms natively.
func (h *Handler) queryByProviderPhasePercentiles(ctx context.Context, start, end time.Time, source string) map[string]byProviderPhase {
	var sourceFilter string
	args := []any{start, end}
	if source != "" && source != "all" {
		sourceFilter = " AND source = $3"
		args = append(args, source)
	}
	q := `
		SELECT
			COALESCE(routed_provider_name, provider_name, 'unknown') AS provider,
			(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY GREATEST(0, COALESCE(latency_ms,0) - COALESCE(upstream_total_ms,0))))::int AS us_p95,
			(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY upstream_ttfb_ms)  FILTER (WHERE upstream_ttfb_ms  IS NOT NULL))::int AS ttfb_p95,
			(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY upstream_total_ms) FILTER (WHERE upstream_total_ms IS NOT NULL))::int AS total_p95
		FROM   traffic_event
		WHERE  timestamp >= $1 AND timestamp < $2` + sourceFilter + `
		GROUP  BY provider`
	rows, err := h.pool.Query(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[string]byProviderPhase, 16)
	for rows.Next() {
		var provider string
		var p byProviderPhase
		if err := rows.Scan(&provider, &p.UsP95, &p.TtfbP95, &p.TotalP95); err != nil {
			continue
		}
		out[provider] = p
	}
	return out
}

// tryRollupGroupBy attempts to serve a group-by analytics query (usage, cost,
// cost-report) from rollup data. sumFields is "tokens" or "cost".
func (h *Handler) tryRollupGroupBy(c echo.Context, groupKey, sumFields string) ([]analyticsstore.GroupByResult, bool) {
	startP, endP := parseTimeRange(c)
	start, end := rollupDefaultTimeRange(startP, endP, tzLoc(c))

	dimKey, ok := rollupDimensionForGroupKey[groupKey]
	if !ok || dimKey == "" {
		return nil, false
	}

	var metricNames []string
	switch sumFields {
	case "tokens":
		metricNames = []string{
			metrics.MetricRequestCount, metrics.MetricPromptTokens,
			metrics.MetricCompletionTokens, metrics.MetricTotalTokens,
		}
	case "cost":
		metricNames = []string{
			metrics.MetricRequestCount, metrics.MetricEstimatedCostUSD,
			metrics.MetricTotalTokens, metrics.MetricCacheHitCount,
			metrics.MetricCacheSavedCostUSD,
		}
	default:
		metricNames = []string{metrics.MetricRequestCount}
	}

	q := metrics.MetricsQuery{
		Metrics:      metricNames,
		DimensionKey: dimKey,
		SubDimension: rollupSubDimensionForGroupKey(c, groupKey),
		StartTime:    start,
		EndTime:      end,
	}
	result, _ := h.queryMetricsOrFallback(c.Request().Context(), q)
	if result == nil {
		return nil, false
	}

	return h.rollupGroupsToGroupByResults(c.Request().Context(), dimKey, result.Groups, sumFields), true
}

// tryRollupMetricsAggregates attempts to serve MetricsAggregates from rollup
// time-series data.
func (h *Handler) tryRollupMetricsAggregates(c echo.Context) bool {
	startP, endP := parseTimeRange(c)
	start, end := rollupDefaultTimeRange(startP, endP, tzLoc(c))

	// Query raw rollup rows (not via BuildResult) so we can do custom name
	// mapping and histogram→P50 conversion per bucket.
	q := metrics.MetricsQuery{
		Metrics: []string{
			metrics.MetricRequestCount,
			metrics.MetricStatus4xxCount, metrics.MetricStatus5xxCount,
			metrics.MetricTotalTokens,
			metrics.MetricEstimatedCostUSD,
			metrics.MetricCacheHitCount,
			metrics.MetricCacheSavedCostUSD,
			metrics.MetricLatencyHistogram,
			// Phase metrics emitted by Hub rollup_5m. The "Latency Phase
			// Breakdown Over Time" chart filters on these names
			// (latency_us_sum / latency_us_count / latency_upstream_ttfb_* /
			// latency_upstream_total_* / latency_hooks_*). Omitting them
			// leaves the chart blank even when the rollup table has rows.
			metrics.MetricLatencyUsSum, metrics.MetricLatencyUsCount,
			metrics.MetricLatencyUpstreamTtfbSum, metrics.MetricLatencyUpstreamTtfbCount,
			metrics.MetricLatencyUpstreamSum, metrics.MetricLatencyUpstreamCount,
			metrics.MetricLatencyHooksSum, metrics.MetricLatencyHooksCount,
		},
		SubDimension: sourceSubDimension(c),
		StartTime:    start,
		EndTime:      end,
	}
	rows, err := h.metrics.QueryRollup(c.Request().Context(), q)
	if err != nil || len(rows) == 0 {
		return false
	}

	// Map rollup metric names → legacy UI metric names. Phase metrics
	// pass through with their canonical names (already what the chart
	// reads) so no entry is needed here for them.
	legacyName := map[string]string{
		metrics.MetricRequestCount:      "request_count",
		metrics.MetricTotalTokens:       "token_usage",
		metrics.MetricEstimatedCostUSD:  "estimated_cost",
		metrics.MetricCacheHitCount:     "cache_hits",
		metrics.MetricCacheSavedCostUSD: "cache_saved_cost",
		// Phase sums + counts pass through unchanged — the chart reads
		// these exact names from the response.
		metrics.MetricLatencyUsSum:             metrics.MetricLatencyUsSum,
		metrics.MetricLatencyUsCount:           metrics.MetricLatencyUsCount,
		metrics.MetricLatencyUpstreamTtfbSum:   metrics.MetricLatencyUpstreamTtfbSum,
		metrics.MetricLatencyUpstreamTtfbCount: metrics.MetricLatencyUpstreamTtfbCount,
		metrics.MetricLatencyUpstreamSum:       metrics.MetricLatencyUpstreamSum,
		metrics.MetricLatencyUpstreamCount:     metrics.MetricLatencyUpstreamCount,
		metrics.MetricLatencyHooksSum:          metrics.MetricLatencyHooksSum,
		metrics.MetricLatencyHooksCount:        metrics.MetricLatencyHooksCount,
	}

	// Group rows by bucket, accumulate values and histograms.
	type bucketAcc struct {
		values map[string]float64
		errors float64
		histo  metrics.Histogram
	}
	buckets := make(map[string]*bucketAcc)
	var order []string
	bucketTimes := make(map[string]time.Time)

	for _, r := range rows {
		key := r.BucketStart.UTC().Format(time.RFC3339)
		acc, ok := buckets[key]
		if !ok {
			acc = &bucketAcc{values: make(map[string]float64)}
			buckets[key] = acc
			order = append(order, key)
			bucketTimes[key] = r.BucketStart
		}

		switch r.MetricName {
		case metrics.MetricStatus4xxCount, metrics.MetricStatus5xxCount:
			acc.errors += r.Value
		case metrics.MetricLatencyHistogram:
			if h, err := metrics.ParseHistogramMetadata(r.Metadata); err == nil {
				acc.histo = metrics.MergeHistograms(acc.histo, h)
			}
		default:
			if ln, ok := legacyName[r.MetricName]; ok {
				acc.values[ln] += r.Value
			}
		}
	}

	// Also query per-provider dimension for provider-level charts.
	// "Provider" here means the provider that actually handled the call
	// (routed_provider). The legacy `provider=` dimension was the
	// requested provider, which OpenAI-style traffic never sets — see
	// tryRollupByProvider for the same rationale.
	qProv := metrics.MetricsQuery{
		Metrics: []string{
			metrics.MetricRequestCount,
			metrics.MetricTotalTokens,
			metrics.MetricEstimatedCostUSD,
			metrics.MetricLatencyHistogram,
		},
		DimensionKey: "routed_provider",
		SubDimension: sourceSubDimension(c),
		StartTime:    start,
		EndTime:      end,
	}
	provRows, _ := h.metrics.QueryRollup(c.Request().Context(), qProv)

	// Translate provider IDs (rollup dim values are stable UUIDs) into
	// display names so chart legends read "OpenAI", not a UUID. Empty
	// labels fall through to the raw ID — that's a clearer failure mode
	// than swallowing the row.
	provIDSet := make(map[string]struct{}, len(provRows))
	for _, r := range provRows {
		_, id := metrics.ParseDimensionKey(r.DimensionKey)
		if id != "" {
			provIDSet[id] = struct{}{}
		}
	}
	provIDs := make([]string, 0, len(provIDSet))
	for id := range provIDSet {
		provIDs = append(provIDs, id)
	}
	provLabels := h.resolveDimensionLabels(c.Request().Context(), "provider", provIDs)

	// Emit output rows in legacy MetricAggregateRow format.
	sort.Strings(order)
	var data []metricsAggRow

	for _, key := range order {
		acc := buckets[key]
		bt := bucketTimes[key]

		for metricName, val := range acc.values {
			data = append(data, metricsAggRow{
				BucketStart: bt, MetricName: metricName, DimensionKey: "", Value: formatFloat(val),
			})
		}
		if acc.errors > 0 {
			data = append(data, metricsAggRow{
				BucketStart: bt, MetricName: "error_count", DimensionKey: "", Value: formatFloat(acc.errors),
			})
		}
		p50 := acc.histo.Percentile(0.50)
		if p50 > 0 {
			data = append(data, metricsAggRow{
				BucketStart: bt, MetricName: "latency_p50", DimensionKey: "", Value: formatFloat(p50),
			})
		}
	}

	// Emit per-provider rows.
	for _, r := range provRows {
		_, provID := metrics.ParseDimensionKey(r.DimensionKey)
		if provID == "" {
			continue
		}
		label := provLabels[provID]
		if label == "" {
			label = provID
		}
		dim := buildProviderDim(provID, label)
		dimKey := "routed_provider:" + provID
		switch r.MetricName {
		case metrics.MetricRequestCount:
			data = append(data, metricsAggRow{
				BucketStart: r.BucketStart, MetricName: "request_count", DimensionKey: dimKey,
				Dimensions: dim, Value: formatFloat(r.Value),
			})
		case metrics.MetricTotalTokens:
			data = append(data, metricsAggRow{
				BucketStart: r.BucketStart, MetricName: "token_usage", DimensionKey: dimKey,
				Dimensions: dim, Value: formatFloat(r.Value),
			})
		case metrics.MetricEstimatedCostUSD:
			data = append(data, metricsAggRow{
				BucketStart: r.BucketStart, MetricName: "estimated_cost", DimensionKey: dimKey,
				Dimensions: dim, Value: formatFloat(r.Value),
			})
		case metrics.MetricLatencyHistogram:
			if h, err := metrics.ParseHistogramMetadata(r.Metadata); err == nil {
				p50 := h.Percentile(0.50)
				if p50 > 0 {
					data = append(data, metricsAggRow{
						BucketStart: r.BucketStart, MetricName: "latency_p50", DimensionKey: dimKey,
						Dimensions: dim, Value: formatFloat(p50),
					})
				}
			}
		}
	}

	_ = c.JSON(http.StatusOK, map[string]any{"data": data})
	return true
}

// tryRollupQuality attempts to serve AnalyticsQuality from rollup data.
// Returns true if rollup data was found and response was written.
func (h *Handler) tryRollupQuality(c echo.Context) bool {
	startP, endP := parseTimeRange(c)
	start, end := rollupDefaultTimeRange(startP, endP, tzLoc(c))

	q := metrics.MetricsQuery{
		Metrics:      []string{metrics.MetricStatus2xxCount, metrics.MetricQualityAnomalyCount},
		SubDimension: sourceSubDimension(c),
		StartTime:    start,
		EndTime:      end,
	}
	result, _ := h.queryMetricsOrFallback(c.Request().Context(), q)
	if result == nil {
		return false
	}

	totalResponses := int(result.Summary[metrics.MetricStatus2xxCount])
	anomalyCount := int(result.Summary[metrics.MetricQualityAnomalyCount])
	var anomalyRate float64
	if totalResponses > 0 {
		anomalyRate = float64(anomalyCount) / float64(totalResponses)
	}

	_ = c.JSON(http.StatusOK, map[string]any{
		"totalResponses": totalResponses,
		"anomalyCount":   anomalyCount,
		"anomalyRate":    anomalyRate,
	})
	return true
}

// formatFloat converts a float64 to a compact string representation,
// matching the legacy MetricAggregateRow.Value format (%g).
func formatFloat(v float64) string {
	return fmt.Sprintf("%g", v)
}

// buildProviderDim emits the `dimensions` JSON column for a per-provider
// metric row. Both `provider` (stable UUID for joining / filtering) and
// `providerLabel` (display name for chart legends) are included so the
// frontend can render the human label without a second round-trip.
func buildProviderDim(id, label string) []byte {
	out, err := json.Marshal(map[string]string{
		"provider":      id,
		"providerLabel": label,
	})
	if err != nil {
		// Fall back to the bare ID; never block a metrics row on a marshal
		// edge case (string fields cannot fail in practice).
		return []byte(`{"provider":"` + id + `"}`)
	}
	return out
}

// tryRollupCostReport attempts to serve AnalyticsCostReport from rollup data.
// Returns a slice of GroupByResult (or nil if rollup has no data).
func (h *Handler) tryRollupCostReport(c echo.Context) []analyticsstore.GroupByResult {
	startP, endP := parseTimeRange(c)
	start, end := rollupDefaultTimeRange(startP, endP, tzLoc(c))

	q := metrics.MetricsQuery{
		Metrics: []string{
			metrics.MetricRequestCount, metrics.MetricEstimatedCostUSD,
			metrics.MetricTotalTokens,
		},
		DimensionKey: "organization",
		SubDimension: sourceSubDimension(c),
		StartTime:    start,
		EndTime:      end,
	}
	result, _ := h.queryMetricsOrFallback(c.Request().Context(), q)
	if result == nil {
		return nil
	}

	return h.rollupGroupsToGroupByResults(c.Request().Context(), "organization", result.Groups, "cost")
}
