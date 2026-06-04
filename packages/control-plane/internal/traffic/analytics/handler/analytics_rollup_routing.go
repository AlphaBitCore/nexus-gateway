package analytics

import (
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/analytics/analyticsstore"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

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
