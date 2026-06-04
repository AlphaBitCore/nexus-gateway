package quota

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterQuotaAnalyticsRoutes registers quota analytics read-only endpoints.
func (h *Handler) RegisterQuotaAnalyticsRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/quota-analytics/overview", h.QuotaAnalyticsOverview, iamMW(iam.ResourceQuotaAnalytics.Action(iam.VerbRead)))
	g.GET("/quota-analytics/trend", h.QuotaAnalyticsTrend, iamMW(iam.ResourceQuotaAnalytics.Action(iam.VerbRead)))
	g.GET("/quota-analytics/top", h.QuotaAnalyticsTop, iamMW(iam.ResourceQuotaAnalytics.Action(iam.VerbRead)))
}

// parsePeriodKey converts a period key string (e.g. "2026-04" for monthly,
// "2026-04-15" for daily) into a [start, end) time range.
// Monthly keys ("YYYY-MM") return the full calendar month.
// Daily keys ("YYYY-MM-DD") return the full calendar day.
// Returns an error for unrecognised formats.
func parsePeriodKey(key string) (start, end time.Time, err error) {
	switch len(key) {
	case 7: // YYYY-MM
		t, parseErr := time.Parse("2006-01", key)
		if parseErr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid period key %q: %w", key, parseErr)
		}
		start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
		return start, end, nil
	case 10: // YYYY-MM-DD
		t, parseErr := time.Parse("2006-01-02", key)
		if parseErr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid period key %q: %w", key, parseErr)
		}
		start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 1)
		return start, end, nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unsupported period key format %q (expected YYYY-MM or YYYY-MM-DD)", key)
	}
}

// currentMonthPeriodKey returns the current month as a "YYYY-MM" string.
func currentMonthPeriodKey() string {
	return time.Now().UTC().Format("2006-01")
}

// scopeToDimension maps a quota scope name to a rollup DimensionKey prefix.
// The rollup DimensionKey format is "dimension=value".
func scopeToDimension(scope string) (string, bool) {
	m := map[string]string{
		"user":         "user",
		"vk":           "virtual_key",
		"virtual_key":  "virtual_key",
		"project":      "organization", // project maps to org dimension for now
		"organization": "organization",
	}
	dim, ok := m[scope]
	return dim, ok
}

// QuotaAnalyticsOverviewItem is one row in the overview response.
type QuotaAnalyticsOverviewItem struct {
	EntityID       string   `json:"entityId"`
	EntityName     string   `json:"entityName"`
	EntityType     string   `json:"entityType"`
	CurrentCostUsd float64  `json:"currentCostUsd"`
	CostLimitUsd   *float64 `json:"costLimitUsd"`
	UsagePercent   float64  `json:"usagePercent"`
	AlertLevel     string   `json:"alertLevel"`
}

// alertLevelFromPercent returns a human-readable alert level string.
func alertLevelFromPercent(pct float64) string {
	switch {
	case pct >= 90:
		return "critical"
	case pct >= 70:
		return "warning"
	default:
		return "normal"
	}
}

