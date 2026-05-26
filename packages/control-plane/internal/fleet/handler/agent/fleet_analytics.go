package agent

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/settings/store/metricsstore"
	metricspkg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterFleetAnalyticsRoutes mounts fleet-wide analytics endpoints.
func (h *Handler) RegisterFleetAnalyticsRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/fleet-analytics/summary", h.FleetAnalyticsSummary, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/fleet-analytics/trends", h.FleetAnalyticsTrends, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/fleet-analytics/top-destinations", h.FleetAnalyticsTopDest, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
}

// FleetAnalyticsSummary returns the current fleet health snapshot.
func (h *Handler) FleetAnalyticsSummary(c echo.Context) error {
	health, err := h.agents.GetAgentFleetHealth(c.Request().Context())
	if err != nil {
		return internalServerError(c, "Internal server error")
	}
	return c.JSON(http.StatusOK, health)
}

// FleetAnalyticsTrends returns hourly metric rollup buckets for the requested
// metric (default: device_fleet_status) over the last 168 h.
func (h *Handler) FleetAnalyticsTrends(c echo.Context) error {
	metric := c.QueryParam("metric")
	if metric == "" {
		metric = "device_fleet_status"
	}
	buckets, err := h.metrics.ListMetricRollupBuckets(c.Request().Context(), metric, 168)
	if err != nil {
		return internalServerError(c, "Internal server error")
	}
	return c.JSON(http.StatusOK, map[string]any{"metric": metric, "buckets": buckets})
}

// FleetAnalyticsTopDest returns the top-50 agent destination hosts by
// request count over the last 24 h.
func (h *Handler) FleetAnalyticsTopDest(c echo.Context) error {
	const windowHours = 24
	const limit = 50

	now := time.Now().UTC()
	windowStart := now.Add(-time.Duration(windowHours) * time.Hour)
	q := metricspkg.MetricsQuery{
		Metrics:      []string{metricspkg.MetricRequestCount, metricspkg.MetricActiveEntities},
		DimensionKey: "target_host",
		SubDimension: "source=agent",
		StartTime:    windowStart,
		EndTime:      now,
		TopN:         limit,
	}
	result, _ := h.queryMetricsOrFallback(c.Request().Context(), q)
	if result != nil {
		var out []metricsstore.TopDestination
		for _, g := range result.Groups {
			_, host := metricspkg.ParseDimensionKey(g.DimensionKey)
			out = append(out, metricsstore.TopDestination{
				DestHost:    host,
				EventCount:  int(g.Values[metricspkg.MetricRequestCount]),
				DeviceCount: int(g.Values[metricspkg.MetricActiveEntities]),
			})
		}
		return c.JSON(http.StatusOK, map[string]any{"data": out})
	}

	return c.JSON(http.StatusOK, map[string]any{"data": []any{}})
}
