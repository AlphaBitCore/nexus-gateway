package analytics

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// AnalyticsLatencyPhases returns per-phase P50/P95/P99 latency aggregates
// grouped by the requested dimension over the requested time window.
// OpenAPI: docs/users/api/openapi/admin/e50-s6-latency-phases.yaml.
//
// Query params:
//
//	groupBy    provider | model | virtual_key | node | host | device  (required)
//	start, end ISO-8601 timestamps (required)
//	source     all | ai-gateway | compliance-proxy | agent  (optional, default all)
//	percentile comma-separated subset of p50,p95,p99  (optional)
func (h *Handler) AnalyticsLatencyPhases(c echo.Context) error {
	groupBy := strings.TrimSpace(c.QueryParam("groupBy"))
	if groupBy == "" {
		return c.JSON(http.StatusBadRequest, errJSON("groupBy is required", "missing_groupBy", ""))
	}
	column, ok := latencyPhasesGroupColumn(groupBy)
	if !ok {
		return c.JSON(http.StatusBadRequest, errJSON("unsupported groupBy: "+groupBy, "bad_groupBy", ""))
	}

	start, end := parseTimeRange(c)
	if start == nil || end == nil {
		return c.JSON(http.StatusBadRequest, errJSON("start and end are required", "missing_window", ""))
	}

	source := strings.TrimSpace(c.QueryParam("source"))
	if source == "" {
		source = "all"
	}

	rows, err := h.queryLatencyPhases(c.Request().Context(), column, *start, *end, source)
	if err != nil {
		h.logger.Error("analytics latency-phases", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"window": map[string]string{
			"start": start.UTC().Format(time.RFC3339),
			"end":   end.UTC().Format(time.RFC3339),
		},
		"rows": rows,
	})
}

// latencyPhasesGroupColumn maps the public groupBy enum to the SQL column
// used in GROUP BY. Returns ok=false for unknown values; the caller turns
// that into a 400. Keeping the mapping out of the SQL string keeps the
// endpoint safe from injection.
func latencyPhasesGroupColumn(groupBy string) (string, bool) {
	switch groupBy {
	case "provider":
		return "COALESCE(routed_provider_name, provider_name, 'unknown')", true
	case "model":
		return "COALESCE(routed_model_name, model_name, 'unknown')", true
	case "virtual_key":
		// identity.vk is the Virtual Key (identity.apiCredential is the
		// upstream provider's API key, NOT what users mean by "VK").
		return "COALESCE((identity->'vk'->>'name'), 'unknown')", true
	case "node":
		return "COALESCE(thing_name, thing_id, 'unknown')", true
	case "host":
		return "COALESCE(target_host, 'unknown')", true
	case "device":
		return "COALESCE(thing_name, source_ip, 'unknown')", true
	default:
		return "", false
	}
}

// latencyPhasesRow is the JSON shape for one entry in the response `rows`
// array. Matches docs/users/api/openapi/admin/e50-s6-latency-phases.yaml schema
// `LatencyPhaseRow`. Percentile fields are pointers so a NULL P95 (all
// historical rows in the window had NULL upstream_total_ms) reaches the
// client as `null` rather than `0`.
type latencyPhasesRow struct {
	GroupKey           string `json:"groupKey"`
	GroupLabel         string `json:"groupLabel"`
	RequestCount       int64  `json:"requestCount"`
	TotalP50Ms         *int   `json:"totalP50Ms"`
	TotalP95Ms         *int   `json:"totalP95Ms"`
	TotalP99Ms         *int   `json:"totalP99Ms"`
	UsOverheadP50Ms    *int   `json:"usOverheadP50Ms"`
	UsOverheadP95Ms    *int   `json:"usOverheadP95Ms"`
	UsOverheadP99Ms    *int   `json:"usOverheadP99Ms"`
	UpstreamTtfbP50Ms  *int   `json:"upstreamTtfbP50Ms"`
	UpstreamTtfbP95Ms  *int   `json:"upstreamTtfbP95Ms"`
	UpstreamTtfbP99Ms  *int   `json:"upstreamTtfbP99Ms"`
	UpstreamTotalP50Ms *int   `json:"upstreamTotalP50Ms"`
	UpstreamTotalP95Ms *int   `json:"upstreamTotalP95Ms"`
	UpstreamTotalP99Ms *int   `json:"upstreamTotalP99Ms"`
	RequestHooksP50Ms  *int   `json:"requestHooksP50Ms"`
	RequestHooksP95Ms  *int   `json:"requestHooksP95Ms"`
	ResponseHooksP50Ms *int   `json:"responseHooksP50Ms"`
	ResponseHooksP95Ms *int   `json:"responseHooksP95Ms"`
}

