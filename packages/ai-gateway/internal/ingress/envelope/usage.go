package envelope

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
)

const maxDailyRangeDays = 90

// usageStore is the DB seam consumed by the usage handlers.
// *store.DB satisfies this interface at all wiring call sites via Go
// structural typing — no changes to that package are required.
type usageStore interface {
	GetDailyUsageForVK(ctx context.Context, virtualKeyID string, start, end time.Time) ([]store.DailyModelUsage, error)
}

// vkAuthenticator is the auth seam consumed by the usage handlers.
// *vkauth.Authenticator satisfies this interface at all wiring call sites
// via Go structural typing — no changes to that package are required.
type vkAuthenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (*vkauth.VKMeta, error)
}

type usageSummaryResponse struct {
	VirtualKeyID string      `json:"virtualKeyId"`
	Period       string      `json:"period"`
	PeriodType   string      `json:"periodType"`
	Usage        usageBlock  `json:"usage"`
	Quota        *quotaBlock `json:"quota"`
}

type usageBlock struct {
	TotalRequests    int64   `json:"totalRequests"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	EstimatedCostUsd float64 `json:"estimatedCostUsd"`
}

type quotaBlock struct {
	LimitUsd        float64 `json:"limitUsd"`
	UsedUsd         float64 `json:"usedUsd"`
	RemainingUsd    float64 `json:"remainingUsd"`
	EnforcementMode string  `json:"enforcementMode"`
	RateLimitRpm    *int    `json:"rateLimitRpm"`
}

type dailyResponse struct {
	VirtualKeyID string     `json:"virtualKeyId"`
	StartDate    string     `json:"startDate"`
	EndDate      string     `json:"endDate"`
	Daily        []dayEntry `json:"daily"`
	Totals       totalBlock `json:"totals"`
}

type dayEntry struct {
	Date             string           `json:"date"`
	Requests         int64            `json:"requests"`
	PromptTokens     int64            `json:"promptTokens"`
	CompletionTokens int64            `json:"completionTokens"`
	TotalTokens      int64            `json:"totalTokens"`
	CostUsd          float64          `json:"costUsd"`
	Models           []modelBreakdown `json:"models"`
}

type modelBreakdown struct {
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	CostUsd          float64 `json:"costUsd"`
}

type totalBlock struct {
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	CostUsd          float64 `json:"costUsd"`
}

// UsageSummaryHandler handles GET /v1/usage — real-time summary + quota status.
// db and vkAuth are injected via interfaces so the package can be unit-tested
// without a live PostgreSQL connection or real HMAC-backed authenticator.
func UsageSummaryHandler(db usageStore, vkAuth vkAuthenticator, quotaEngine *quota.Engine, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeDetailedError(w, http.StatusInternalServerError, "USAGE_QUERY_FAILED",
				"database not available", "")
			return
		}

		vkMeta, err := vkAuth.Authenticate(r.Context(), r)
		if err != nil {
			writeDetailedError(w, http.StatusUnauthorized, "AUTH_INVALID",
				"authentication required", "Provide a valid Virtual Key in the Authorization header")
			return
		}

		periodKey := quota.CurrentPeriodKey("monthly")

		resp := usageSummaryResponse{
			VirtualKeyID: vkMeta.ID,
			Period:       periodKey,
			PeriodType:   "monthly",
		}

		// Read current-period usage from Redis (via quota engine) or fall back to DB.
		if quotaEngine != nil {
			usedCents, qErr := quotaEngine.UsageForTarget(r.Context(), "virtual_key", vkMeta.ID, periodKey)
			if qErr != nil {
				logger.Warn("usage cache read failed, falling back to DB", "error", qErr, "vkId", vkMeta.ID)
			} else {
				resp.Usage.EstimatedCostUsd = float64(usedCents) / 100
			}
		}

		// Always read token counts from DB (Redis only tracks cost cents).
		periodStart := parsePeriodStart(periodKey)
		periodEnd := periodStart.AddDate(0, 1, 0)
		dbRows, err := db.GetDailyUsageForVK(r.Context(), vkMeta.ID, periodStart, periodEnd)
		if err != nil {
			logger.Error("usage DB query failed", "error", err, "vkId", vkMeta.ID)
			writeDetailedError(w, http.StatusInternalServerError, "USAGE_QUERY_FAILED",
				"failed to query usage", "")
			return
		}

		for _, row := range dbRows {
			resp.Usage.TotalRequests += row.Requests
			resp.Usage.PromptTokens += row.PromptTokens
			resp.Usage.CompletionTokens += row.CompletionTokens
			resp.Usage.TotalTokens += row.TotalTokens
		}
		// If we didn't get cost from Redis, sum from DB rows.
		if quotaEngine == nil || resp.Usage.EstimatedCostUsd == 0 {
			for _, row := range dbRows {
				resp.Usage.EstimatedCostUsd += row.CostUsd
			}
		}

		// Build quota block from the new policy/override system. The
		// engine's VKLimit helper resolves the VK-level override (then
		// policy fallback) and returns the active limit + current period
		// usage; when neither applies we fall through to the rate-limit
		// only block.
		var quotaBuilt bool
		if quotaEngine != nil {
			if limitCents, currentCents, _, has := quotaEngine.VKLimit(r.Context(), vkMeta); has {
				limit := float64(limitCents) / 100
				used := float64(currentCents) / 100
				remaining := limit - used
				if remaining < 0 {
					remaining = 0
				}
				resp.Quota = &quotaBlock{
					LimitUsd:        limit,
					UsedUsd:         used,
					RemainingUsd:    remaining,
					EnforcementMode: "reject",
					RateLimitRpm:    vkMeta.RateLimitRpm,
				}
				quotaBuilt = true
			}
		}
		if !quotaBuilt && vkMeta.RateLimitRpm != nil {
			resp.Quota = &quotaBlock{
				EnforcementMode: "none",
				RateLimitRpm:    vkMeta.RateLimitRpm,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(resp)
	}
}

// UsageDailyHandler handles GET /v1/usage/daily — daily time-series with
// model/provider breakdowns.
// db and vkAuth are injected via interfaces so the package can be unit-tested
// without a live PostgreSQL connection or real HMAC-backed authenticator.
func UsageDailyHandler(db usageStore, vkAuth vkAuthenticator, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeDetailedError(w, http.StatusInternalServerError, "USAGE_QUERY_FAILED",
				"database not available", "")
			return
		}

		vkMeta, err := vkAuth.Authenticate(r.Context(), r)
		if err != nil {
			writeDetailedError(w, http.StatusUnauthorized, "AUTH_INVALID",
				"authentication required", "Provide a valid Virtual Key in the Authorization header")
			return
		}

		now := time.Now().UTC()
		startDate, endDate, parseErr := parseDateRange(r, now)
		if parseErr != nil {
			writeDetailedError(w, http.StatusBadRequest, parseErr.code,
				parseErr.message, parseErr.hint)
			return
		}

		// Query DB.
		dbStart := time.Date(startDate.Year(), startDate.Month(), startDate.Day(), 0, 0, 0, 0, time.UTC)
		dbEnd := time.Date(endDate.Year(), endDate.Month(), endDate.Day()+1, 0, 0, 0, 0, time.UTC) // exclusive
		rows, err := db.GetDailyUsageForVK(r.Context(), vkMeta.ID, dbStart, dbEnd)
		if err != nil {
			logger.Error("daily usage query failed", "error", err, "vkId", vkMeta.ID)
			writeDetailedError(w, http.StatusInternalServerError, "USAGE_QUERY_FAILED",
				"failed to query daily usage", "")
			return
		}

		resp := buildDailyResponse(vkMeta.ID, startDate, endDate, rows)

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(resp)
	}
}

type dateParseError struct {
	code    string
	message string
	hint    string
}

func parseDateRange(r *http.Request, now time.Time) (start, end time.Time, err *dateParseError) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	start = today.AddDate(0, 0, -30)
	end = today

	if s := r.URL.Query().Get("startDate"); s != "" {
		parsed, pErr := time.Parse("2006-01-02", s)
		if pErr != nil {
			return time.Time{}, time.Time{}, &dateParseError{
				code:    "USAGE_INVALID_DATE",
				message: fmt.Sprintf("invalid startDate: %s", s),
				hint:    "Use YYYY-MM-DD format",
			}
		}
		start = parsed
	}
	if s := r.URL.Query().Get("endDate"); s != "" {
		parsed, pErr := time.Parse("2006-01-02", s)
		if pErr != nil {
			return time.Time{}, time.Time{}, &dateParseError{
				code:    "USAGE_INVALID_DATE",
				message: fmt.Sprintf("invalid endDate: %s", s),
				hint:    "Use YYYY-MM-DD format",
			}
		}
		end = parsed
	}

	if end.Before(start) {
		return time.Time{}, time.Time{}, &dateParseError{
			code:    "USAGE_INVALID_DATE",
			message: "endDate must be on or after startDate",
			hint:    "Swap startDate and endDate",
		}
	}

	days := int(end.Sub(start).Hours()/24) + 1
	if days > maxDailyRangeDays {
		return time.Time{}, time.Time{}, &dateParseError{
			code:    "USAGE_RANGE_TOO_LARGE",
			message: fmt.Sprintf("requested range of %d days exceeds maximum of %d", days, maxDailyRangeDays),
			hint:    fmt.Sprintf("Reduce range to %d days or less", maxDailyRangeDays),
		}
	}

	return start, end, nil
}

func buildDailyResponse(vkID string, start, end time.Time, rows []store.DailyModelUsage) *dailyResponse {
	type dayKey string
	dayMap := make(map[dayKey]*dayEntry)
	var dayOrder []dayKey

	var totals totalBlock

	for _, r := range rows {
		dk := dayKey(r.Day.Format("2006-01-02"))
		entry, ok := dayMap[dk]
		if !ok {
			entry = &dayEntry{Date: string(dk)}
			dayMap[dk] = entry
			dayOrder = append(dayOrder, dk)
		}
		entry.Requests += r.Requests
		entry.PromptTokens += r.PromptTokens
		entry.CompletionTokens += r.CompletionTokens
		entry.TotalTokens += r.TotalTokens
		entry.CostUsd += r.CostUsd
		entry.Models = append(entry.Models, modelBreakdown{
			Model:            r.ModelName,
			Provider:         r.ProviderName,
			Requests:         r.Requests,
			PromptTokens:     r.PromptTokens,
			CompletionTokens: r.CompletionTokens,
			TotalTokens:      r.TotalTokens,
			CostUsd:          r.CostUsd,
		})

		totals.Requests += r.Requests
		totals.PromptTokens += r.PromptTokens
		totals.CompletionTokens += r.CompletionTokens
		totals.TotalTokens += r.TotalTokens
		totals.CostUsd += r.CostUsd
	}

	daily := make([]dayEntry, 0, len(dayOrder))
	for _, dk := range dayOrder {
		daily = append(daily, *dayMap[dk])
	}

	return &dailyResponse{
		VirtualKeyID: vkID,
		StartDate:    start.Format("2006-01-02"),
		EndDate:      end.Format("2006-01-02"),
		Daily:        daily,
		Totals:       totals,
	}
}

// writeDetailedError writes a JSON error response with code/message/hint fields.
func writeDetailedError(w http.ResponseWriter, status int, code, message, hint string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	errBody := map[string]any{
		"message": message,
		"type":    "proxy_error",
		"code":    code,
	}
	if hint != "" {
		errBody["hint"] = hint
	}
	resp, _ := json.Marshal(map[string]any{"error": errBody})
	_, _ = w.Write(resp)
}

// parsePeriodStart parses "2026-04" into the first day of that month.
func parsePeriodStart(periodKey string) time.Time {
	t, err := time.Parse("2006-01", periodKey)
	if err != nil {
		return time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return t
}
