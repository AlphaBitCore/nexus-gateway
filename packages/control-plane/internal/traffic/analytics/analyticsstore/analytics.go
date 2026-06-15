package analyticsstore

import (
	"context"
	"fmt"
	"time"
)

// AnalyticsSummary holds aggregate KPIs.
type AnalyticsSummary struct {
	TotalRequests         int     `json:"totalRequests"`
	ErrorCount            int     `json:"errorCount"`
	ErrorRate             float64 `json:"errorRate"`
	AvgLatencyMs          float64 `json:"avgLatencyMs"`
	TotalTokens           int64   `json:"totalTokens"`
	TotalPromptTokens     int64   `json:"totalPromptTokens"`
	TotalCompletionTokens int64   `json:"totalCompletionTokens"`
	TotalEstimatedCostUsd float64 `json:"totalEstimatedCostUsd"`
	P95LatencyMs          float64 `json:"p95LatencyMs"`
	CacheHitRate          float64 `json:"cacheHitRate"`
	// Latency phase aggregates. Nullable so a NULL P95 on a historical window
	// without phase data reaches the UI as `null` rather than `0`. Computed
	// by the AnalyticsSummary query via PERCENTILE_CONT.
	UsOverheadP95Ms    *int `json:"usOverheadP95Ms,omitempty"`
	UpstreamTtfbP95Ms  *int `json:"upstreamTtfbP95Ms,omitempty"`
	UpstreamTotalP95Ms *int `json:"upstreamTotalP95Ms,omitempty"`
}

// GroupByResult holds a generic group-by aggregation row.
type GroupByResult struct {
	Group                  string  `json:"group"`
	GroupLabel             string  `json:"groupLabel,omitempty"`
	GroupExtra             string  `json:"groupExtra,omitempty"`
	RequestCount           int     `json:"requestCount"`
	AvgLatencyMs           float64 `json:"avgLatencyMs,omitempty"`
	TotalTokens            int64   `json:"totalTokens,omitempty"`
	TotalPromptTokens      int64   `json:"totalPromptTokens,omitempty"`
	TotalCompletionTokens  int64   `json:"totalCompletionTokens,omitempty"`
	TotalCostUsd           float64 `json:"totalCostUsd,omitempty"`
	TotalEstimatedCostUsd  float64 `json:"totalEstimatedCostUsd,omitempty"`
	CacheHitCount          int     `json:"cacheHitCount,omitempty"`
	GatewayCacheSavingsUsd float64 `json:"gatewayCacheSavingsUsd,omitempty"`
	CacheNetSavingsUsd     float64 `json:"cacheNetSavingsUsd,omitempty"`
}

// analyticsGroupSQL holds the pre-built SQL for each groupBy column.
// Every query is fully hardcoded — no fmt.Sprintf for column names at runtime.
type analyticsGroupSpec struct {
	selectExpr string
	nullFilter string
	// allSources disables the default "source = 'ai-gateway'" filter when true.
	allSources bool
}

var analyticsGroupSQL = map[string]analyticsGroupSpec{
	"provider":       {selectExpr: `COALESCE(routed_provider_name, provider_name)`, nullFilter: ``},
	"modelUsed":      {selectExpr: `COALESCE(routed_model_name, model_name)`, nullFilter: ` AND COALESCE(routed_model_name, model_name) IS NOT NULL`},
	"entityId":       {selectExpr: `entity_id`, nullFilter: ` AND entity_id IS NOT NULL`, allSources: true},
	"orgId":          {selectExpr: `org_id`, nullFilter: ` AND org_id IS NOT NULL`},
	"entityType":     {selectExpr: `entity_type`, nullFilter: ` AND entity_type IS NOT NULL`, allSources: true},
	"routedProvider": {selectExpr: `routed_provider_name`, nullFilter: ` AND routed_provider_name IS NOT NULL`},
	"routingRuleId":  {selectExpr: `routing_rule_id`, nullFilter: ` AND routing_rule_id IS NOT NULL`},
	"targetHost":     {selectExpr: `target_host`, nullFilter: ` AND target_host IS NOT NULL`, allSources: true},
}

// AnalyticsGroupByParams holds optional filter/pagination params for group-by queries.
type AnalyticsGroupByParams struct {
	Search string // ILIKE filter on the group column
	Limit  int    // 0 = no limit
	Offset int
}

