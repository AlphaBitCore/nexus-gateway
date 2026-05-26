package analytics

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

type costSummaryResponse struct {
	// Actual spend after all cache savings (what was truly paid to providers).
	TotalCostUSD float64 `json:"totalCostUsd"`
	// Savings from the gateway response cache (cost avoided entirely;
	// full upstream call did not happen on a hit).
	TotalGatewayCacheSavingsUSD float64 `json:"totalGatewayCacheSavingsUsd"`
	// Net savings from the upstream provider's prompt cache (reduced token
	// price on cached reads, minus the write surcharge).
	TotalProviderPromptCacheNetSavingsUSD float64 `json:"totalProviderPromptCacheNetSavingsUsd"`
	// Combined savings from all cache layers.
	TotalCombinedSavingsUSD float64             `json:"totalCombinedSavingsUsd"`
	// Sum of reasoning_cost_usd across the window. Already counted inside
	// TotalCostUSD — surfaced separately so the Cost dashboard can render a
	// "reasoning cost share" widget without re-querying.
	TotalReasoningCostUSD float64 `json:"totalReasoningCostUsd"`
	// Gateway's own LLM spend (L2 embedding lookups + ai-guard classifier
	// calls) — NOT part of TotalCostUSD (which tracks customer-billable
	// upstream spend) so dashboards can render them as a separate "internal
	// ops" line.
	TotalEmbeddingCostUSD float64 `json:"totalEmbeddingCostUsd"`
	TotalAIGuardCostUSD   float64 `json:"totalAiGuardCostUsd"`
	// ExcludeInternalOpsFromBilledCost mirrors the Hub yaml flag so the
	// UI can render the right "internal-ops counted/excluded" hint per
	// traffic event drawer without making a Hub round-trip.
	ExcludeInternalOpsFromBilledCost bool `json:"excludeInternalOpsFromBilledCost"`
	PeriodDays              int                 `json:"periodDays"`
	Since                   string              `json:"since"`
	ByOrg                   []orgCostEntry      `json:"byOrg"`
	ByProvider              []providerCostEntry `json:"byProvider"`
}

type orgCostEntry struct {
	OrgID   string  `json:"orgId"`
	OrgName string  `json:"orgName,omitempty"`
	CostUSD float64 `json:"costUsd"`
}

type providerCostEntry struct {
	ProviderID string  `json:"providerId"`
	CostUSD    float64 `json:"costUsd"`
}

// costSummaryMetrics is the metric set needed for rollup-based cost summary.
var costSummaryMetrics = []string{
	metrics.MetricEstimatedCostUSD,
	metrics.MetricGatewayCacheSavingsUSD,
	metrics.MetricCacheNetSavingsUSD,
	metrics.MetricEmbeddingCostUSD,
	metrics.MetricAIGuardCostUSD,
}

// costSummaryTotals bundles the rollup-summed cost windows.
type costSummaryTotals struct {
	estimated          float64
	gatewaySavings     float64
	providerPromptNet  float64
	embedding          float64
	aiGuard            float64
}

// rollupCostSummaryTotals reads global cost totals from rollup.
// ok=false when the rollup cascade returned no rows (caller falls back to a direct DB SUM).
func (h *Handler) rollupCostSummaryTotals(ctx context.Context, since, until time.Time) (costSummaryTotals, bool) {
	q := metrics.MetricsQuery{Metrics: costSummaryMetrics, StartTime: since, EndTime: until}
	rows, err := h.metrics.QueryRollupCascade(ctx, q)
	if err != nil || len(rows) == 0 {
		return costSummaryTotals{}, false
	}
	result := metrics.BuildResult(q, rows, metrics.SelectGranularity(since, until))
	s := result.Summary
	return costSummaryTotals{
		estimated:         s[metrics.MetricEstimatedCostUSD],
		gatewaySavings:    s[metrics.MetricGatewayCacheSavingsUSD],
		providerPromptNet: s[metrics.MetricCacheNetSavingsUSD],
		embedding:         s[metrics.MetricEmbeddingCostUSD],
		aiGuard:           s[metrics.MetricAIGuardCostUSD],
	}, true
}

// rollupCostSummaryByDim reads per-dimension cost breakdown from rollup.
// dim is the rollup dimension name (e.g. "organization", "routed_provider").
func (h *Handler) rollupCostSummaryByDim(ctx context.Context, since, until time.Time, dim string) []metrics.MetricsGroup {
	q := metrics.MetricsQuery{
		Metrics:      costSummaryMetrics,
		DimensionKey: dim,
		StartTime:    since,
		EndTime:      until,
	}
	rows, err := h.metrics.QueryRollupCascade(ctx, q)
	if err != nil || len(rows) == 0 {
		return nil
	}
	result := metrics.BuildResult(q, rows, metrics.SelectGranularity(since, until))
	return result.Groups
}

