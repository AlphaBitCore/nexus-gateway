package analytics

import (
	"context"
	"encoding/json"
	"fmt"
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

// queryMetricsOrFallback tries the rollup tables first; returns nil if rollup
// has no data, signalling the caller to use the legacy raw query.
//
// Routing:
//   - TimeSeries: true  → QueryRollupAware: picks a single granularity table
//     (SelectGranularity) and fills the recent unsettled tail from the
//     next-finer table. Mixed granularities in one time-series would produce
//     inconsistent bucket intervals on charts, so a single consistent
//     granularity is required here.
//   - TimeSeries: false → QueryRollupCascade: walks 1mo→1d→1h→5m, splitting
//     at each merge watermark. Granularity is irrelevant for aggregate totals
//     and group-by sums; the cascade gives complete coverage even when the
//     coarser tables only hold partial history (e.g. early in deployment).
func (h *Handler) queryMetricsOrFallback(ctx context.Context, q metrics.MetricsQuery) (*metrics.MetricsResult, error) {
	var rows []metrics.RollupRow
	var err error
	if q.TimeSeries {
		rows, err = h.metrics.QueryRollupAware(ctx, q)
	} else {
		rows, err = h.metrics.QueryRollupCascade(ctx, q)
	}
	if err == nil && len(rows) > 0 {
		gran := metrics.SelectGranularity(q.StartTime, q.EndTime)
		return metrics.BuildResult(q, rows, gran), nil
	}
	return nil, nil
}

// rollupGroupsToGroupByResults converts MetricsGroups from a rollup result
// into the legacy []store.GroupByResult format expected by the analytics
// response shape.
//
// dimName is the rollup dimension being grouped on (e.g. "provider",
// "model"). It drives [resolveDimensionLabels] so the response carries
// both the stable ID (`Group`) and a human-readable label (`GroupLabel`)
// looked up from the source table at read time. sumFields selects the
// metric subset to copy onto each row.
func (h *Handler) rollupGroupsToGroupByResults(ctx context.Context, dimName string, groups []metrics.MetricsGroup, sumFields string) []analyticsstore.GroupByResult {
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		_, val := metrics.ParseDimensionKey(g.DimensionKey)
		if val != "" {
			ids = append(ids, val)
		}
	}
	labels := h.resolveDimensionLabels(ctx, dimName, ids)

	results := make([]analyticsstore.GroupByResult, 0, len(groups))
	for _, g := range groups {
		_, val := metrics.ParseDimensionKey(g.DimensionKey)
		r := analyticsstore.GroupByResult{
			Group:        val,
			GroupLabel:   labels[val],
			RequestCount: int(g.Values[metrics.MetricRequestCount]),
		}
		switch sumFields {
		case "tokens":
			r.TotalPromptTokens = int64(g.Values[metrics.MetricPromptTokens])
			r.TotalCompletionTokens = int64(g.Values[metrics.MetricCompletionTokens])
			r.TotalTokens = int64(g.Values[metrics.MetricTotalTokens])
		case "cost":
			r.TotalCostUsd = g.Values[metrics.MetricEstimatedCostUSD]
			r.TotalTokens = int64(g.Values[metrics.MetricTotalTokens])
			r.CacheHitCount = int(g.Values[metrics.MetricCacheHitCount])
			r.GatewayCacheSavingsUsd = g.Values[metrics.MetricCacheSavedCostUSD]
		case "provider":
			if lc := g.Values[metrics.MetricLatencyCount]; lc > 0 {
				r.AvgLatencyMs = g.Values[metrics.MetricLatencySum] / lc
			}
			r.TotalTokens = int64(g.Values[metrics.MetricTotalTokens])
			r.TotalEstimatedCostUsd = g.Values[metrics.MetricEstimatedCostUSD]
		}
		results = append(results, r)
	}
	return results
}

// applyTopN sorts results by RequestCount DESC, keeps the top N, and
// aggregates the remainder into a single "Other" row. If results <= topN,
// returns unchanged.
func applyTopN(results []analyticsstore.GroupByResult, topN int) []analyticsstore.GroupByResult {
	if len(results) <= topN {
		return results
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].RequestCount > results[j].RequestCount
	})
	top := results[:topN]
	other := analyticsstore.GroupByResult{Group: "Other", GroupLabel: "Other"}
	for _, r := range results[topN:] {
		other.RequestCount += r.RequestCount
		other.TotalTokens += r.TotalTokens
		other.TotalPromptTokens += r.TotalPromptTokens
		other.TotalCompletionTokens += r.TotalCompletionTokens
		other.TotalCostUsd += r.TotalCostUsd
		other.TotalEstimatedCostUsd += r.TotalEstimatedCostUsd
	}
	return append(top, other)
}

