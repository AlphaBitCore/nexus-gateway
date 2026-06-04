package analytics

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/analytics/analyticsstore"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

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
