package analytics

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/analytics/analyticsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// RegisterMetricsRoutes registers metric rollup routes.
func (h *Handler) RegisterMetricsRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/metrics/aggregates", h.MetricsAggregates, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
}

// RegisterAnalyticsRoutes registers analytics query routes.
func (h *Handler) RegisterAnalyticsRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/analytics/summary", h.AnalyticsSummary, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/by-provider", h.AnalyticsByProvider, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/by-user", h.AnalyticsByUser, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/usage", h.AnalyticsUsage, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/cost", h.AnalyticsCost, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/cost-report", h.AnalyticsCostReport, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/cost-summary", h.AnalyticsCostSummary, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/cache-roi", h.AnalyticsCacheROI, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/routing", h.AnalyticsRouting, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/routing/fallbacks", h.AnalyticsRoutingFallbacks, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/quality", h.AnalyticsQuality, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/provider/:providerId", h.AnalyticsProviderDetail, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	g.GET("/analytics/sparkline", h.AnalyticsSparkline, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
	// Latency phase breakdown.
	g.GET("/analytics/latency-phases", h.AnalyticsLatencyPhases, iamMW(iam.ResourceAnalytics.Action(iam.VerbRead)))
}

func (h *Handler) AnalyticsProviderDetail(c echo.Context) error {
	providerID := c.Param("providerId")
	start, end := parseTimeRange(c)
	if start == nil {
		t := time.Now().UTC().AddDate(0, 0, -30)
		start = &t
	}

	result, err := h.analytics.GetProviderAnalyticsDetail(c.Request().Context(), providerID, start, end)
	if err != nil {
		h.logger.Error("provider analytics detail", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"summary": result.Summary, "byModel": result.ByModel,
		"byProject": result.ByProject, "byVirtualKey": result.ByVirtualKey,
		"daily": result.Daily, "byStatus": result.ByStatus,
	})
}

// maxTopN caps the "limit" query param on top-N analytics endpoints. Values
// above this cap silently clamp to maxTopN; large top-N queries are not a
// product use case and unbounded values enable DoS via wide-range scans.
const maxTopN = 100

// parseTopN reads the "limit" query param, defaulting to fallback and
// clamping to [1, maxTopN].
func parseTopN(c echo.Context, fallback int) int {
	if s := c.QueryParam("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			if n > maxTopN {
				n = maxTopN
			}
			return n
		}
	}
	return fallback
}

func parseTimeRange(c echo.Context) (start, end *time.Time) {
	startStr := c.QueryParam("startTime")
	if startStr == "" {
		startStr = c.QueryParam("start")
	}
	if startStr != "" {
		if t, ok := parseRFC3339Flexible(startStr); ok {
			start = &t
		}
	}
	endStr := c.QueryParam("endTime")
	if endStr == "" {
		endStr = c.QueryParam("end")
	}
	if endStr != "" {
		if t, ok := parseRFC3339Flexible(endStr); ok {
			end = &t
		}
	}
	return
}

// parseTZParam reads a `?tz=` IANA name from the request and returns
// the matching *time.Location. Empty / unknown values fall back to UTC
// so a malformed query doesn't 500. The returned name is the resolved
// IANA string ("UTC" on fallback) so handlers can echo it in the
// response for client transparency.
func parseTZParam(c echo.Context) (loc *time.Location, name string) {
	raw := c.QueryParam("tz")
	if raw == "" {
		return time.UTC, "UTC"
	}
	l, err := time.LoadLocation(raw)
	if err != nil {
		return time.UTC, "UTC"
	}
	return l, raw
}

// tzLoc is a single-return convenience wrapper over parseTZParam for
// inline use at call sites that don't need the resolved name string.
func tzLoc(c echo.Context) *time.Location {
	l, _ := parseTZParam(c)
	return l
}

// MetricsAggregates returns metric rollup data for the given time range.
func (h *Handler) MetricsAggregates(c echo.Context) error {
	if h.tryRollupMetricsAggregates(c) {
		return nil
	}
	// Rollup returned no data — return empty result.
	return c.JSON(http.StatusOK, map[string]any{"data": []any{}})
}

func (h *Handler) AnalyticsSummary(c echo.Context) error {
	if h.tryRollupSummary(c) {
		return nil
	}
	// Rollup returned no data — return zero-value summary.
	return c.JSON(http.StatusOK, &analyticsstore.AnalyticsSummary{})
}

func (h *Handler) AnalyticsByProvider(c echo.Context) error {
	if h.tryRollupByProvider(c) {
		return nil
	}
	// Rollup returned no data — return empty result.
	return c.JSON(http.StatusOK, map[string]any{"data": []any{}})
}

// usageGroupByMap maps user-facing groupBy query param values to logical keys
// used by store.GetAnalyticsGroupBy (which resolves them via analyticsGroupSQL).
var usageGroupByMap = map[string]string{
	"provider":     "provider",
	"model":        "modelUsed",
	"project":      "projectId",
	"organization": "organizationId",
	"user":         "userId",
	"virtual_key":  "virtualKeyId",
	"host":         "targetHost",
	"device":       "deviceId",
}

