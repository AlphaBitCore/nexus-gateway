package analytics

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/labstack/echo/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// CacheROISummary is the top-level cache ROI analytics response.
type CacheROISummary struct {
	// Period
	Since      string `json:"since"`
	Until      string `json:"until"`
	PeriodDays int    `json:"periodDays"`

	// Aggregate totals
	TotalEstimatedCostUSD       float64 `json:"totalEstimatedCostUsd"`       // actual cost paid to providers (non-gateway-cached requests)
	TotalGatewayCacheSavingsUSD float64 `json:"totalGatewayCacheSavingsUsd"` // Gateway response cache savings (full upstream cost avoided)
	GatewayCacheHitCount        int64   `json:"gatewayCacheHitCount"`        // requests served from the gateway response cache
	TotalCacheWriteCostUSD      float64 `json:"totalCacheWriteCostUsd"`
	TotalCacheReadSavingsUSD    float64 `json:"totalCacheReadSavingsUsd"`
	TotalCacheNetSavingsUSD     float64 `json:"totalCacheNetSavingsUsd"`
	TotalPromptTokens           int64   `json:"totalPromptTokens"`
	TotalCompletionTokens       int64   `json:"totalCompletionTokens"`
	TotalCacheCreationTokens    int64   `json:"totalCacheCreationTokens"`
	TotalCacheReadTokens        int64   `json:"totalCacheReadTokens"`
	TotalNormalisedStripCount   int64   `json:"totalNormalisedStripCount"`
	TotalNormalisedStripBytes   int64   `json:"totalNormalisedStripBytes"`
	TotalMarkersInjected        int64   `json:"totalMarkersInjected"`
	RequestsWithCacheHit        int64   `json:"requestsWithCacheHit"` // requests with upstream provider prompt-cache hits

	// Breakdowns
	ByAdapterType []CacheROIByAdapter `json:"byAdapter"`
	Daily         []CacheROIDay       `json:"daily"`

	// DataSource indicates whether aggregate totals were served from rollup
	// tables ("rollup") or computed on-the-fly from raw traffic_event rows
	// ("direct"). The UI uses this to surface a prompt to run rollup jobs
	// when rollup tables are empty (e.g. fresh deployment).
	DataSource string `json:"dataSource"`
}

type CacheROIByAdapter struct {
	AdapterType            string  `json:"adapter"`
	EstimatedCostUSD       float64 `json:"estimatedCostUsd"`       // actual cost paid to providers (non-gateway-cached requests)
	GatewayCacheSavingsUSD float64 `json:"gatewayCacheSavingsUsd"` // Gateway response cache savings (full upstream cost avoided) for this adapter
	GatewayCacheHitCount   int64   `json:"gatewayCacheHitCount"`   // Gateway cache hits for this adapter
	CacheWriteCostUSD      float64 `json:"cacheWriteCostUsd"`
	CacheReadSavingsUSD    float64 `json:"cacheReadSavingsUsd"`
	CacheNetSavingsUSD     float64 `json:"cacheNetSavingsUsd"`
	PromptTokens           int64   `json:"promptTokens"`
	CompletionTokens       int64   `json:"completionTokens"`
	CacheCreationTokens    int64   `json:"cacheCreationTokens"`
	CacheReadTokens        int64   `json:"cacheReadTokens"`
	RequestsWithCacheHit   int64   `json:"requestsWithCacheHit"`
}

type CacheROIDay struct {
	Date                   string  `json:"date"`
	GatewayCacheSavingsUSD float64 `json:"gatewayCacheSavingsUsd"` // Gateway response cache savings (full upstream cost avoided) for this day
	CacheWriteCostUSD      float64 `json:"cacheWriteCostUsd"`
	CacheReadSavingsUSD    float64 `json:"cacheReadSavingsUsd"`
	CacheNetSavingsUSD     float64 `json:"cacheNetSavingsUsd"`
	CacheCreationTokens    int64   `json:"cacheCreationTokens"`
	CacheReadTokens        int64   `json:"cacheReadTokens"`
}