// QuotaAnalyticsOverview returns cost usage per entity for a given scope and period.
//
// Query params:
//
//	scope     — user | vk | project | organization
//	periodKey — YYYY-MM (default: current month)
func (h *Handler) QuotaAnalyticsOverview(c echo.Context) error {
	scope := c.QueryParam("scope")
	if scope == "" {
		scope = "user"
	}
	periodKey := c.QueryParam("periodKey")
	if periodKey == "" {
		periodKey = currentMonthPeriodKey()
	}

	dimension, ok := scopeToDimension(scope)
	if !ok {
		return c.JSON(http.StatusBadRequest, errJSON(
			fmt.Sprintf("unsupported scope %q", scope), "validation_error", ""))
	}

	start, end, err := parsePeriodKey(periodKey)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", ""))
	}

	rows, err := h.metrics.QueryRollup(c.Request().Context(), metrics.MetricsQuery{
		Metrics:      []string{metrics.MetricEstimatedCostUSD},
		DimensionKey: dimension,
		StartTime:    start,
		EndTime:      end,
	})
	if err != nil {
		h.logger.Error("quota analytics overview: query rollup", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	// Aggregate cost per dimension value.
	totals := map[string]float64{}
	prefix := dimension + "="
	for _, r := range rows {
		if !strings.HasPrefix(r.DimensionKey, prefix) {
			continue
		}
		targetID := strings.TrimPrefix(r.DimensionKey, prefix)
		totals[targetID] += r.Value
	}

	items := make([]QuotaAnalyticsOverviewItem, 0, len(totals))
	for targetID, cost := range totals {
		item := QuotaAnalyticsOverviewItem{
			EntityID:       targetID,
			EntityName:     h.resolveEntityName(c.Request().Context(), scope, targetID),
			EntityType:     scope,
			CurrentCostUsd: cost,
		}
		// Look up override for this target to get explicit limit.
		override, oErr := h.quota.GetQuotaOverrideByTarget(c.Request().Context(), scope, targetID)
		if oErr == nil && override != nil && override.CostLimitUsd != nil {
			item.CostLimitUsd = override.CostLimitUsd
			if *override.CostLimitUsd > 0 {
				item.UsagePercent = (cost / *override.CostLimitUsd) * 100
			}
		}
		item.AlertLevel = alertLevelFromPercent(item.UsagePercent)
		items = append(items, item)
	}

	// Sort by cost descending for consistent ordering.
	sort.Slice(items, func(i, j int) bool {
		return items[i].CurrentCostUsd > items[j].CurrentCostUsd
	})

	return c.JSON(http.StatusOK, map[string]any{
		"data":      items,
		"total":     len(items),
		"scope":     scope,
		"periodKey": periodKey,
	})
}

// QuotaAnalyticsTrendPoint is one data point in the trend response.
type QuotaAnalyticsTrendPoint struct {
	PeriodKey string  `json:"periodKey"`
	CostUsd   float64 `json:"costUsd"`
}

// QuotaAnalyticsTrend returns per-period cost for a single target over N months.
//
// Query params:
//
//	targetType — user | vk | organization
//	targetId   — entity ID
//	periods    — number of past months to include (default 6, max 24)
func (h *Handler) QuotaAnalyticsTrend(c echo.Context) error {
	targetType := c.QueryParam("targetType")
	targetID := c.QueryParam("targetId")
	if targetType == "" || targetID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("targetType and targetId are required", "validation_error", ""))
	}

	periods := 6
	if v := c.QueryParam("periods"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 24 {
			periods = n
		}
	}

	dimension, ok := scopeToDimension(targetType)
	if !ok {
		return c.JSON(http.StatusBadRequest, errJSON(
			fmt.Sprintf("unsupported targetType %q", targetType), "validation_error", ""))
	}

	// Build monthly buckets for the past N months (oldest first).
	now := time.Now().UTC()
	data := make([]QuotaAnalyticsTrendPoint, 0, periods)

	for i := periods - 1; i >= 0; i-- {
		t := now.AddDate(0, -i, 0)
		periodKey := t.Format("2006-01")
		start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, 0)

		rows, err := h.metrics.QueryRollup(c.Request().Context(), metrics.MetricsQuery{
			Metrics:      []string{metrics.MetricEstimatedCostUSD},
			DimensionKey: dimension,
			SubDimension: "", // no sub-dimension filter
			StartTime:    start,
			EndTime:      end,
		})
		if err != nil {
			h.logger.Error("quota analytics trend: query rollup", "error", err, "periodKey", periodKey)
			// Return empty point for this period rather than aborting.
			data = append(data, QuotaAnalyticsTrendPoint{PeriodKey: periodKey, CostUsd: 0})
			continue
		}

		prefix := dimension + "=" + targetID
		var total float64
		for _, r := range rows {
			if r.DimensionKey == prefix {
				total += r.Value
			}
		}
		data = append(data, QuotaAnalyticsTrendPoint{PeriodKey: periodKey, CostUsd: total})
	}

	return c.JSON(http.StatusOK, map[string]any{
		"data":       data,
		"targetType": targetType,
		"targetId":   targetID,
		"periods":    periods,
	})
}