// maxGroupByLimit and maxGroupByOffset cap the analytics groupBy query
// pagination. Values above the caps silently clamp; unbounded pagination
// allows the caller to force expensive scans + GROUP BY passes on
// traffic_event.
const (
	maxGroupByLimit  = 1000
	maxGroupByOffset = 1_000_000
)

func parseGroupByParams(c echo.Context) *analyticsstore.AnalyticsGroupByParams {
	p := &analyticsstore.AnalyticsGroupByParams{
		Search: c.QueryParam("q"),
	}
	if v := c.QueryParam("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > maxGroupByLimit {
				n = maxGroupByLimit
			}
			p.Limit = n
		}
	}
	if v := c.QueryParam("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			if n > maxGroupByOffset {
				n = maxGroupByOffset
			}
			p.Offset = n
		}
	}
	return p
}

func (h *Handler) AnalyticsByUser(c echo.Context) error {
	start, end := parseTimeRange(c)
	data, total, err := h.analytics.GetAnalyticsGroupBy(c.Request().Context(), "userId", start, end, "cost", parseGroupByParams(c))
	if err != nil {
		h.logger.Error("analytics by user", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": data, "total": total})
}

func (h *Handler) AnalyticsUsage(c echo.Context) error {
	groupBy := c.QueryParam("groupBy")
	col := usageGroupByMap[groupBy]
	if col == "" {
		col = "provider"
	}

	rollupData, ok := h.tryRollupGroupBy(c, col, "tokens")
	if !ok {
		return c.JSON(http.StatusOK, map[string]any{"data": []any{}, "total": 0})
	}
	h.resolveGroupLabels(c.Request().Context(), col, rollupData)
	topN := parseTopN(c, 10)
	return c.JSON(http.StatusOK, map[string]any{"data": applyTopN(rollupData, topN), "total": len(rollupData)})
}

func (h *Handler) AnalyticsCost(c echo.Context) error {
	groupBy := c.QueryParam("groupBy")
	col := usageGroupByMap[groupBy]
	if col == "" {
		col = "provider"
	}

	rollupData, ok := h.tryRollupGroupBy(c, col, "cost")
	if !ok {
		return c.JSON(http.StatusOK, map[string]any{"data": []any{}, "total": 0})
	}
	h.resolveGroupLabels(c.Request().Context(), col, rollupData)
	topN := parseTopN(c, 10)
	return c.JSON(http.StatusOK, map[string]any{"data": applyTopN(rollupData, topN), "total": len(rollupData)})
}

func (h *Handler) AnalyticsCostReport(c echo.Context) error {
	rollupData := h.tryRollupCostReport(c)
	if rollupData == nil {
		return c.JSON(http.StatusOK, map[string]any{"data": []any{}, "total": 0})
	}
	h.resolveGroupLabels(c.Request().Context(), "organizationId", rollupData)
	return c.JSON(http.StatusOK, map[string]any{"data": rollupData, "total": len(rollupData)})
}

// resolveGroupLabels populates GroupLabel (and GroupExtra where applicable)
// for group-by results that return IDs.
func (h *Handler) resolveGroupLabels(ctx context.Context, groupKey string, data []analyticsstore.GroupByResult) {
	if len(data) == 0 {
		return
	}

	ids := make([]string, len(data))
	for i, d := range data {
		ids[i] = d.Group
	}

	var query string
	var hasExtra bool
	switch groupKey {
	case "virtualKeyId":
		// Rollup stores the vk ID (UUID) as the dimension value. JOIN to look
		// up the human-readable name and enrich GroupExtra with the owner's
		// display name.
		query = `SELECT vk.id, vk.name, COALESCE(nu."displayName", '')
			FROM "VirtualKey" vk
			LEFT JOIN "NexusUser" nu ON nu.id = vk."ownerId"
			WHERE vk.id = ANY($1)`
		hasExtra = true
	case "projectId":
		query = `SELECT id, name FROM "Project" WHERE id = ANY($1)`
	case "organizationId":
		query = `SELECT id, name FROM "Organization" WHERE id = ANY($1)`
	case "userId":
		query = `SELECT id, "displayName" FROM "NexusUser" WHERE id = ANY($1)`
	case "deviceId":
		// Use traffic_event to get the most recent user for each device
		query = `SELECT id, COALESCE(hostname, '') FROM thing WHERE id = ANY($1)`
	case "routingRuleId":
		query = `SELECT id, name FROM "RoutingRule" WHERE id = ANY($1)`
	default:
		return
	}

	rows, err := h.pool.Query(ctx, query, ids)
	if err != nil {
		h.logger.Warn("resolve group labels", "error", err, "groupKey", groupKey)
		return
	}
	defer rows.Close()

	labelMap := map[string]string{}
	extraMap := map[string]string{}
	for rows.Next() {
		if hasExtra {
			var id, name, extra string
			if err := rows.Scan(&id, &name, &extra); err == nil {
				labelMap[id] = name
				extraMap[id] = extra
			}
		} else {
			var id, name string
			if err := rows.Scan(&id, &name); err == nil {
				labelMap[id] = name
			}
		}
	}

	for i := range data {
		if label, ok := labelMap[data[i].Group]; ok {
			data[i].GroupLabel = label
		}
		if extra, ok := extraMap[data[i].Group]; ok && extra != "" {
			data[i].GroupExtra = extra
		}
	}

	// For devices, resolve users from traffic_event (a device may have multiple users)
	if groupKey == "deviceId" {
		h.resolveDeviceUsers(ctx, data)
	}
}

// resolveDeviceUsers populates GroupExtra with comma-separated user display
// names for device group-by results. Looks up users assigned to each device
// via DeviceAssignment (active + historical) rather than scraping
// traffic_event.entity_name — agent traffic stamps thing_id, not entity_id,
// so the old entity_name aggregation returned nothing.
func (h *Handler) resolveDeviceUsers(ctx context.Context, data []analyticsstore.GroupByResult) {
	ids := make([]string, len(data))
	for i, d := range data {
		ids[i] = d.Group
	}
	rows, err := h.pool.Query(ctx, `
		SELECT da."deviceId",
		       string_agg(DISTINCT COALESCE(NULLIF(u."displayName", ''), u.email), ', ')
		FROM "DeviceAssignment" da
		LEFT JOIN "NexusUser" u ON u.id = da."userId"
		WHERE da."deviceId" = ANY($1)
		GROUP BY da."deviceId"
	`, ids)
	if err != nil {
		h.logger.Warn("resolve device users", "error", err)
		return
	}
	defer rows.Close()

	userMap := map[string]string{}
	for rows.Next() {
		var id, users string
		if err := rows.Scan(&id, &users); err == nil {
			userMap[id] = users
		}
	}
	for i := range data {
		if users, ok := userMap[data[i].Group]; ok {
			data[i].GroupExtra = users
		}
	}
}

func (h *Handler) AnalyticsRouting(c echo.Context) error {
	rollupData := h.tryRollupRouting(c)
	if rollupData == nil {
		return c.JSON(http.StatusOK, map[string]any{"data": []any{}})
	}
	return c.JSON(http.StatusOK, map[string]any{"data": rollupData})
}

func (h *Handler) AnalyticsRoutingFallbacks(c echo.Context) error {
	rollupData := h.tryRollupRoutingFallbacks(c)
	if rollupData == nil {
		return c.JSON(http.StatusOK, map[string]any{"data": []any{}})
	}
	h.resolveGroupLabels(c.Request().Context(), "routingRuleId", rollupData)
	return c.JSON(http.StatusOK, map[string]any{"data": rollupData})
}

func (h *Handler) AnalyticsSparkline(c echo.Context) error {
	start, end := parseTimeRange(c)
	if start == nil || end == nil {
		now := time.Now().UTC()
		s := now.Add(-7 * 24 * time.Hour)
		start = &s
		end = &now
	}

	q := metrics.MetricsQuery{
		Metrics: []string{
			metrics.MetricRequestCount,
			metrics.MetricStatus4xxCount, metrics.MetricStatus5xxCount,
			metrics.MetricLatencySum, metrics.MetricLatencyCount,
			metrics.MetricEstimatedCostUSD, metrics.MetricTotalTokens,
			metrics.MetricCacheHitCount,
			metrics.MetricCacheSavedCostUSD, metrics.MetricCacheNetSavingsUSD,
			// Phase aggregates — Hub rollup_5m emits these from
			// traffic_event so the Dashboard sparkline can render phase trends.
			metrics.MetricLatencyUsSum, metrics.MetricLatencyUsCount,
			metrics.MetricLatencyUpstreamTtfbSum, metrics.MetricLatencyUpstreamTtfbCount,
			metrics.MetricLatencyUpstreamSum, metrics.MetricLatencyUpstreamCount,
			metrics.MetricLatencyHooksSum, metrics.MetricLatencyHooksCount,
		},
		SubDimension: sourceSubDimension(c),
		StartTime:    *start,
		EndTime:      *end,
		TimeSeries:   true,
	}
	result, _ := h.queryMetricsOrFallback(c.Request().Context(), q)
	if result != nil {
		return c.JSON(http.StatusOK, result)
	}

	return c.JSON(http.StatusOK, &metrics.MetricsResult{
		Granularity: "1d",
		Source:      "rollup",
		Series:      []metrics.MetricsBucket{},
	})
}

func (h *Handler) AnalyticsQuality(c echo.Context) error {
	if h.tryRollupQuality(c) {
		return nil
	}
	// Rollup returned no data — return zero-value quality summary.
	return c.JSON(http.StatusOK, map[string]any{
		"totalResponses": 0,
		"anomalyCount":   0,
		"anomalyRate":    0,
	})
}
