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

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/quota/quotastore"
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
//
// `project` is intentionally NOT mapped: the rollup pipeline does not emit a
// per-project dimension yet (the project dim is not backfilled — see the quota
// architecture doc §9). The previous `"project":"organization"` alias silently
// returned org-aggregated numbers under a project label AND made
// GetQuotaOverrideByTarget("project", <orgID>) query a project override against
// an organization ID, so project overrides never resolved. Until the
// project dimension is wired, an analytics scope/targetType of `project` is
// rejected with HTTP 400 rather than returning wrong data.
func scopeToDimension(scope string) (string, bool) {
	m := map[string]string{
		"user":         "user",
		"vk":           "virtual_key",
		"virtual_key":  "virtual_key",
		"project":      "project",
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
// Cost is read from the BILLED cost metric (billed_cost_usd: success-only,
// cache-hits excluded), the SAME base the gateway quota engine enforces against
// — so the displayed usage % matches what triggers throttling. The
// gross estimated_cost_usd metric is deliberately not used here.
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
		Metrics:      []string{metrics.MetricBilledCostUSD},
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

	// Load the enabled policies that could govern entities in this scope once, so
	// the per-entity effective-limit resolution does not re-query for every row.
	// Failure is non-fatal: usage is still reported, effective limits just fall
	// back to overrides only (the pre-fix behaviour) rather than aborting the page.
	policies, pErr := h.quota.ListEnabledPoliciesForScopes(c.Request().Context(), policyScopesForAnalytics(scope))
	if pErr != nil {
		h.logger.Warn("quota analytics overview: list policies failed; effective limits reflect overrides only", "error", pErr)
		policies = nil
	}

	items := make([]QuotaAnalyticsOverviewItem, 0, len(totals))
	for targetID, cost := range totals {
		item := QuotaAnalyticsOverviewItem{
			EntityID:       targetID,
			EntityName:     h.resolveEntityName(c.Request().Context(), scope, targetID),
			EntityType:     scope,
			CurrentCostUsd: cost,
		}
		// Resolve the effective cost cap through the SAME override→policy
		// precedence the gateway enforcement engine uses, so a policy-capped
		// entity (no override) shows its real cap instead of 0%/normal.
		if limit := h.resolveEffectiveCostLimit(c.Request().Context(), scope, targetID, policies); limit != nil {
			item.CostLimitUsd = limit
			if *limit > 0 {
				item.UsagePercent = (cost / *limit) * 100
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
//	targetType — user | vk | project | organization
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
			Metrics:      []string{metrics.MetricBilledCostUSD},
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
// Cost is the BILLED cost metric, matching enforcement (see QuotaAnalyticsOverview).
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
		Metrics:      []string{metrics.MetricBilledCostUSD},
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
	case "organization":
		org, err := h.orgs.GetOrganization(ctx, targetID)
		if err == nil && org != nil && org.Name != "" {
			return org.Name
		}
	case "project":
		proj, err := h.orgs.GetProject(ctx, targetID)
		if err == nil && proj != nil {
			if proj.Name != "" {
				return proj.Name
			}
			if proj.Code != "" {
				return proj.Code
			}
		}
	case "vk", "virtual_key":
		vk, err := h.vks.GetVirtualKey(ctx, targetID)
		if err == nil && vk != nil && vk.Name != "" {
			return vk.Name
		}
	}
	return targetID
}

// policyScopesForAnalytics returns the QuotaPolicy.scope values that govern
// entities of the given analytics scope. The repository uses both "vk" and
// "virtual_key" as the persisted scope for virtual-key policies (admin-created
// rows use "vk"; seed/gateway rows use "virtual_key"), so both are matched for
// the VK dimension. All other scopes map 1:1.
func policyScopesForAnalytics(scope string) []string {
	switch scope {
	case "vk", "virtual_key":
		return []string{"vk", "virtual_key"}
	default:
		return []string{scope}
	}
}

// entityScopeAttrs resolves the (organizationID, vkType) pair the gateway's
// PolicyCache.FindPolicy filters on for an entity of the given scope. Only the
// attributes a policy can filter by per scope are looked up:
//   - organization: the entity IS the org, so orgID = targetID.
//   - user:         the user's organization (user policies may be org-scoped).
//   - vk:           the VK's type (vk policies are vkType-scoped, never org-scoped).
//   - project:      project policies carry neither filter — nothing to resolve.
//
// Missing lookups degrade to empty strings, which the matcher treats as
// "matches any policy that does not constrain that attribute".
func (h *Handler) entityScopeAttrs(ctx context.Context, scope, targetID string) (orgID, vkType string) {
	switch scope {
	case "organization":
		return targetID, ""
	case "user":
		if h.users != nil {
			if oid, _, err := h.users.GetNexusUserOrgInfo(ctx, targetID); err == nil {
				return oid, ""
			}
		}
	case "vk", "virtual_key":
		if h.vks != nil {
			if vk, err := h.vks.GetVirtualKey(ctx, targetID); err == nil && vk != nil && vk.VKType != nil {
				return "", *vk.VKType
			}
		}
	}
	return "", ""
}

// resolveEffectiveCostLimit mirrors the gateway quota engine's override→policy
// precedence (enforcement.go Check + policy_cache.go FindPolicy):
//
//  1. An override on this target with an explicit cost cap wins outright.
//  2. Otherwise (no override, or an override that leaves the cap nil so it
//     inherits) the highest-priority policy whose org/vkType filters match the
//     entity supplies the cap.
//
// policies must already be filtered to the relevant scope(s) and ordered by
// priority DESC (ListEnabledPoliciesForScopes guarantees this). Returns nil when
// no override and no matching policy define a cap.
func (h *Handler) resolveEffectiveCostLimit(ctx context.Context, scope, targetID string, policies []quotastore.QuotaPolicy) *float64 {
	if override, err := h.quota.GetQuotaOverrideByTarget(ctx, scope, targetID); err == nil && override != nil && override.CostLimitUsd != nil {
		return override.CostLimitUsd
	}

	orgID, vkType := h.entityScopeAttrs(ctx, scope, targetID)
	for i := range policies {
		p := &policies[i]
		if p.OrganizationID != nil && *p.OrganizationID != "" && *p.OrganizationID != orgID {
			continue
		}
		if p.VKType != nil && *p.VKType != "" && *p.VKType != vkType {
			continue
		}
		return p.CostLimitUsd
	}
	return nil
}