// rollupDimensionForGroupKey maps the legacy store groupKey to the rollup
// dimension name used in MetricsQuery.DimensionKey.
//
// "provider" and "routedProvider" both resolve to the rollup's
// `routed_provider` dimension — there is only one provider per request
// (the one that actually handled it) since OpenAI-style clients can't
// pin a provider. The legacy `provider=` rollup dimension was retired
// in the same change that consolidates this mapping.
var rollupDimensionForGroupKey = map[string]string{
	"provider":       "routed_provider",
	"modelUsed":      "model",
	"projectId":      "project",
	"organizationId": "organization",
	"userId":         "user",
	"virtualKeyId":   "virtual_key",
	"routedProvider": "routed_provider",
	"routingRuleId":  "routing_rule",
	"targetHost":     "target_host",
	"host":           "target_host",
	"deviceId":       "device",
}

// rollupSubDimensionForGroupKey returns the sub-dimension filter to use for
// rollup queries based on the legacy group key.
func rollupSubDimensionForGroupKey(c echo.Context, groupKey string) string {
	return sourceSubDimension(c)
}

// sourceSubDimension reads the "source" query param and returns the
// appropriate SubDimension filter. Empty means all sources.
func sourceSubDimension(c echo.Context) string {
	src := c.QueryParam("source")
	switch src {
	case "vk", "proxy", "agent":
		return "source=" + src
	default:
		return "" // all sources
	}
}

// rollupDefaultTimeRange provides the default window when no time range is
// specified. Uses a 365-day lookback so the dashboard always shows all
// available rollup data (metric_rollup_1d retains 365 days). Callers that
// need a short window (e.g. live traffic pages) always supply explicit params.
func rollupDefaultTimeRange(start, end *time.Time, _ *time.Location) (time.Time, time.Time) {
	if start != nil && end != nil {
		return *start, *end
	}
	now := time.Now().UTC()
	return now.AddDate(-1, 0, 0), now
}

// tryRollupSummary attempts to serve AnalyticsSummary from rollup data.
// Returns true if rollup data was found and response was written.
func (h *Handler) tryRollupSummary(c echo.Context) bool {
	start, end := parseTimeRange(c)
	s, e := rollupDefaultTimeRange(start, end, tzLoc(c))

	q := metrics.MetricsQuery{
		Metrics: []string{
			metrics.MetricRequestCount, metrics.MetricStatus4xxCount, metrics.MetricStatus5xxCount,
			metrics.MetricTotalTokens, metrics.MetricPromptTokens, metrics.MetricCompletionTokens,
			metrics.MetricEstimatedCostUSD, metrics.MetricLatencySum, metrics.MetricLatencyCount,
			metrics.MetricLatencyHistogram, metrics.MetricCacheHitCount,
		},
		SubDimension: sourceSubDimension(c),
		StartTime:    s,
		EndTime:      e,
	}
	result, _ := h.queryMetricsOrFallback(c.Request().Context(), q)
	if result == nil {
		return false
	}

	as := &analyticsstore.AnalyticsSummary{
		TotalRequests:         int(result.Summary[metrics.MetricRequestCount]),
		ErrorCount:            int(result.Summary[metrics.MetricStatus4xxCount] + result.Summary[metrics.MetricStatus5xxCount]),
		TotalTokens:           int64(result.Summary[metrics.MetricTotalTokens]),
		TotalPromptTokens:     int64(result.Summary[metrics.MetricPromptTokens]),
		TotalCompletionTokens: int64(result.Summary[metrics.MetricCompletionTokens]),
		TotalEstimatedCostUsd: result.Summary[metrics.MetricEstimatedCostUSD],
	}
	if lc := result.Summary[metrics.MetricLatencyCount]; lc > 0 {
		as.AvgLatencyMs = result.Summary[metrics.MetricLatencySum] / lc
	}
	if as.TotalRequests > 0 {
		as.ErrorRate = float64(as.ErrorCount) / float64(as.TotalRequests)
	}

	// P95 latency from histogram metadata.
	if result.Metadata != nil {
		if histRaw, ok := result.Metadata[metrics.MetricLatencyHistogram]; ok {
			if hist, ok2 := histRaw.(metrics.Histogram); ok2 {
				as.P95LatencyMs = hist.Percentile(0.95)
			}
		}
	}

	// Cache hit rate.
	requests := result.Summary[metrics.MetricRequestCount]
	cacheHits := result.Summary[metrics.MetricCacheHitCount]
	if requests > 0 {
		as.CacheHitRate = cacheHits / requests
	}

	// Phase P95s computed directly from traffic_event since the rollup pipeline
	// doesn't carry per-phase histograms yet. One small extra query — fine for
	// the summary surface that polls every ~5s.
	if usP95, ttfbP95, totalP95, ok := h.queryAnalyticsPhasePercentiles(c.Request().Context(), s, e, sourceFilterParam(c)); ok {
		as.UsOverheadP95Ms = usP95
		as.UpstreamTtfbP95Ms = ttfbP95
		as.UpstreamTotalP95Ms = totalP95
	}

	_ = c.JSON(http.StatusOK, as)
	return true
}