// AnalyticsCostSummary returns a rolling 30-day cost breakdown by org and provider.
func (h *Handler) AnalyticsCostSummary(c echo.Context) error {
	if h.pool == nil {
		return c.JSON(http.StatusInternalServerError, errJSON("database not available", "db_unavailable", ""))
	}

	ctx := c.Request().Context()
	periodDays := 30
	since := time.Now().UTC().AddDate(0, 0, -periodDays)
	until := time.Now().UTC()

	// Aggregate totals — rollup first, direct scan fallback.
	var totalCost, gatewaySavings, providerPromptCacheNetSavings, totalReasoningCost float64
	var totalEmbeddingCost, totalAIGuardCost float64
	if tot, ok := h.rollupCostSummaryTotals(ctx, since, until); ok {
		totalCost = tot.estimated
		gatewaySavings = tot.gatewaySavings
		providerPromptCacheNetSavings = tot.providerPromptNet
		totalEmbeddingCost = tot.embedding
		totalAIGuardCost = tot.aiGuard
		// Reasoning cost not yet in rollup; fall back to a direct DB query
		// for this single sum until the rollup catches up.
		_ = h.pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(reasoning_cost_usd), 0)
			FROM traffic_event
			WHERE timestamp > $1
		`, since).Scan(&totalReasoningCost)
	} else {
		err := h.pool.QueryRow(ctx, `
			SELECT
				COALESCE(SUM(estimated_cost_usd),          0),
				COALESCE(SUM(gateway_cache_savings_usd),   0),
				COALESCE(SUM(cache_net_savings_usd),       0),
				COALESCE(SUM(reasoning_cost_usd),          0),
				COALESCE(SUM(embedding_cost_usd),          0),
				COALESCE(SUM(ai_guard_cost_usd),           0)
			FROM traffic_event
			WHERE timestamp > $1
		`, since).Scan(&totalCost, &gatewaySavings, &providerPromptCacheNetSavings, &totalReasoningCost,
			&totalEmbeddingCost, &totalAIGuardCost)
		if err != nil {
			h.logger.Error("cost summary: total query failed", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("query failed", "server_error", ""))
		}
	}

	// By-org breakdown — rollup first, direct scan fallback.
	var byOrg []orgCostEntry
	if groups := h.rollupCostSummaryByDim(ctx, since, until, "organization"); len(groups) > 0 {
		orgLabels := h.resolveDimensionLabels(ctx, "organization", func() []string {
			ids := make([]string, 0, len(groups))
			for _, g := range groups {
				_, v := metrics.ParseDimensionKey(g.DimensionKey)
				if v != "" {
					ids = append(ids, v)
				}
			}
			return ids
		}())
		for _, g := range groups {
			_, orgID := metrics.ParseDimensionKey(g.DimensionKey)
			if orgID == "" {
				orgID = "unassigned"
			}
			byOrg = append(byOrg, orgCostEntry{
				OrgID:   orgID,
				OrgName: orgLabels[orgID],
				CostUSD: g.Values[metrics.MetricEstimatedCostUSD],
			})
		}
	} else {
		rows, err := h.pool.Query(ctx, `
			SELECT COALESCE(org_id, 'unassigned'), COALESCE(SUM(estimated_cost_usd), 0)
			FROM traffic_event
			WHERE timestamp > $1
			GROUP BY org_id
			ORDER BY 2 DESC
		`, since)
		if err != nil {
			h.logger.Error("cost summary: org breakdown query failed", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("query failed", "server_error", ""))
		}
		defer rows.Close()
		for rows.Next() {
			var entry orgCostEntry
			if err := rows.Scan(&entry.OrgID, &entry.CostUSD); err != nil {
				continue
			}
			byOrg = append(byOrg, entry)
		}
	}
	if byOrg == nil {
		byOrg = []orgCostEntry{}
	}

	// By-provider breakdown — rollup first, direct scan fallback.
	var byProvider []providerCostEntry
	if groups := h.rollupCostSummaryByDim(ctx, since, until, "routed_provider"); len(groups) > 0 {
		for _, g := range groups {
			_, provID := metrics.ParseDimensionKey(g.DimensionKey)
			if provID == "" {
				provID = "unknown"
			}
			byProvider = append(byProvider, providerCostEntry{
				ProviderID: provID,
				CostUSD:    g.Values[metrics.MetricEstimatedCostUSD],
			})
		}
	} else {
		provRows, err := h.pool.Query(ctx, `
			SELECT COALESCE(provider_id, 'unknown'), COALESCE(SUM(estimated_cost_usd), 0)
			FROM traffic_event
			WHERE timestamp > $1
			GROUP BY provider_id
			ORDER BY 2 DESC
		`, since)
		if err != nil {
			h.logger.Error("cost summary: provider breakdown query failed", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("query failed", "server_error", ""))
		}
		defer provRows.Close()
		for provRows.Next() {
			var entry providerCostEntry
			if err := provRows.Scan(&entry.ProviderID, &entry.CostUSD); err != nil {
				continue
			}
			byProvider = append(byProvider, entry)
		}
	}
	if byProvider == nil {
		byProvider = []providerCostEntry{}
	}

	return c.JSON(http.StatusOK, costSummaryResponse{
		TotalCostUSD:                totalCost,
		TotalGatewayCacheSavingsUSD: gatewaySavings,
		TotalProviderPromptCacheNetSavingsUSD: providerPromptCacheNetSavings,
		TotalCombinedSavingsUSD:     gatewaySavings + providerPromptCacheNetSavings,
		TotalReasoningCostUSD:       totalReasoningCost,
		TotalEmbeddingCostUSD:       totalEmbeddingCost,
		TotalAIGuardCostUSD:         totalAIGuardCost,
		ExcludeInternalOpsFromBilledCost: h.excludeInternalOpsFromBilledCost,
		PeriodDays:                  periodDays,
		Since:                       since.Format(time.RFC3339),
		ByOrg:                       byOrg,
		ByProvider:                  byProvider,
	})
}
