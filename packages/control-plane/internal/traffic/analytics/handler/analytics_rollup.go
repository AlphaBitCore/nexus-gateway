package analytics

import (
	"context"
	"sort"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/analytics/analyticsstore"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

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
