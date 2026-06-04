// Package handler — opsmetrics.go: HTTP handlers for /api/admin/ops-metrics/*.
// Reads run against the metric_ops_* tables CP shares with Hub. Writes are
// limited to retention config + diag-mode windows (in separate files).
package opsmetrics

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/opsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterOpsMetricsRoutes wires GET /api/admin/ops-metrics/{current,timeseries,fleet}.
// All three are read-only and gated by admin:ReadObservability per spec §10.
func (h *Handler) RegisterOpsMetricsRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/ops-metrics/current", h.OpsMetricsCurrent, iamMW(iam.ResourceObservability.Action(iam.VerbRead)))
	g.GET("/ops-metrics/timeseries", h.OpsMetricsTimeseries, iamMW(iam.ResourceObservability.Action(iam.VerbRead)))
	g.GET("/ops-metrics/fleet", h.OpsMetricsFleet, iamMW(iam.ResourceObservability.Action(iam.VerbRead)))
}

// OpsMetricsCurrent returns the latest sample per (thing_id, metric_name,
// dimension_key) within the last 90s, optionally filtered by nodeType /
// nodeId.
func (h *Handler) OpsMetricsCurrent(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	thingType := strings.TrimSpace(c.QueryParam("nodeType"))
	if thingType != "" && thingType != "service" && thingType != "agent" &&
		thingType != "control-plane" && thingType != "ai-gateway" &&
		thingType != "compliance-proxy" && thingType != "nexus-hub" {
		return c.JSON(http.StatusBadRequest, errJSON("invalid nodeType", "validation_error", "VALIDATION_ERROR"))
	}
	thingID := strings.TrimSpace(c.QueryParam("nodeId"))

	samples, err := h.ops.GetOpsMetricsCurrent(c.Request().Context(), opsstore.OpsCurrentParams{
		ThingType: thingType,
		ThingID:   thingID,
	})
	if err != nil {
		h.logger.Error("ops_metrics_current", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read ops metrics", "server_error", "INTERNAL_ERROR"))
	}
	if samples == nil {
		samples = []opsstore.OpsMetricSample{}
	}
	return c.JSON(http.StatusOK, map[string]any{"data": samples})
}

// OpsMetricsTimeseries returns time-bucketed metric values for one
// (nodeId, metric, dim?) tuple. Granularity defaults to "auto".
func (h *Handler) OpsMetricsTimeseries(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	p, herr := parseOpsTimeseriesParams(c)
	if herr != nil {
		return c.JSON(herr.status, herr.body)
	}

	buckets, err := h.ops.GetOpsMetricsTimeseries(c.Request().Context(), p)
	if err != nil {
		h.logger.Error("ops_metrics_timeseries", "error", err, "metric", p.MetricName, "thing", p.ThingID)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read ops timeseries", "server_error", "INTERNAL_ERROR"))
	}
	if buckets == nil {
		buckets = []opsstore.OpsMetricBucket{}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"data":        buckets,
		"granularity": p.Granularity,
	})
}