// QuotaAnalyticsTopItem is one entry in the top-N response.
type QuotaAnalyticsTopItem struct {
	EntityID     string  `json:"entityId"`
	EntityName   string  `json:"entityName"`
	EntityType   string  `json:"entityType"`
	TotalCostUsd float64 `json:"totalCostUsd"`
}

// QuotaAnalyticsTop returns the top-N consumers for a given scope and period.
//
// Query params:
//
//	scope     — user | vk | organization (default: user)
//	periodKey — YYYY-MM (default: current month)
//	limit     — number of top entries to return (default 10, max 100)
func (h *Handler) QuotaAnalyticsTop(c echo.Context) error {
	scope := c.QueryParam("scope")
	if scope == "" {
		scope = "user"
	}
	periodKey := c.QueryParam("periodKey")
	if periodKey == "" {
		periodKey = currentMonthPeriodKey()
	}
	limit := 10
	if v := c.QueryParam("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	dimension, ok := scopeToDimension(scope)
	if !ok {
		return c.JSON(http.StatusBadRequest, errJSON(
			fmt.Sprintf("unsupported scope %q", scope), "validation_error", ""))
	}

	start, end, err := parsePeriodKey(periodKey)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", ""))
	}

	rows, err := h.metrics.QueryRollup(c.Request().Context(), metrics.MetricsQuery{
		Metrics:      []string{metrics.MetricEstimatedCostUSD},
		DimensionKey: dimension,
		StartTime:    start,
		EndTime:      end,
	})
	if err != nil {
		h.logger.Error("quota analytics top: query rollup", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	// Aggregate cost per dimension value.
	totals := map[string]float64{}
	prefix := dimension + "="
	for _, r := range rows {
		if !strings.HasPrefix(r.DimensionKey, prefix) {
			continue
		}
		targetID := strings.TrimPrefix(r.DimensionKey, prefix)
		totals[targetID] += r.Value
	}

	items := make([]QuotaAnalyticsTopItem, 0, len(totals))
	for targetID, cost := range totals {
		items = append(items, QuotaAnalyticsTopItem{
			EntityID:     targetID,
			EntityName:   h.resolveEntityName(c.Request().Context(), scope, targetID),
			EntityType:   scope,
			TotalCostUsd: cost,
		})
	}

	// Sort descending by cost.
	sort.Slice(items, func(i, j int) bool {
		return items[i].TotalCostUsd > items[j].TotalCostUsd
	})
	if len(items) > limit {
		items = items[:limit]
	}

	return c.JSON(http.StatusOK, map[string]any{
		"data":      items,
		"scope":     scope,
		"periodKey": periodKey,
		"limit":     limit,
	})
}

// resolveEntityName looks up a human-readable name for a target ID based on scope.
// Falls back to the raw ID if the lookup fails.
func (h *Handler) resolveEntityName(ctx context.Context, scope, targetID string) string {
	switch scope {
	case "user":
		u, err := h.users.GetNexusUserSafe(ctx, targetID)
		if err == nil && u != nil {
			if u.DisplayName != "" {
				return u.DisplayName
			}
			if u.Email != nil && *u.Email != "" {
				return *u.Email
			}
		}
	case "organization", "project":
		org, err := h.orgs.GetOrganization(ctx, targetID)
		if err == nil && org != nil && org.Name != "" {
			return org.Name
		}
	case "vk", "virtual_key":
		vk, err := h.vks.GetVirtualKey(ctx, targetID)
		if err == nil && vk != nil && vk.Name != "" {
			return vk.Name
		}
	}
	return targetID
}
