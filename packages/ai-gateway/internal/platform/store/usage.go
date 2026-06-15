package store

import (
	"context"
	"fmt"
	"time"
)

// VKUsage holds aggregated usage for a virtual key (current period).
type VKUsage struct {
	VirtualKeyID     string  `json:"virtualKeyId"`
	TotalRequests    int64   `json:"totalRequests"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	EstimatedCostUsd float64 `json:"estimatedCostUsd"`
}

// GetUsageForVK returns aggregated usage from audit logs for a virtual key.
// Uses ::float8 cast so pgx can scan the numeric aggregate directly into float64.
func (db *DB) GetUsageForVK(ctx context.Context, virtualKeyID string) (*VKUsage, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT
			COALESCE(COUNT(*), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(estimated_cost_usd), 0)::float8
		FROM traffic_event
		WHERE source = 'ai-gateway' AND identity->'vk'->>'id' = $1
	`, virtualKeyID)

	usage := &VKUsage{VirtualKeyID: virtualKeyID}
	err := row.Scan(
		&usage.TotalRequests,
		&usage.PromptTokens,
		&usage.CompletionTokens,
		&usage.TotalTokens,
		&usage.EstimatedCostUsd,
	)
	if err != nil {
		return nil, fmt.Errorf("store: get usage for vk: %w", err)
	}
	return usage, nil
}

// CostSumSince returns the sum of estimated_cost_usd for ai-gateway traffic
// events within the given window ending at now. Used by the proxy.cost_spike
// alert evaluator. No unit test is provided here because the method requires a
// live PostgreSQL connection; it is exercised at the evaluator / e2e level.
func (db *DB) CostSumSince(ctx context.Context, window time.Duration) (float64, error) {
	since := time.Now().Add(-window)
	var cost float64
	err := db.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(estimated_cost_usd), 0)::float8
		FROM traffic_event
		WHERE source = 'ai-gateway'
		  AND timestamp >= $1
	`, since).Scan(&cost)
	if err != nil {
		return 0, fmt.Errorf("store: cost sum since: %w", err)
	}
	return cost, nil
}

// DailyModelUsage holds one row of the daily usage aggregation.
type DailyModelUsage struct {
	Day              time.Time `json:"day"`
	ModelName        string    `json:"modelName"`
	ProviderName     string    `json:"providerName"`
	Requests         int64     `json:"requests"`
	PromptTokens     int64     `json:"promptTokens"`
	CompletionTokens int64     `json:"completionTokens"`
	TotalTokens      int64     `json:"totalTokens"`
	CostUsd          float64   `json:"costUsd"`
}

// GetDailyUsageForVK returns daily usage aggregated by model and provider
// for a virtual key within the specified time range.
// start is inclusive, end is exclusive (end = endDate + 1 day).
func (db *DB) GetDailyUsageForVK(ctx context.Context, virtualKeyID string, start, end time.Time) ([]DailyModelUsage, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT
			date_trunc('day', timestamp)::date AS day,
			COALESCE(routed_model_name, model_name, '') AS model_name,
			COALESCE(routed_provider_name, provider_name, '') AS provider_name,
			COUNT(*) AS requests,
			COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
			COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
			COALESCE(SUM(total_tokens), 0) AS total_tokens,
			COALESCE(SUM(estimated_cost_usd), 0)::float8 AS cost_usd
		FROM traffic_event
		WHERE source = 'ai-gateway'
		  AND identity->'vk'->>'id' = $1
		  AND timestamp >= $2
		  AND timestamp < $3
		GROUP BY day, 2, 3
		ORDER BY day DESC, cost_usd DESC
	`, virtualKeyID, start, end)
	if err != nil {
		return nil, fmt.Errorf("store: get daily usage for vk: %w", err)
	}
	defer rows.Close()

	var result []DailyModelUsage
	for rows.Next() {
		var r DailyModelUsage
		if err := rows.Scan(
			&r.Day, &r.ModelName, &r.ProviderName,
			&r.Requests, &r.PromptTokens, &r.CompletionTokens,
			&r.TotalTokens, &r.CostUsd,
		); err != nil {
			return nil, fmt.Errorf("store: scan daily usage row: %w", err)
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate daily usage: %w", err)
	}
	return result, nil
}