// GetAnalyticsGroupBy returns aggregation grouped by the given dimension.
// groupKey must be one of: provider, modelUsed, entityId, orgId, entityType, routedProvider, routingRuleId, targetHost.
func (store *Store) GetAnalyticsGroupBy(ctx context.Context, groupKey string, start, end *time.Time, sumFields string, params *AnalyticsGroupByParams) ([]GroupByResult, int, error) {
	spec, ok := analyticsGroupSQL[groupKey]
	if !ok {
		return nil, 0, fmt.Errorf("invalid group key: %q", groupKey)
	}

	timeClause, args, argIdx := buildTimeClause(start, end)

	// Build the full query string — column expressions come from the hardcoded map, not from caller input.
	var sumExpr string
	switch sumFields {
	case "tokens":
		sumExpr = `, SUM(prompt_tokens) AS pt, SUM(completion_tokens) AS ct, SUM(total_tokens) AS tt`
	case "cost":
		sumExpr = `, SUM(estimated_cost_usd) AS cost, SUM(total_tokens) AS tt,` +
			` COALESCE(SUM(gateway_cache_savings_usd), 0) AS gcs, COALESCE(SUM(cache_net_savings_usd), 0) AS cns`
	}

	sourceFilter := ` WHERE source = 'ai-gateway'`
	if spec.allSources {
		sourceFilter = ` WHERE 1=1`
	}

	searchFilter := ""
	if params != nil && params.Search != "" {
		searchFilter = fmt.Sprintf(` AND %s::text ILIKE $%d`, spec.selectExpr, argIdx)
		args = append(args, "%"+escapeILIKE(params.Search)+"%")
	}

	baseWhere := sourceFilter + timeClause + spec.nullFilter + searchFilter

	// Count total groups
	countQ := `SELECT COUNT(DISTINCT ` + spec.selectExpr + `) FROM traffic_event` + baseWhere
	var total int
	if err := store.pool.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("analytics group by count: %w", err)
	}

	q := `SELECT ` + spec.selectExpr + ` AS grp, COUNT(*) AS cnt` + sumExpr +
		` FROM traffic_event` + baseWhere + ` GROUP BY grp ORDER BY cnt DESC`

	if params != nil && params.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d OFFSET %d`, params.Limit, params.Offset)
	}

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("analytics group by: %w", err)
	}
	defer rows.Close()

	results := []GroupByResult{}
	for rows.Next() {
		var r GroupByResult
		switch sumFields {
		case "tokens":
			var pt, ct, tt *int64
			if err := rows.Scan(&r.Group, &r.RequestCount, &pt, &ct, &tt); err != nil {
				return nil, 0, err
			}
			if pt != nil {
				r.TotalPromptTokens = *pt
			}
			if ct != nil {
				r.TotalCompletionTokens = *ct
			}
			if tt != nil {
				r.TotalTokens = *tt
			}
		case "cost":
			var cost *float64
			var tt *int64
			if err := rows.Scan(&r.Group, &r.RequestCount, &cost, &tt, &r.GatewayCacheSavingsUsd, &r.CacheNetSavingsUsd); err != nil {
				return nil, 0, err
			}
			if cost != nil {
				r.TotalCostUsd = *cost
			}
			if tt != nil {
				r.TotalTokens = *tt
			}
		default:
			if err := rows.Scan(&r.Group, &r.RequestCount); err != nil {
				return nil, 0, err
			}
		}
		results = append(results, r)
	}
	return results, total, rows.Err()
}

// RoutingDistribution holds routing decision data.
type RoutingDistribution struct {
	Provider     *string `json:"provider"`
	Model        *string `json:"model"`
	RequestCount int     `json:"requestCount"`
}

// QualitySummary holds quality analytics.
type QualitySummary struct {
	TotalResponses int     `json:"totalResponses"`
	AnomalyCount   int     `json:"anomalyCount"`
	AnomalyRate    float64 `json:"anomalyRate"`
}

func buildTimeClause(start, end *time.Time) (string, []any, int) {
	clause := ""
	args := []any{}
	argIdx := 1

	if start != nil {
		clause += fmt.Sprintf(` AND timestamp >= $%d`, argIdx)
		args = append(args, *start)
		argIdx++
	}
	if end != nil {
		clause += fmt.Sprintf(` AND timestamp <= $%d`, argIdx)
		args = append(args, *end)
		argIdx++
	}
	return clause, args, argIdx
}