// queryLatencyPhases runs the percentile aggregation SQL. The us_overhead
// is computed in SQL as GREATEST(0, latency_ms - upstream_total_ms) so the
// percentile reflects per-row overhead, not the difference of percentiles.
func (h *Handler) queryLatencyPhases(ctx context.Context, groupCol string, start, end time.Time, source string) ([]latencyPhasesRow, error) {
	var sourceFilter string
	args := []any{start, end}
	if source != "" && source != "all" {
		sourceFilter = " AND source = $3"
		args = append(args, source)
	}

	q := fmt.Sprintf(`
		SELECT %[1]s AS group_key,
		       COUNT(*) AS request_count,
		       (PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY latency_ms))::int  AS total_p50,
		       (PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY latency_ms))::int  AS total_p95,
		       (PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY latency_ms))::int  AS total_p99,
		       (PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY GREATEST(0, COALESCE(latency_ms,0) - COALESCE(upstream_total_ms,0))))::int AS us_p50,
		       (PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY GREATEST(0, COALESCE(latency_ms,0) - COALESCE(upstream_total_ms,0))))::int AS us_p95,
		       (PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY GREATEST(0, COALESCE(latency_ms,0) - COALESCE(upstream_total_ms,0))))::int AS us_p99,
		       (PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY upstream_ttfb_ms)  FILTER (WHERE upstream_ttfb_ms  IS NOT NULL))::int AS ttfb_p50,
		       (PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY upstream_ttfb_ms)  FILTER (WHERE upstream_ttfb_ms  IS NOT NULL))::int AS ttfb_p95,
		       (PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY upstream_ttfb_ms)  FILTER (WHERE upstream_ttfb_ms  IS NOT NULL))::int AS ttfb_p99,
		       (PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY upstream_total_ms) FILTER (WHERE upstream_total_ms IS NOT NULL))::int AS upt_p50,
		       (PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY upstream_total_ms) FILTER (WHERE upstream_total_ms IS NOT NULL))::int AS upt_p95,
		       (PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY upstream_total_ms) FILTER (WHERE upstream_total_ms IS NOT NULL))::int AS upt_p99,
		       (PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY request_hooks_ms)  FILTER (WHERE request_hooks_ms  IS NOT NULL))::int AS rh_p50,
		       (PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY request_hooks_ms)  FILTER (WHERE request_hooks_ms  IS NOT NULL))::int AS rh_p95,
		       (PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY response_hooks_ms) FILTER (WHERE response_hooks_ms IS NOT NULL))::int AS sh_p50,
		       (PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY response_hooks_ms) FILTER (WHERE response_hooks_ms IS NOT NULL))::int AS sh_p95
		FROM   traffic_event
		WHERE  timestamp >= $1 AND timestamp < $2%[2]s
		GROUP  BY group_key
		ORDER  BY request_count DESC
		LIMIT  200`, groupCol, sourceFilter)

	rows, err := h.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query latency phases: %w", err)
	}
	defer rows.Close()

	out := make([]latencyPhasesRow, 0, 64)
	for rows.Next() {
		var r latencyPhasesRow
		if err := rows.Scan(
			&r.GroupKey, &r.RequestCount,
			&r.TotalP50Ms, &r.TotalP95Ms, &r.TotalP99Ms,
			&r.UsOverheadP50Ms, &r.UsOverheadP95Ms, &r.UsOverheadP99Ms,
			&r.UpstreamTtfbP50Ms, &r.UpstreamTtfbP95Ms, &r.UpstreamTtfbP99Ms,
			&r.UpstreamTotalP50Ms, &r.UpstreamTotalP95Ms, &r.UpstreamTotalP99Ms,
			&r.RequestHooksP50Ms, &r.RequestHooksP95Ms,
			&r.ResponseHooksP50Ms, &r.ResponseHooksP95Ms,
		); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		r.GroupLabel = r.GroupKey
		out = append(out, r)
	}
	return out, rows.Err()
}