// queryAnalyticsPhasePercentiles computes the three phase P95s used by
// AnalyticsSummary against traffic_event directly. Returns ok=false (and
// nil pointers) when the window has no qualifying rows so the caller
// leaves the fields nil → SQL NULL on the wire → UI shows "—".
func (h *Handler) queryAnalyticsPhasePercentiles(ctx context.Context, start, end time.Time, source string) (us, ttfb, total *int, ok bool) {
	var sourceFilter string
	args := []any{start, end}
	if source != "" && source != "all" {
		sourceFilter = " AND source = $3"
		args = append(args, source)
	}
	q := `
		SELECT
			(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY GREATEST(0, COALESCE(latency_ms,0) - COALESCE(upstream_total_ms,0))))::int AS us_p95,
			(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY upstream_ttfb_ms)  FILTER (WHERE upstream_ttfb_ms  IS NOT NULL))::int AS ttfb_p95,
			(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY upstream_total_ms) FILTER (WHERE upstream_total_ms IS NOT NULL))::int AS total_p95
		FROM   traffic_event
		WHERE  timestamp >= $1 AND timestamp < $2` + sourceFilter
	var usP95, ttfbP95, totalP95 *int
	if err := h.pool.QueryRow(ctx, q, args...).Scan(&usP95, &ttfbP95, &totalP95); err != nil {
		return nil, nil, nil, false
	}
	return usP95, ttfbP95, totalP95, true
}

// sourceFilterParam reads ?source= and normalises to the canonical
// traffic_event.source enum value (or "all" for unfiltered queries).
func sourceFilterParam(c echo.Context) string {
	s := c.QueryParam("source")
	switch s {
	case "vk", "ai-gateway":
		return "ai-gateway"
	case "proxy", "compliance-proxy":
		return "compliance-proxy"
	case "agent":
		return "agent"
	default:
		return "all"
	}
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

// tryRollupRouting attempts to serve AnalyticsRouting from rollup data.
// Returns a slice of RoutingDistribution (or nil if rollup has no data).
func (h *Handler) tryRollupRouting(c echo.Context) []analyticsstore.RoutingDistribution {
	startP, endP := parseTimeRange(c)
	start, end := rollupDefaultTimeRange(startP, endP, tzLoc(c))

	q := metrics.MetricsQuery{
		Metrics:      []string{metrics.MetricRequestCount},
		DimensionKey: "routed_provider",
		SubDimension: sourceSubDimension(c),
		StartTime:    start,
		EndTime:      end,
	}
	result, _ := h.queryMetricsOrFallback(c.Request().Context(), q)
	if result == nil {
		return nil
	}

	data := make([]analyticsstore.RoutingDistribution, 0, len(result.Groups))
	for _, g := range result.Groups {
		_, val := metrics.ParseDimensionKey(g.DimensionKey)
		provider := val
		count := int(g.Values[metrics.MetricRequestCount])
		data = append(data, analyticsstore.RoutingDistribution{
			Provider:     &provider,
			RequestCount: count,
		})
	}
	return data
}

// tryRollupRoutingFallbacks attempts to serve AnalyticsRoutingFallbacks from
// rollup data. Returns a slice of GroupByResult (or nil if rollup has no data).
func (h *Handler) tryRollupRoutingFallbacks(c echo.Context) []analyticsstore.GroupByResult {
	startP, endP := parseTimeRange(c)
	start, end := rollupDefaultTimeRange(startP, endP, tzLoc(c))

	q := metrics.MetricsQuery{
		Metrics:      []string{metrics.MetricRoutingRuleHit},
		DimensionKey: "routing_rule",
		SubDimension: sourceSubDimension(c),
		StartTime:    start,
		EndTime:      end,
	}
	result, _ := h.queryMetricsOrFallback(c.Request().Context(), q)
	if result == nil {
		return nil
	}

	results := make([]analyticsstore.GroupByResult, 0, len(result.Groups))
	for _, g := range result.Groups {
		_, val := metrics.ParseDimensionKey(g.DimensionKey)
		results = append(results, analyticsstore.GroupByResult{
			Group:        val,
			RequestCount: int(g.Values[metrics.MetricRoutingRuleHit]),
		})
	}
	return results
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