// cacheROIMetrics is the set of metric names needed for cache ROI rollup queries.
var cacheROIMetrics = []string{
	metrics.MetricEstimatedCostUSD,
	metrics.MetricGatewayCacheSavingsUSD,
	metrics.MetricCacheHitCount,
	metrics.MetricCacheWriteCostUSD,
	metrics.MetricCacheReadSavingsUSD,
	metrics.MetricCacheNetSavingsUSD,
	metrics.MetricPromptTokens,
	metrics.MetricCompletionTokens,
	metrics.MetricCacheCreationTokens,
	metrics.MetricCacheReadTokens,
	metrics.MetricRequestsWithProviderPromptCacheHit,
	metrics.MetricNormalisedStripCount,
	metrics.MetricNormalisedStripBytes,
	metrics.MetricCacheMarkersInjected,
}

// rollupCacheROITotals reads aggregate cache ROI totals from rollup tables.
// Returns (filled summary, true) when rollup has data; (zero, false) to signal
// the caller should fall back to a direct traffic_event query.
func (h *Handler) rollupCacheROITotals(ctx context.Context, since, until time.Time) (CacheROISummary, bool) {
	q := metrics.MetricsQuery{
		Metrics:   cacheROIMetrics,
		StartTime: since,
		EndTime:   until,
	}
	rows, err := h.metrics.QueryRollupCascade(ctx, q)
	if err != nil || len(rows) == 0 {
		return CacheROISummary{}, false
	}
	gran := metrics.SelectGranularity(since, until)
	result := metrics.BuildResult(q, rows, gran)
	s := result.Summary
	var total CacheROISummary
	total.TotalEstimatedCostUSD = s[metrics.MetricEstimatedCostUSD]
	total.TotalGatewayCacheSavingsUSD = s[metrics.MetricGatewayCacheSavingsUSD]
	total.GatewayCacheHitCount = int64(s[metrics.MetricCacheHitCount])
	total.TotalCacheWriteCostUSD = s[metrics.MetricCacheWriteCostUSD]
	total.TotalCacheReadSavingsUSD = s[metrics.MetricCacheReadSavingsUSD]
	total.TotalCacheNetSavingsUSD = s[metrics.MetricCacheNetSavingsUSD]
	total.TotalPromptTokens = int64(s[metrics.MetricPromptTokens])
	total.TotalCompletionTokens = int64(s[metrics.MetricCompletionTokens])
	total.TotalCacheCreationTokens = int64(s[metrics.MetricCacheCreationTokens])
	total.TotalCacheReadTokens = int64(s[metrics.MetricCacheReadTokens])
	total.RequestsWithCacheHit = int64(s[metrics.MetricRequestsWithProviderPromptCacheHit])
	total.TotalNormalisedStripCount = int64(s[metrics.MetricNormalisedStripCount])
	total.TotalNormalisedStripBytes = int64(s[metrics.MetricNormalisedStripBytes])
	total.TotalMarkersInjected = int64(s[metrics.MetricCacheMarkersInjected])
	return total, true
}

// rollupCacheROIDaily reads per-day cache ROI series using the full rollup
// cascade (1mo→1d→1h→5m) so that recent data not yet merged into metric_rollup_1d
// is included. Rows from finer-grained tables are grouped into UTC-day buckets,
// making the daily chart consistent with the aggregate totals and byAdapter
// breakdown (both of which also use QueryRollupCascade).
func (h *Handler) rollupCacheROIDaily(ctx context.Context, since, until time.Time) []CacheROIDay {
	q := metrics.MetricsQuery{
		Metrics:   cacheROIMetrics,
		StartTime: since,
		EndTime:   until,
	}
	rows, err := h.metrics.QueryRollupCascade(ctx, q)
	if err != nil || len(rows) == 0 {
		return nil
	}

	allowed := map[string]bool{}
	for _, m := range cacheROIMetrics {
		allowed[m] = true
	}

	type dayAcc struct {
		gatewaySavings, writeCost, readSavings, netSavings float64
		creationTokens, readTokens                         float64
	}
	byDay := make(map[string]*dayAcc)
	var order []string

	for _, r := range rows {
		if !allowed[r.MetricName] {
			continue
		}
		dayKey := r.BucketStart.UTC().Truncate(24 * time.Hour).Format("2006-01-02")
		acc, ok := byDay[dayKey]
		if !ok {
			acc = &dayAcc{}
			byDay[dayKey] = acc
			order = append(order, dayKey)
		}
		switch r.MetricName {
		case metrics.MetricGatewayCacheSavingsUSD:
			acc.gatewaySavings += r.Value
		case metrics.MetricCacheWriteCostUSD:
			acc.writeCost += r.Value
		case metrics.MetricCacheReadSavingsUSD:
			acc.readSavings += r.Value
		case metrics.MetricCacheNetSavingsUSD:
			acc.netSavings += r.Value
		case metrics.MetricCacheCreationTokens:
			acc.creationTokens += r.Value
		case metrics.MetricCacheReadTokens:
			acc.readTokens += r.Value
		}
	}

	sort.Strings(order)
	out := make([]CacheROIDay, 0, len(order))
	for _, dayKey := range order {
		acc := byDay[dayKey]
		out = append(out, CacheROIDay{
			Date:                   dayKey,
			GatewayCacheSavingsUSD: acc.gatewaySavings,
			CacheWriteCostUSD:      acc.writeCost,
			CacheReadSavingsUSD:    acc.readSavings,
			CacheNetSavingsUSD:     acc.netSavings,
			CacheCreationTokens:    int64(acc.creationTokens),
			CacheReadTokens:        int64(acc.readTokens),
		})
	}
	return out
}

