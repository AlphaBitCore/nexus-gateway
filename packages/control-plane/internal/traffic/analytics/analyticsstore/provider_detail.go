package analyticsstore

import (
	"context"
	"fmt"
	"time"
)

// ProviderDetailResult holds the full analytics detail for a provider.
type ProviderDetailResult struct {
	Summary      map[string]any   `json:"summary"`
	ByModel      []map[string]any `json:"byModel"`
	ByProject    []map[string]any `json:"byProject"`
	ByVirtualKey []map[string]any `json:"byVirtualKey"`
	Daily        []map[string]any `json:"daily"`
	ByStatus     []map[string]any `json:"byStatus"`
}

// GetProviderAnalyticsDetail returns comprehensive analytics for a single provider.
func (store *Store) GetProviderAnalyticsDetail(ctx context.Context, providerID string, start, end *time.Time) (*ProviderDetailResult, error) {
	timeClause := ""
	args := []any{providerID}
	argIdx := 2
	if start != nil {
		timeClause += fmt.Sprintf(` AND a.timestamp >= $%d`, argIdx)
		args = append(args, *start)
		argIdx++
	}
	if end != nil {
		timeClause += fmt.Sprintf(` AND a.timestamp <= $%d`, argIdx)
		args = append(args, *end)
	}

	// Summary
	var totalCount, errorCount, cacheHitCount int
	var avgLatency, totalCost *float64
	var totalTokens, promptTokens, completionTokens *int64
	var avgUsMs, avgUpstreamTtfbMs, avgUpstreamTotalMs *float64
	summaryQ := fmt.Sprintf(`
		SELECT COUNT(*), COUNT(*) FILTER (WHERE a.status_code >= 400),
			COUNT(*) FILTER (WHERE a.cache_status = 'HIT'),
			AVG(a.latency_ms), SUM(a.total_tokens), SUM(a.prompt_tokens),
			SUM(a.completion_tokens), SUM(a.estimated_cost_usd),
			AVG(GREATEST(0, a.latency_ms - a.upstream_total_ms)) FILTER (WHERE a.upstream_total_ms IS NOT NULL),
			AVG(a.upstream_ttfb_ms)  FILTER (WHERE a.upstream_ttfb_ms  IS NOT NULL),
			AVG(a.upstream_total_ms) FILTER (WHERE a.upstream_total_ms IS NOT NULL)
		FROM traffic_event a
		INNER JOIN "Model" m ON m.id = a.model_id AND m."providerId" = $1
		WHERE a.source = 'ai-gateway' %s
	`, timeClause)
	if err := store.pool.QueryRow(ctx, summaryQ, args...).Scan(
		&totalCount, &errorCount, &cacheHitCount,
		&avgLatency, &totalTokens, &promptTokens, &completionTokens, &totalCost,
		&avgUsMs, &avgUpstreamTtfbMs, &avgUpstreamTotalMs,
	); err != nil {
		return nil, fmt.Errorf("provider analytics summary: %w", err)
	}

	errorRate := 0.0
	if totalCount > 0 {
		errorRate = float64(errorCount) / float64(totalCount)
	}
	cacheHitRate := 0.0
	if totalCount > 0 {
		cacheHitRate = float64(cacheHitCount) / float64(totalCount)
	}

	df := func(p *float64) float64 {
		if p != nil {
			return *p
		}
		return 0
	}
	di := func(p *int64) int64 {
		if p != nil {
			return *p
		}
		return 0
	}

	summary := map[string]any{
		"totalRequests": totalCount, "errorCount": errorCount, "errorRate": errorRate,
		"avgLatencyMs": df(avgLatency), "totalTokens": di(totalTokens),
		"totalPromptTokens": di(promptTokens), "totalCompletionTokens": di(completionTokens),
		"totalEstimatedCostUsd": df(totalCost),
		"cacheHitCount":         cacheHitCount, "cacheHitRate": cacheHitRate,
		"avgUsOverheadMs":    df(avgUsMs),
		"avgUpstreamTtfbMs":  df(avgUpstreamTtfbMs),
		"avgUpstreamTotalMs": df(avgUpstreamTotalMs),
	}

	// By model
	byModelQ := fmt.Sprintf(`
		SELECT COALESCE(NULLIF(m.name, ''), NULLIF(a.model_name, ''), a.model_id::text),
			COUNT(*), AVG(a.latency_ms), SUM(a.total_tokens),
			SUM(a.prompt_tokens), SUM(a.completion_tokens), SUM(a.estimated_cost_usd),
			AVG(GREATEST(0, a.latency_ms - a.upstream_total_ms)) FILTER (WHERE a.upstream_total_ms IS NOT NULL),
			AVG(a.upstream_total_ms) FILTER (WHERE a.upstream_total_ms IS NOT NULL)
		FROM traffic_event a
		INNER JOIN "Model" m ON m.id = a.model_id AND m."providerId" = $1
		WHERE a.source = 'ai-gateway' AND a.model_id IS NOT NULL %s
		GROUP BY 1 ORDER BY COUNT(*) DESC
	`, timeClause)
	byModelRows, err := store.pool.Query(ctx, byModelQ, args...)
	if err != nil {
		return nil, fmt.Errorf("provider analytics by model: %w", err)
	}
	byModel := []map[string]any{}
	for byModelRows.Next() {
		var model string
		var cnt int
		var avg, cost *float64
		var tt, pt, ct *int64
		var avgUs, avgUp *float64
		if byModelRows.Scan(&model, &cnt, &avg, &tt, &pt, &ct, &cost, &avgUs, &avgUp) == nil {
			byModel = append(byModel, map[string]any{
				"model": model, "requestCount": cnt, "avgLatencyMs": df(avg),
				"totalTokens": di(tt), "promptTokens": di(pt),
				"completionTokens": di(ct), "estimatedCostUsd": df(cost),
				"avgUsOverheadMs":    df(avgUs),
				"avgUpstreamTotalMs": df(avgUp),
			})
		}
	}
	byModelRows.Close()

	// By project
	byProjectQ := fmt.Sprintf(`
		SELECT
			p.id,
			p.name,
			p.code,
			COUNT(*),
			AVG(a.latency_ms),
			SUM(a.total_tokens),
			SUM(a.prompt_tokens),
			SUM(a.completion_tokens),
			SUM(a.estimated_cost_usd),
			AVG(GREATEST(0, a.latency_ms - a.upstream_total_ms)) FILTER (WHERE a.upstream_total_ms IS NOT NULL),
			AVG(a.upstream_total_ms) FILTER (WHERE a.upstream_total_ms IS NOT NULL)
		FROM traffic_event a
		INNER JOIN "Model" m ON m.id = a.model_id AND m."providerId" = $1
		INNER JOIN "Project" p ON p.id = (a.identity->'project'->>'id')
		WHERE a.source = 'ai-gateway' AND a.identity->'project'->>'id' IS NOT NULL %s
		GROUP BY p.id, p.name, p.code
		ORDER BY COUNT(*) DESC
	`, timeClause)
	byProjectRows, err := store.pool.Query(ctx, byProjectQ, args...)
	if err != nil {
		return nil, fmt.Errorf("provider analytics by project: %w", err)
	}
	byProject := []map[string]any{}
	for byProjectRows.Next() {
		var projectID string
		var projectName, projectCode *string
		var cnt int
		var avg, cost *float64
		var tt, pt, ct *int64
		var avgUs, avgUp *float64
		if byProjectRows.Scan(&projectID, &projectName, &projectCode, &cnt, &avg, &tt, &pt, &ct, &cost, &avgUs, &avgUp) == nil {
			byProject = append(byProject, map[string]any{
				"projectId": projectID,
				"projectName": func() any {
					if projectName == nil {
						return nil
					}
					return *projectName
				}(),
				"projectCode": func() any {
					if projectCode == nil {
						return nil
					}
					return *projectCode
				}(),
				"requestCount":       cnt,
				"avgLatencyMs":       df(avg),
				"totalTokens":        di(tt),
				"promptTokens":       di(pt),
				"completionTokens":   di(ct),
				"estimatedCostUsd":   df(cost),
				"avgUsOverheadMs":    df(avgUs),
				"avgUpstreamTotalMs": df(avgUp),
			})
		}
	}
	byProjectRows.Close()

	// By virtual key
	byVKQ := fmt.Sprintf(`
		SELECT
			vk.id,
			vk.name,
			vk."keyPrefix",
			COUNT(*),
			AVG(a.latency_ms),
			SUM(a.total_tokens),
			SUM(a.prompt_tokens),
			SUM(a.completion_tokens),
			SUM(a.estimated_cost_usd),
			AVG(GREATEST(0, a.latency_ms - a.upstream_total_ms)) FILTER (WHERE a.upstream_total_ms IS NOT NULL),
			AVG(a.upstream_total_ms) FILTER (WHERE a.upstream_total_ms IS NOT NULL)
		FROM traffic_event a
		INNER JOIN "Model" m ON m.id = a.model_id AND m."providerId" = $1
		INNER JOIN "VirtualKey" vk ON vk.id = (a.identity->'vk'->>'id')
		WHERE a.source = 'ai-gateway' AND a.identity->'vk'->>'id' IS NOT NULL %s
		GROUP BY vk.id, vk.name, vk."keyPrefix"
		ORDER BY COUNT(*) DESC
	`, timeClause)
	byVKRows, err := store.pool.Query(ctx, byVKQ, args...)
	if err != nil {
		return nil, fmt.Errorf("provider analytics by virtual key: %w", err)
	}
	byVirtualKey := []map[string]any{}
	for byVKRows.Next() {
		var vkID string
		var vkName, vkPrefix *string
		var cnt int
		var avg, cost *float64
		var tt, pt, ct *int64
		var avgUs, avgUp *float64
		if byVKRows.Scan(&vkID, &vkName, &vkPrefix, &cnt, &avg, &tt, &pt, &ct, &cost, &avgUs, &avgUp) == nil {
			byVirtualKey = append(byVirtualKey, map[string]any{
				"virtualKeyId": vkID,
				"name": func() any {
					if vkName == nil {
						return nil
					}
					return *vkName
				}(),
				"keyPrefix": func() any {
					if vkPrefix == nil {
						return nil
					}
					return *vkPrefix
				}(),
				"requestCount":       cnt,
				"avgLatencyMs":       df(avg),
				"totalTokens":        di(tt),
				"promptTokens":       di(pt),
				"completionTokens":   di(ct),
				"estimatedCostUsd":   df(cost),
				"avgUsOverheadMs":    df(avgUs),
				"avgUpstreamTotalMs": df(avgUp),
			})
		}
	}
	byVKRows.Close()

	// Daily time series
	dailyQ := fmt.Sprintf(`
		SELECT DATE_TRUNC('day', a.timestamp) AS day, COUNT(*), COUNT(*) FILTER (WHERE a.status_code >= 400),
			SUM(a.total_tokens), SUM(a.estimated_cost_usd)
		FROM traffic_event a
		INNER JOIN "Model" m ON m.id = a.model_id AND m."providerId" = $1
		WHERE a.source = 'ai-gateway' AND a.timestamp >= NOW() - INTERVAL '30 days' %s
		GROUP BY day ORDER BY day ASC
	`, timeClause)
	dailyRows, err := store.pool.Query(ctx, dailyQ, args...)
	if err != nil {
		return nil, fmt.Errorf("provider analytics daily: %w", err)
	}
	daily := []map[string]any{}
	for dailyRows.Next() {
		var day time.Time
		var cnt, errCnt int
		var tokens *int64
		var cost *float64
		if dailyRows.Scan(&day, &cnt, &errCnt, &tokens, &cost) == nil {
			daily = append(daily, map[string]any{
				"date": day.Format("2006-01-02"), "requests": cnt, "errors": errCnt,
				"totalTokens": di(tokens), "estimatedCostUsd": df(cost),
			})
		}
	}
	dailyRows.Close()

	// Status code distribution
	statusQ := fmt.Sprintf(`
		SELECT a.status_code, COUNT(*)
		FROM traffic_event a
		INNER JOIN "Model" m ON m.id = a.model_id AND m."providerId" = $1
		WHERE a.source = 'ai-gateway' AND a.status_code IS NOT NULL %s
		GROUP BY a.status_code ORDER BY a.status_code ASC
	`, timeClause)
	statusRows, err := store.pool.Query(ctx, statusQ, args...)
	if err != nil {
		return nil, fmt.Errorf("provider analytics status: %w", err)
	}
	byStatus := []map[string]any{}
	for statusRows.Next() {
		var code, cnt int
		if statusRows.Scan(&code, &cnt) == nil {
			byStatus = append(byStatus, map[string]any{"statusCode": code, "count": cnt})
		}
	}
	statusRows.Close()

	return &ProviderDetailResult{
		Summary:      summary,
		ByModel:      byModel,
		ByProject:    byProject,
		ByVirtualKey: byVirtualKey,
		Daily:        daily,
		ByStatus:     byStatus,
	}, nil
}