// OpsMetricsFleet returns the fleet-aggregate (thing_id IS NULL) bucket series
// for the given metric over [from, to). Used by the agent fleet rollup view.
func (h *Handler) OpsMetricsFleet(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	thingType := strings.TrimSpace(c.QueryParam("nodeType"))
	if thingType == "" {
		return c.JSON(http.StatusBadRequest, errJSON("nodeType is required", "validation_error", "VALIDATION_ERROR"))
	}
	metric := strings.TrimSpace(c.QueryParam("metric"))
	if metric == "" {
		return c.JSON(http.StatusBadRequest, errJSON("metric is required", "validation_error", "VALIDATION_ERROR"))
	}
	from, to, herr := parseFromTo(c)
	if herr != nil {
		return c.JSON(herr.status, herr.body)
	}
	gran := strings.TrimSpace(c.QueryParam("granularity"))
	if gran == "" || gran == "auto" {
		gran = opsstore.SelectGranularity(from, to)
		if gran == "raw" {
			gran = "5m" // raw has no fleet aggregate; smallest fleet tier is 5m
		}
	}
	if gran != "5m" && gran != "1h" && gran != "1d" && gran != "1mo" {
		return c.JSON(http.StatusBadRequest, errJSON("invalid granularity for fleet (need 5m|1h|1d|1mo)", "validation_error", "VALIDATION_ERROR"))
	}

	var dim *string
	if v, ok := c.QueryParams()["dim"]; ok && len(v) > 0 {
		s := v[0]
		dim = &s
	}

	buckets, err := h.ops.GetOpsMetricsFleet(c.Request().Context(), opsstore.OpsFleetParams{
		ThingType:    thingType,
		MetricName:   metric,
		DimensionKey: dim,
		From:         from,
		To:           to,
		Granularity:  gran,
	})
	if err != nil {
		h.logger.Error("ops_metrics_fleet", "error", err, "metric", metric, "nodeType", thingType)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read fleet metrics", "server_error", "INTERNAL_ERROR"))
	}
	if buckets == nil {
		buckets = []opsstore.OpsMetricBucket{}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"data":        buckets,
		"granularity": gran,
	})
}

// Param parsing helpers (also used by diag handlers)

// httpErr packages an HTTP status + JSON body so handlers can short-circuit
// from a helper without juggling two return values per failure case.
type httpErr struct {
	status int
	body   map[string]any
}

func badReq(msg string) *httpErr {
	return &httpErr{
		status: http.StatusBadRequest,
		body:   errJSON(msg, "validation_error", "VALIDATION_ERROR"),
	}
}

// parseFromTo extracts ?from=&to= as RFC3339 timestamps. Both are required.
func parseFromTo(c echo.Context) (time.Time, time.Time, *httpErr) {
	fromStr := strings.TrimSpace(c.QueryParam("from"))
	toStr := strings.TrimSpace(c.QueryParam("to"))
	if fromStr == "" || toStr == "" {
		return time.Time{}, time.Time{}, badReq("from and to are required (RFC3339)")
	}
	from, ok := parseRFC3339Flexible(fromStr)
	if !ok {
		return time.Time{}, time.Time{}, badReq("invalid from (need RFC3339)")
	}
	to, ok2 := parseRFC3339Flexible(toStr)
	if !ok2 {
		return time.Time{}, time.Time{}, badReq("invalid to (need RFC3339)")
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, badReq("from must be < to")
	}
	return from, to, nil
}

// parseOpsTimeseriesParams normalises ?nodeId=&metric=&dim=&from=&to=&granularity=.
// Granularity defaults to auto-pick on the from/to span.
func parseOpsTimeseriesParams(c echo.Context) (opsstore.OpsTimeseriesParams, *httpErr) {
	thingID := strings.TrimSpace(c.QueryParam("nodeId"))
	if thingID == "" {
		return opsstore.OpsTimeseriesParams{}, badReq("nodeId is required")
	}
	metric := strings.TrimSpace(c.QueryParam("metric"))
	if metric == "" {
		return opsstore.OpsTimeseriesParams{}, badReq("metric is required")
	}
	from, to, herr := parseFromTo(c)
	if herr != nil {
		return opsstore.OpsTimeseriesParams{}, herr
	}

	gran := strings.TrimSpace(c.QueryParam("granularity"))
	if gran == "" || gran == "auto" {
		gran = opsstore.SelectGranularity(from, to)
	}
	switch gran {
	case "raw", "5m", "1h", "1d", "1mo":
	default:
		return opsstore.OpsTimeseriesParams{}, badReq("invalid granularity (need auto|raw|5m|1h|1d|1mo)")
	}

	var dim *string
	if v, ok := c.QueryParams()["dim"]; ok && len(v) > 0 {
		s := v[0]
		dim = &s
	}

	return opsstore.OpsTimeseriesParams{
		ThingID:      thingID,
		MetricName:   metric,
		DimensionKey: dim,
		From:         from,
		To:           to,
		Granularity:  gran,
	}, nil
}