// AnalyticsCacheROI returns cache ROI metrics aggregated over the requested time range.
// GET /api/admin/analytics/cache-roi?start=<RFC3339>&end=<RFC3339>
//
// Aggregation contract: the top-level totals (TotalEstimatedCostUSD,
// TotalCacheWriteCostUSD, TotalCacheNetSavingsUSD, …) are FLEET-WIDE combined
// sums across every provider/adapter — they deliberately have NO provider join
// or GROUP BY. This is correct for an ROI *summary*: it answers "what did the
// cache save the whole deployment", a single combined figure the dashboard shows
// at the top. Per-provider attribution is NOT lost — it is the explicit job of
// the ByAdapterType breakdown below, which groups by routed_provider →
// adapter_type. A tenant on OpenAI + Gemini sees one combined total AND a
// per-adapter split; the two serve different questions and must not be conflated.
//
// Derived ratios: this endpoint returns only raw component sums; it
// does NOT compute the "ROI multiplier" (net savings ÷ write cost). That ratio
// is derived once at the single rendering site (control-plane-ui
// CacheROIDashboard), which guards the zero-denominator case
// (totalCacheWriteCostUsd == 0 → render "—") so a new tenant with no cache
// writes yet never sees an Inf/NaN. Keeping the division at one UI site avoids
// duplicating the guard across backend + frontend.
func (h *Handler) AnalyticsCacheROI(c echo.Context) error {
	if h.pool == nil {
		return c.JSON(http.StatusInternalServerError, errJSON("database not available", "db_unavailable", ""))
	}

	start, end := parseTimeRange(c)
	now := time.Now().UTC()

	since := now.AddDate(0, 0, -30)
	if start != nil {
		since = *start
	}
	until := now
	if end != nil {
		until = *end
	}
	periodDays := int(until.Sub(since).Hours()/24) + 1

	ctx := c.Request().Context()

	// Aggregate totals — try rollup first; fall back to direct traffic_event scan.
	var total CacheROISummary
	if rollupTotal, ok := h.rollupCacheROITotals(ctx, since, until); ok {
		total = rollupTotal
		total.DataSource = "rollup"
	} else {
		total.DataSource = "direct"
		err := h.pool.QueryRow(ctx, `
			SELECT
				COALESCE(SUM(estimated_cost_usd),        0),
				COALESCE(SUM(gateway_cache_savings_usd), 0),
				COUNT(*) FILTER (WHERE gateway_cache_status IN ('hit', 'hit_inflight')),
				COALESCE(SUM(cache_write_cost_usd),      0),
				COALESCE(SUM(cache_read_savings_usd),    0),
				COALESCE(SUM(cache_net_savings_usd),     0),
				COALESCE(SUM(prompt_tokens),             0),
				COALESCE(SUM(completion_tokens),         0),
				COALESCE(SUM(cache_creation_tokens),     0),
				COALESCE(SUM(cache_read_tokens),         0),
				COALESCE(SUM(normalized_strip_count),    0),
				COALESCE(SUM(normalized_strip_bytes),    0),
				COALESCE(SUM(cache_marker_injected),     0),
				COUNT(*) FILTER (WHERE cache_read_tokens IS NOT NULL AND cache_read_tokens > 0)
			FROM traffic_event
			WHERE timestamp >= $1 AND timestamp < $2
		`, since, until).Scan(
			&total.TotalEstimatedCostUSD,
			&total.TotalGatewayCacheSavingsUSD,
			&total.GatewayCacheHitCount,
			&total.TotalCacheWriteCostUSD,
			&total.TotalCacheReadSavingsUSD,
			&total.TotalCacheNetSavingsUSD,
			&total.TotalPromptTokens,
			&total.TotalCompletionTokens,
			&total.TotalCacheCreationTokens,
			&total.TotalCacheReadTokens,
			&total.TotalNormalisedStripCount,
			&total.TotalNormalisedStripBytes,
			&total.TotalMarkersInjected,
			&total.RequestsWithCacheHit,
		)
		if err != nil {
			h.logger.Error("cache roi: totals query failed", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("query failed", "server_error", ""))
		}
	}

	// Breakdown by adapter — rollup by routed_provider, then resolve adapter type.
	var byAdapter []CacheROIByAdapter
	adapterQ := metrics.MetricsQuery{
		Metrics:      cacheROIMetrics,
		DimensionKey: "routed_provider",
		StartTime:    since,
		EndTime:      until,
	}
	if adapterRollupRows, err := h.metrics.QueryRollupCascade(ctx, adapterQ); err == nil && len(adapterRollupRows) > 0 {
		result := metrics.BuildResult(adapterQ, adapterRollupRows, metrics.SelectGranularity(since, until))
		provIDs := make([]string, 0, len(result.Groups))
		for _, g := range result.Groups {
			_, provID := metrics.ParseDimensionKey(g.DimensionKey)
			if provID != "" {
				provIDs = append(provIDs, provID)
			}
		}
		adapterByProv := h.fetchProviderAdapterTypes(ctx, provIDs)

		type adapterAcc struct {
			costUSD, gatewaySavings, writeCost, readSavings, netSavings                               float64
			gatewayHits, promptTokens, completionTokens, creationTokens, readTokens, reqsWithCacheHit int64
		}
		byType := make(map[string]*adapterAcc)
		for _, g := range result.Groups {
			_, provID := metrics.ParseDimensionKey(g.DimensionKey)
			at := adapterByProv[provID]
			if at == "" {
				at = "unknown"
			}
			a := byType[at]
			if a == nil {
				a = &adapterAcc{}
				byType[at] = a
			}
			v := g.Values
			a.costUSD += v[metrics.MetricEstimatedCostUSD]
			a.gatewaySavings += v[metrics.MetricGatewayCacheSavingsUSD]
			a.gatewayHits += int64(v[metrics.MetricCacheHitCount])
			a.writeCost += v[metrics.MetricCacheWriteCostUSD]
			a.readSavings += v[metrics.MetricCacheReadSavingsUSD]
			a.netSavings += v[metrics.MetricCacheNetSavingsUSD]
			a.promptTokens += int64(v[metrics.MetricPromptTokens])
			a.completionTokens += int64(v[metrics.MetricCompletionTokens])
			a.creationTokens += int64(v[metrics.MetricCacheCreationTokens])
			a.readTokens += int64(v[metrics.MetricCacheReadTokens])
			a.reqsWithCacheHit += int64(v[metrics.MetricRequestsWithProviderPromptCacheHit])
		}
		for at, a := range byType {
			byAdapter = append(byAdapter, CacheROIByAdapter{
				AdapterType:            at,
				EstimatedCostUSD:       a.costUSD,
				GatewayCacheSavingsUSD: a.gatewaySavings,
				GatewayCacheHitCount:   a.gatewayHits,
				CacheWriteCostUSD:      a.writeCost,
				CacheReadSavingsUSD:    a.readSavings,
				CacheNetSavingsUSD:     a.netSavings,
				PromptTokens:           a.promptTokens,
				CompletionTokens:       a.completionTokens,
				CacheCreationTokens:    a.creationTokens,
				CacheReadTokens:        a.readTokens,
				RequestsWithCacheHit:   a.reqsWithCacheHit,
			})
		}
		sort.Slice(byAdapter, func(i, j int) bool {
			si := byAdapter[i].GatewayCacheSavingsUSD + byAdapter[i].CacheNetSavingsUSD
			sj := byAdapter[j].GatewayCacheSavingsUSD + byAdapter[j].CacheNetSavingsUSD
			return si > sj
		})
	} else {
		// Fallback: direct traffic_event query with Provider JOIN.
		// LEFT JOIN (not INNER) so rows with NULL provider_id (compliance-proxy,
		// agent, or traffic errored before routing) are included rather than
		// silently dropped; COALESCE maps them to the "unknown" bucket so that
		// Σ(byAdapter) == fleet-wide totals.
		adapterRows, err := h.pool.Query(ctx, `
			SELECT
				COALESCE(p.adapter_type, 'unknown') AS adapter_type,
				COALESCE(SUM(te.estimated_cost_usd),        0),
				COALESCE(SUM(te.gateway_cache_savings_usd), 0),
				COUNT(*) FILTER (WHERE te.gateway_cache_status IN ('hit', 'hit_inflight')),
				COALESCE(SUM(te.cache_write_cost_usd),      0),
				COALESCE(SUM(te.cache_read_savings_usd),    0),
				COALESCE(SUM(te.cache_net_savings_usd),     0),
				COALESCE(SUM(te.prompt_tokens),             0),
				COALESCE(SUM(te.completion_tokens),         0),
				COALESCE(SUM(te.cache_creation_tokens),     0),
				COALESCE(SUM(te.cache_read_tokens),         0),
				COUNT(*) FILTER (WHERE te.cache_read_tokens IS NOT NULL AND te.cache_read_tokens > 0)
			FROM traffic_event te
			LEFT JOIN "Provider" p ON p.id = COALESCE(te.routed_provider_id, te.provider_id)
			WHERE te.timestamp >= $1 AND te.timestamp < $2
			GROUP BY COALESCE(p.adapter_type, 'unknown')
			ORDER BY (COALESCE(SUM(te.gateway_cache_savings_usd), 0) + COALESCE(SUM(te.cache_net_savings_usd), 0)) DESC
		`, since, until)
		if err != nil {
			h.logger.Error("cache roi: adapter breakdown query failed", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("query failed", "server_error", ""))
		}
		defer adapterRows.Close()
		for adapterRows.Next() {
			var row CacheROIByAdapter
			if err := adapterRows.Scan(
				&row.AdapterType,
				&row.EstimatedCostUSD,
				&row.GatewayCacheSavingsUSD,
				&row.GatewayCacheHitCount,
				&row.CacheWriteCostUSD,
				&row.CacheReadSavingsUSD,
				&row.CacheNetSavingsUSD,
				&row.PromptTokens,
				&row.CompletionTokens,
				&row.CacheCreationTokens,
				&row.CacheReadTokens,
				&row.RequestsWithCacheHit,
			); err != nil {
				continue
			}
			byAdapter = append(byAdapter, row)
		}
	}
	if byAdapter == nil {
		byAdapter = []CacheROIByAdapter{}
	}

	// Daily time series — try rollup (1d table) first; fall back to direct scan.
	var daily []CacheROIDay
	if rollupDaily := h.rollupCacheROIDaily(ctx, since, until); len(rollupDaily) > 0 {
		daily = rollupDaily
	} else {
		loc := tzLoc(c)
		dailyRows, err := h.pool.Query(ctx, `
			SELECT
				DATE_TRUNC('day', timestamp AT TIME ZONE $3) AS day,
				COALESCE(SUM(gateway_cache_savings_usd), 0),
				COALESCE(SUM(cache_write_cost_usd),      0),
				COALESCE(SUM(cache_read_savings_usd),    0),
				COALESCE(SUM(cache_net_savings_usd),     0),
				COALESCE(SUM(cache_creation_tokens),     0),
				COALESCE(SUM(cache_read_tokens),         0)
			FROM traffic_event
			WHERE timestamp >= $1 AND timestamp < $2
			GROUP BY 1
			ORDER BY 1 ASC
		`, since, until, loc.String())
		if err != nil {
			h.logger.Error("cache roi: daily query failed", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("query failed", "server_error", ""))
		}
		defer dailyRows.Close()

		for dailyRows.Next() {
			var row CacheROIDay
			var day time.Time
			if err := dailyRows.Scan(
				&day,
				&row.GatewayCacheSavingsUSD,
				&row.CacheWriteCostUSD,
				&row.CacheReadSavingsUSD,
				&row.CacheNetSavingsUSD,
				&row.CacheCreationTokens,
				&row.CacheReadTokens,
			); err != nil {
				continue
			}
			row.Date = day.Format("2006-01-02")
			daily = append(daily, row)
		}
	}
	if daily == nil {
		daily = []CacheROIDay{}
	}

	total.Since = since.Format(time.RFC3339)
	total.Until = until.Format(time.RFC3339)
	total.PeriodDays = periodDays
	total.ByAdapterType = byAdapter
	total.Daily = daily

	return c.JSON(http.StatusOK, total)
}
