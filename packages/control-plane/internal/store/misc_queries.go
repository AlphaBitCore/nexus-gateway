package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// ProviderHealthRow holds a provider health record.
type ProviderHealthRow struct {
	ProviderID  string  `json:"providerId"`
	Provider    string  `json:"provider"`
	Status      string  `json:"status"`
	ErrorRate   float64 `json:"errorRate"`
	AvgLatency  int     `json:"avgLatencyMs"`
	SampleCount int     `json:"sampleCount"`
	LastReqAt   any     `json:"lastRequestAt"`
	LastErrAt   any     `json:"lastErrorAt"`
}

// ListProviderHealth returns all provider health records.
func (db *DB) ListProviderHealth(ctx context.Context) ([]ProviderHealthRow, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT "providerId", provider, status, "rollingErrorRate", "avgLatencyMs", "sampleCount", "lastRequestAt", "lastErrorAt"
		FROM "ProviderHealth" ORDER BY provider ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list provider health: %w", err)
	}
	defer rows.Close()

	result := []ProviderHealthRow{}
	for rows.Next() {
		var r ProviderHealthRow
		if err := rows.Scan(&r.ProviderID, &r.Provider, &r.Status, &r.ErrorRate, &r.AvgLatency, &r.SampleCount, &r.LastReqAt, &r.LastErrAt); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// MetricRollupBucket holds a single metric rollup bucket.
type MetricRollupBucket struct {
	BucketStart any     `json:"bucketStart"`
	Dimensions  string  `json:"dimensions"`
	Value       float64 `json:"value"`
}

// MetricAggregateRow matches the UI MetricAggregatePoint type.
type MetricAggregateRow struct {
	BucketStart  any             `json:"bucketStart"`
	MetricName   string          `json:"metricName"`
	DimensionKey string          `json:"dimensionKey"`
	Dimensions   json.RawMessage `json:"dimensions"`
	Value        string          `json:"value"`
}

// ListMetricRollupBuckets returns rollup buckets for a given metric.
// Reads from the new metric_rollup_1h table.
func (db *DB) ListMetricRollupBuckets(ctx context.Context, metricName string, limit int) ([]MetricRollupBucket, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT "bucketStart", "dimensionKey", "value" FROM metric_rollup_1h
		WHERE "metricName" = $1 ORDER BY "bucketStart" DESC LIMIT $2
	`, metricName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []MetricRollupBucket{}
	for rows.Next() {
		var b MetricRollupBucket
		if err := rows.Scan(&b.BucketStart, &b.Dimensions, &b.Value); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

// TopDestination holds a top destination host result.
type TopDestination struct {
	DestHost    string `json:"destHost"`
	EventCount  int    `json:"eventCount"`
	DeviceCount int    `json:"deviceCount"`
}

// ModelPricingRow holds a model pricing record.
type ModelPricingRow struct {
	ID                    string  `json:"id"`
	ModelID               string  `json:"modelId"`
	InputPricePerMillion  float64 `json:"inputPricePerMillion"`
	OutputPricePerMillion float64 `json:"outputPricePerMillion"`
	EffectiveDate         any     `json:"effectiveDate"`
}

// ListModelPricing returns all model pricing records.
func (db *DB) ListModelPricing(ctx context.Context) ([]ModelPricingRow, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, "modelId", "inputPricePerMillion", "outputPricePerMillion", "effectiveDate"
		FROM "ModelPricing" ORDER BY "effectiveDate" DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []ModelPricingRow{}
	for rows.Next() {
		var r ModelPricingRow
		if err := rows.Scan(&r.ID, &r.ModelID, &r.InputPricePerMillion, &r.OutputPricePerMillion, &r.EffectiveDate); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// CreateModelPricing upserts a model pricing record.
func (db *DB) CreateModelPricing(ctx context.Context, modelID string, inputPrice, outputPrice float64) (string, error) {
	var id string
	err := db.pool.QueryRow(ctx, `
		INSERT INTO "ModelPricing" (id, "modelId", "inputPricePerMillion", "outputPricePerMillion", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, NOW(), NOW())
		ON CONFLICT ("modelId") DO UPDATE SET "inputPricePerMillion" = $2, "outputPricePerMillion" = $3, "updatedAt" = NOW()
		RETURNING id
	`, modelID, inputPrice, outputPrice).Scan(&id)
	return id, err
}

// DeleteModelPricing deletes a pricing record by ID.
func (db *DB) DeleteModelPricing(ctx context.Context, id string) (int64, error) {
	tag, err := db.pool.Exec(ctx, `DELETE FROM "ModelPricing" WHERE id = $1`, id)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ComplianceCoverageStats holds compliance coverage statistics.
// TimePeriod represents a start/end time range echoed back to the client.
type TimePeriod struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// LabelCount is a generic label+count pair for top-N lists.
type LabelCount struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// ComplianceCoverageStats holds coverage percentage and per-bump_status breakdown.
type ComplianceCoverageStats struct {
	CoveragePct float64        `json:"coveragePercent"`
	Breakdown   map[string]int `json:"breakdown"`
	Period      TimePeriod     `json:"period"`
}

// GetComplianceCoverage returns compliance coverage stats for a time range.
// Reads from rollup tables first (O(buckets)); falls back to direct traffic_event scan.
func (db *DB) GetComplianceCoverage(ctx context.Context, start, end time.Time) (*ComplianceCoverageStats, error) {
	complianceMetrics := []metrics.MetricsQuery{
		{
			Metrics:      []string{metrics.MetricBumpSuccessCount, metrics.MetricBumpFailedCount, metrics.MetricBumpExemptCount, metrics.MetricBumpDisabledCount, metrics.MetricRequestCount},
			SubDimension: "source=compliance-proxy",
			StartTime:    start, EndTime: end,
		},
		{
			Metrics:      []string{metrics.MetricBumpSuccessCount, metrics.MetricBumpFailedCount, metrics.MetricBumpExemptCount, metrics.MetricBumpDisabledCount, metrics.MetricRequestCount},
			SubDimension: "source=agent",
			StartTime:    start, EndTime: end,
		},
	}

	totalSuccess, totalFailed, totalExempt, totalDisabled, totalRequests := 0, 0, 0, 0, 0
	rollupOK := false
	for _, q := range complianceMetrics {
		rows, err := db.QueryRollupCascade(ctx, q)
		if err != nil || len(rows) == 0 {
			continue
		}
		rollupOK = true
		result := metrics.BuildResult(q, rows, metrics.SelectGranularity(start, end))
		s := result.Summary
		totalSuccess += int(s[metrics.MetricBumpSuccessCount])
		totalFailed += int(s[metrics.MetricBumpFailedCount])
		totalExempt += int(s[metrics.MetricBumpExemptCount])
		totalDisabled += int(s[metrics.MetricBumpDisabledCount])
		totalRequests += int(s[metrics.MetricRequestCount])
	}

	if rollupOK {
		breakdown := map[string]int{
			"BUMP_SUCCESS":            totalSuccess,
			"BUMP_FAILED_PASSTHROUGH": totalFailed,
			"BUMP_EXEMPT":             totalExempt,
			"BUMP_DISABLED":           totalDisabled,
		}
		pct := 0.0
		if totalRequests > 0 {
			pct = float64(totalSuccess) / float64(totalRequests) * 100
		}
		return &ComplianceCoverageStats{
			CoveragePct: pct,
			Breakdown:   breakdown,
			Period:      TimePeriod{Start: start, End: end},
		}, nil
	}

	// Fallback: direct traffic_event scan.
	dbRows, err := db.pool.Query(ctx, `
		SELECT COALESCE(bump_status, 'NONE'), COUNT(*)
		FROM traffic_event
		WHERE source IN ('compliance-proxy', 'agent') AND timestamp >= $1 AND timestamp <= $2
		GROUP BY bump_status
	`, start, end)
	if err != nil {
		return nil, fmt.Errorf("compliance coverage: %w", err)
	}
	defer dbRows.Close()

	breakdown := make(map[string]int)
	total := 0
	bumped := 0
	for dbRows.Next() {
		var status string
		var count int
		if err := dbRows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("compliance coverage scan: %w", err)
		}
		breakdown[status] = count
		total += count
		if status == "BUMP_SUCCESS" {
			bumped = count
		}
	}
	if err := dbRows.Err(); err != nil {
		return nil, fmt.Errorf("compliance coverage rows: %w", err)
	}

	pct := 0.0
	if total > 0 {
		pct = float64(bumped) / float64(total) * 100
	}
	return &ComplianceCoverageStats{
		CoveragePct: pct,
		Breakdown:   breakdown,
		Period:      TimePeriod{Start: start, End: end},
	}, nil
}

// HookDecisionBreakdown holds per-decision counts.
type HookDecisionBreakdown struct {
	Allow   int `json:"allow"`
	Deny    int `json:"deny"`
	Error   int `json:"error"`
	Unknown int `json:"unknown"`
}

// HookHealthStats holds hook health including decision breakdown, latency percentiles, and top reason codes.
type HookHealthStats struct {
	Total          int                   `json:"total"`
	ByDecision     HookDecisionBreakdown `json:"byDecision"`
	TopReasonCodes []LabelCount          `json:"topReasonCodes"`
	LatencyP50     *float64              `json:"latencyP50"`
	LatencyP95     *float64              `json:"latencyP95"`
	LatencyP99     *float64              `json:"latencyP99"`
	Period         TimePeriod            `json:"period"`
}

// GetHookHealth returns hook decision stats for a time range.
// Decision counts and latency percentiles are read from rollup tables first;
// top-10 reason codes are always a direct query (not in rollup).
func (db *DB) GetHookHealth(ctx context.Context, start, end time.Time) (*HookHealthStats, error) {
	s := HookHealthStats{
		Period:         TimePeriod{Start: start, End: end},
		TopReasonCodes: []LabelCount{},
	}

	hookMetrics := []string{
		metrics.MetricHookAllowCount,
		metrics.MetricHookDenyCount,
		metrics.MetricHookErrorCount,
		metrics.MetricHookUnknownCount,
		metrics.MetricLatencyHistogram,
	}
	q := metrics.MetricsQuery{Metrics: hookMetrics, StartTime: start, EndTime: end}
	rollupRows, err := db.QueryRollupCascade(ctx, q)
	if err == nil && len(rollupRows) > 0 {
		result := metrics.BuildResult(q, rollupRows, metrics.SelectGranularity(start, end))
		sm := result.Summary
		s.ByDecision.Allow = int(sm[metrics.MetricHookAllowCount])
		s.ByDecision.Deny = int(sm[metrics.MetricHookDenyCount])
		s.ByDecision.Error = int(sm[metrics.MetricHookErrorCount])
		s.ByDecision.Unknown = int(sm[metrics.MetricHookUnknownCount])
		s.Total = s.ByDecision.Allow + s.ByDecision.Deny + s.ByDecision.Error + s.ByDecision.Unknown

		if result.Metadata != nil {
			if hRaw, ok := result.Metadata[metrics.MetricLatencyHistogram]; ok {
				if h, ok2 := hRaw.(metrics.Histogram); ok2 {
					total := int64(0)
					for _, v := range h {
						total += v
					}
					if total > 0 {
						p50 := h.Percentile(0.50)
						p95 := h.Percentile(0.95)
						p99 := h.Percentile(0.99)
						s.LatencyP50 = &p50
						s.LatencyP95 = &p95
						s.LatencyP99 = &p99
					}
				}
			}
		}
	} else {
		// Fallback: direct traffic_event scan for decision counts.
		if err := db.pool.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE request_hook_decision IS NOT NULL),
				COUNT(*) FILTER (WHERE request_hook_decision = 'APPROVE'),
				COUNT(*) FILTER (WHERE request_hook_decision IN ('REJECT_HARD', 'BLOCK_SOFT')),
				COUNT(*) FILTER (WHERE request_hook_decision NOT IN ('APPROVE', 'REJECT_HARD', 'BLOCK_SOFT', 'MODIFY', 'ABSTAIN') AND request_hook_decision IS NOT NULL),
				COUNT(*) FILTER (WHERE request_hook_decision IS NOT NULL
					AND request_hook_decision NOT IN ('APPROVE', 'REJECT_HARD', 'BLOCK_SOFT'))
			FROM traffic_event WHERE timestamp >= $1 AND timestamp <= $2
		`, start, end).Scan(&s.Total, &s.ByDecision.Allow, &s.ByDecision.Deny, &s.ByDecision.Error, &s.ByDecision.Unknown); err != nil {
			return nil, fmt.Errorf("hook health decisions: %w", err)
		}

		// Fallback: direct latency percentiles.
		if err := db.pool.QueryRow(ctx, `
			SELECT
				percentile_cont(0.50) WITHIN GROUP (ORDER BY latency_ms),
				percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms),
				percentile_cont(0.99) WITHIN GROUP (ORDER BY latency_ms)
			FROM traffic_event
			WHERE request_hook_decision IS NOT NULL AND latency_ms IS NOT NULL
				AND timestamp >= $1 AND timestamp <= $2
		`, start, end).Scan(&s.LatencyP50, &s.LatencyP95, &s.LatencyP99); err != nil {
			return nil, fmt.Errorf("hook health latency: %w", err)
		}
	}

	// Top-10 reason codes: always direct (not stored in rollup).
	reasonRows, err := db.pool.Query(ctx, `
		SELECT request_hook_reason_code, COUNT(*) AS cnt
		FROM traffic_event
		WHERE request_hook_decision IN ('REJECT_HARD', 'BLOCK_SOFT') AND request_hook_reason_code IS NOT NULL
			AND timestamp >= $1 AND timestamp <= $2
		GROUP BY request_hook_reason_code
		ORDER BY cnt DESC
		LIMIT 10
	`, start, end)
	if err != nil {
		return nil, fmt.Errorf("hook health reason codes: %w", err)
	}
	defer reasonRows.Close()

	for reasonRows.Next() {
		var lc LabelCount
		if err := reasonRows.Scan(&lc.Label, &lc.Count); err != nil {
			return nil, fmt.Errorf("hook health reason codes scan: %w", err)
		}
		s.TopReasonCodes = append(s.TopReasonCodes, lc)
	}
	if err := reasonRows.Err(); err != nil {
		return nil, fmt.Errorf("hook health reason codes rows: %w", err)
	}

	return &s, nil
}

// RejectStats holds reject statistics with top-N breakdowns.
type RejectStats struct {
	TotalRejects   int          `json:"totalRejects"`
	TopTargets     []LabelCount `json:"topTargets"`
	TopReasonCodes []LabelCount `json:"topReasonCodes"`
	BySource       []LabelCount `json:"bySource"`
	Period         TimePeriod   `json:"period"`
}

// MatrixAuditRow holds a matrix audit event for proxy list/detail.
type MatrixAuditRow struct {
	ID             string   `json:"id"`
	TransactionID  string   `json:"transactionId"`
	SourceIP       string   `json:"sourceIp"`
	TargetHost     string   `json:"targetHost"`
	Method         *string  `json:"method"`
	Path           *string  `json:"path"`
	StatusCode     *int     `json:"statusCode"`
	HookDecision   *string  `json:"hookDecision"`
	HookReasonCode *string  `json:"hookReasonCode"`
	LatencyMs      *int     `json:"latencyMs"`
	Timestamp      any      `json:"timestamp"`
	ComplianceTags []string `json:"complianceTags"`
}

// ListMatrixAuditEvents returns paginated compliance-proxy/agent traffic
// events. A non-nil start/end narrows the window on traffic_event.timestamp;
// pass nil to skip that bound (e.g. the UI list view wants recent-N without
// forcing the caller to supply a window, while CSV export always has one).
//
// Rows emitted only for kill-switch toggle auditing (SYSTEM hook decision,
// synthetic target_host killswitch) are excluded — they are operational signals,
// not intercepted customer traffic; admins use the Kill Switch / config-sync
// surfaces for that history.
func (db *DB) ListMatrixAuditEvents(ctx context.Context, start, end *time.Time, limit, offset int) ([]MatrixAuditRow, int, error) {
	// Use COALESCE on request_hook_decision/bump_status so "NOT (...)" stays boolean when
	// those columns are NULL (otherwise SQL UNKNOWN would drop rows from the result).
	where := `WHERE source IN ('compliance-proxy', 'agent')
		AND NOT (
			COALESCE(request_hook_decision, '') = 'SYSTEM'
			AND COALESCE(bump_status, '') = 'SYSTEM_EVENT'
			AND COALESCE(target_host, '') = 'killswitch'
		)`
	args := []any{}
	n := 1
	if start != nil {
		where += fmt.Sprintf(" AND timestamp >= $%d", n)
		args = append(args, *start)
		n++
	}
	if end != nil {
		where += fmt.Sprintf(" AND timestamp <= $%d", n)
		args = append(args, *end)
		n++
	}

	query := fmt.Sprintf(`
		SELECT id, COALESCE(details->>'transactionId', id), COALESCE(source_ip, ''), COALESCE(target_host, ''), method, path, status_code,
			request_hook_decision, request_hook_reason_code, latency_ms, timestamp, compliance_tags
		FROM traffic_event
		%s
		ORDER BY timestamp DESC
		LIMIT $%d OFFSET $%d
	`, where, n, n+1)
	// Keep `args` (the WHERE params) separate from `queryArgs` (WHERE +
	// pagination) — the count query below reuses `args` without limit/offset.
	queryArgs := append(args, limit, offset) //nolint:gocritic // appendAssign: intentional new slice; see comment above
	rows, err := db.pool.Query(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	result := []MatrixAuditRow{}
	for rows.Next() {
		var r MatrixAuditRow
		if err := rows.Scan(&r.ID, &r.TransactionID, &r.SourceIP, &r.TargetHost, &r.Method, &r.Path,
			&r.StatusCode, &r.HookDecision, &r.HookReasonCode, &r.LatencyMs, &r.Timestamp, &r.ComplianceTags); err != nil {
			return nil, 0, err
		}
		result = append(result, r)
	}

	var total int
	countQuery := "SELECT COUNT(*) FROM traffic_event " + where
	if err := db.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return result, 0, err
	}
	return result, total, rows.Err()
}

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
func (db *DB) GetProviderAnalyticsDetail(ctx context.Context, providerID string, start, end *time.Time) (*ProviderDetailResult, error) {
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
	// Per-provider avg us + upstream phase latency (only rows with non-NULL
	// upstream_total_ms contribute to the upstream avg; us avg uses ALL rows
	// with both columns populated).
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
	if err := db.pool.QueryRow(ctx, summaryQ, args...).Scan(
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
		// Per-provider phase split.
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
	byModelRows, err := db.pool.Query(ctx, byModelQ, args...)
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
				// Phase split.
				"avgUsOverheadMs":    df(avgUs),
				"avgUpstreamTotalMs": df(avgUp),
			})
		}
	}
	byModelRows.Close()

	// By project (identity snapshot)
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
	byProjectRows, err := db.pool.Query(ctx, byProjectQ, args...)
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

	// By virtual key (identity snapshot uses credential.id)
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
		-- identity.vk is the Virtual Key; identity.apiCredential is the
		-- upstream provider's API key (do not confuse the two).
		INNER JOIN "VirtualKey" vk ON vk.id = (a.identity->'vk'->>'id')
		WHERE a.source = 'ai-gateway' AND a.identity->'vk'->>'id' IS NOT NULL %s
		GROUP BY vk.id, vk.name, vk."keyPrefix"
		ORDER BY COUNT(*) DESC
	`, timeClause)
	byVKRows, err := db.pool.Query(ctx, byVKQ, args...)
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
	dailyRows, err := db.pool.Query(ctx, dailyQ, args...)
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
	statusRows, err := db.pool.Query(ctx, statusQ, args...)
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

// ComplianceAuditRow holds a unified compliance traffic event across all three enforcement layers.
type ComplianceAuditRow struct {
	ID             string   `json:"id"`
	Source         string   `json:"source"`
	TransactionID  string   `json:"transactionId"`
	SourceIP       string   `json:"sourceIp"`
	TargetHost     string   `json:"targetHost"`
	Method         *string  `json:"method"`
	Path           *string  `json:"path"`
	StatusCode     *int     `json:"statusCode"`
	HookDecision   *string  `json:"requestHookDecision"`
	HookReasonCode *string  `json:"requestHookReasonCode"`
	BumpStatus     *string  `json:"bumpStatus"`
	LatencyMs      *int     `json:"latencyMs"`
	Timestamp      any      `json:"timestamp"`
	ComplianceTags []string `json:"complianceTags"`
}

// ComplianceAuditParams holds filter parameters for ListComplianceAuditEvents.
type ComplianceAuditParams struct {
	Source         string   // ai-gateway | compliance-proxy | agent | "" (all)
	HookDecision   string   // APPROVE | MODIFY | BLOCK_SOFT | REJECT_HARD | ""
	ComplianceTags []string // tag overlap filter, empty = no filter
	SourceIP       string   // substring match, empty = no filter
	TargetHost     string   // substring match, empty = no filter
	Start          *time.Time
	End            *time.Time
	Limit          int
	Offset         int
}

// ListComplianceAuditEvents returns paginated traffic events across all three
// compliance enforcement layers (ai-gateway, compliance-proxy, agent).
// Kill-switch operational signals are excluded from results.
func (db *DB) ListComplianceAuditEvents(ctx context.Context, p ComplianceAuditParams) ([]ComplianceAuditRow, int, error) {
	args := []any{}
	n := 1

	where := `WHERE NOT (
		COALESCE(request_hook_decision, '') = 'SYSTEM'
		AND COALESCE(bump_status, '') = 'SYSTEM_EVENT'
		AND COALESCE(target_host, '') = 'killswitch'
	)`

	if p.Source != "" {
		where += fmt.Sprintf(" AND source = $%d", n)
		args = append(args, p.Source)
		n++
	}
	if p.HookDecision != "" {
		where += fmt.Sprintf(" AND request_hook_decision = $%d", n)
		args = append(args, p.HookDecision)
		n++
	}
	if len(p.ComplianceTags) > 0 {
		where += fmt.Sprintf(" AND compliance_tags && $%d", n)
		args = append(args, p.ComplianceTags)
		n++
	}
	if p.SourceIP != "" {
		where += fmt.Sprintf(" AND source_ip ILIKE $%d", n)
		args = append(args, "%"+p.SourceIP+"%")
		n++
	}
	if p.TargetHost != "" {
		where += fmt.Sprintf(" AND target_host ILIKE $%d", n)
		args = append(args, "%"+p.TargetHost+"%")
		n++
	}
	if p.Start != nil {
		where += fmt.Sprintf(" AND timestamp >= $%d", n)
		args = append(args, *p.Start)
		n++
	}
	if p.End != nil {
		where += fmt.Sprintf(" AND timestamp <= $%d", n)
		args = append(args, *p.End)
		n++
	}

	query := fmt.Sprintf(`
		SELECT id, source, COALESCE(details->>'transactionId', id),
			COALESCE(source_ip, ''), COALESCE(target_host, ''), method, path,
			status_code, request_hook_decision, request_hook_reason_code,
			bump_status, latency_ms, timestamp, compliance_tags
		FROM traffic_event
		%s
		ORDER BY timestamp DESC
		LIMIT $%d OFFSET $%d
	`, where, n, n+1)
	queryArgs := append(args, p.Limit, p.Offset) //nolint:gocritic // appendAssign: intentional new slice
	rows, err := db.pool.Query(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	result := []ComplianceAuditRow{}
	for rows.Next() {
		var r ComplianceAuditRow
		if err := rows.Scan(&r.ID, &r.Source, &r.TransactionID, &r.SourceIP, &r.TargetHost,
			&r.Method, &r.Path, &r.StatusCode, &r.HookDecision, &r.HookReasonCode,
			&r.BumpStatus, &r.LatencyMs, &r.Timestamp, &r.ComplianceTags); err != nil {
			return nil, 0, err
		}
		result = append(result, r)
	}

	var total int
	if err := db.pool.QueryRow(ctx, "SELECT COUNT(*) FROM traffic_event "+where, args...).Scan(&total); err != nil {
		return result, 0, err
	}
	return result, total, rows.Err()
}

// TrinityLayerStats holds compliance stats for one enforcement layer.
type TrinityLayerStats struct {
	TotalEvents   int            `json:"totalEvents"`
	Decisions     map[string]int `json:"decisions"`                 // APPROVE / MODIFY / BLOCK_SOFT / REJECT_HARD / ABSTAIN
	BlockCount    int            `json:"blockCount"`                // REJECT_HARD + BLOCK_SOFT
	BlockRate     float64        `json:"blockRate"`                 // blockCount / totalEvents (0 if no events)
	BumpBreakdown map[string]int `json:"bumpBreakdown,omitempty"`   // only for compliance-proxy and agent
	CoveragePct   *float64       `json:"coveragePercent,omitempty"` // % BUMP_SUCCESS of all bump-eligible events
}

// TrinityStats holds per-layer compliance stats for the overview dashboard.
type TrinityStats struct {
	Period          TimePeriod        `json:"period"`
	AIGateway       TrinityLayerStats `json:"aiGateway"`
	ComplianceProxy TrinityLayerStats `json:"complianceProxy"`
	Agent           TrinityLayerStats `json:"agent"`
}

// GetTrinityStats returns per-layer compliance stats for all three enforcement
// points (ai-gateway, compliance-proxy, agent) for the given time range.
func (db *DB) GetTrinityStats(ctx context.Context, start, end time.Time) (*TrinityStats, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT
			source,
			COUNT(*) AS total,
			COUNT(*) FILTER (WHERE request_hook_decision = 'APPROVE') AS approve,
			COUNT(*) FILTER (WHERE request_hook_decision = 'MODIFY') AS modify,
			COUNT(*) FILTER (WHERE request_hook_decision = 'BLOCK_SOFT') AS reject_soft,
			COUNT(*) FILTER (WHERE request_hook_decision = 'REJECT_HARD') AS reject_hard,
			COUNT(*) FILTER (WHERE request_hook_decision = 'ABSTAIN') AS abstain,
			COUNT(*) FILTER (WHERE bump_status = 'BUMP_SUCCESS') AS bump_success,
			COUNT(*) FILTER (WHERE bump_status = 'BUMP_FAILED_PASSTHROUGH') AS bump_failed,
			COUNT(*) FILTER (WHERE bump_status = 'BUMP_EXEMPT') AS bump_exempt,
			COUNT(*) FILTER (WHERE bump_status = 'BUMP_DISABLED') AS bump_disabled
		FROM traffic_event
		WHERE source IN ('ai-gateway', 'compliance-proxy', 'agent')
			AND timestamp >= $1 AND timestamp <= $2
			AND NOT (
				COALESCE(request_hook_decision, '') = 'SYSTEM'
				AND COALESCE(bump_status, '') = 'SYSTEM_EVENT'
				AND COALESCE(target_host, '') = 'killswitch'
			)
		GROUP BY source
	`, start, end)
	if err != nil {
		return nil, fmt.Errorf("trinity stats: %w", err)
	}
	defer rows.Close()

	stats := &TrinityStats{Period: TimePeriod{Start: start, End: end}}
	buildLayer := func(total, approve, modify, rejectSoft, rejectHard, abstain,
		bumpSuccess, bumpFailed, bumpExempt, bumpDisabled int) TrinityLayerStats {
		decisions := map[string]int{
			"APPROVE":     approve,
			"MODIFY":      modify,
			"BLOCK_SOFT":  rejectSoft,
			"REJECT_HARD": rejectHard,
			"ABSTAIN":     abstain,
		}
		blockCount := rejectSoft + rejectHard
		blockRate := 0.0
		if total > 0 {
			blockRate = float64(blockCount) / float64(total)
		}
		return TrinityLayerStats{
			TotalEvents: total,
			Decisions:   decisions,
			BlockCount:  blockCount,
			BlockRate:   blockRate,
		}
	}
	addBump := func(layer *TrinityLayerStats, total, bumpSuccess, bumpFailed, bumpExempt, bumpDisabled int) {
		layer.BumpBreakdown = map[string]int{
			"BUMP_SUCCESS":            bumpSuccess,
			"BUMP_FAILED_PASSTHROUGH": bumpFailed,
			"BUMP_EXEMPT":             bumpExempt,
			"BUMP_DISABLED":           bumpDisabled,
		}
		bumpEligible := bumpSuccess + bumpFailed + bumpExempt + bumpDisabled
		if bumpEligible > 0 {
			pct := float64(bumpSuccess) / float64(bumpEligible) * 100
			layer.CoveragePct = &pct
		}
	}

	for rows.Next() {
		var src string
		var total, approve, modify_, rejectSoft, rejectHard, abstain int
		var bumpSuccess, bumpFailed, bumpExempt, bumpDisabled int
		if err := rows.Scan(&src, &total, &approve, &modify_, &rejectSoft, &rejectHard, &abstain,
			&bumpSuccess, &bumpFailed, &bumpExempt, &bumpDisabled); err != nil {
			return nil, fmt.Errorf("trinity stats scan: %w", err)
		}
		layer := buildLayer(total, approve, modify_, rejectSoft, rejectHard, abstain,
			bumpSuccess, bumpFailed, bumpExempt, bumpDisabled)
		switch src {
		case "ai-gateway":
			stats.AIGateway = layer
		case "compliance-proxy":
			addBump(&layer, total, bumpSuccess, bumpFailed, bumpExempt, bumpDisabled)
			stats.ComplianceProxy = layer
		case "agent":
			addBump(&layer, total, bumpSuccess, bumpFailed, bumpExempt, bumpDisabled)
			stats.Agent = layer
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trinity stats rows: %w", err)
	}
	return stats, nil
}

// GetMatrixAuditEvent returns a single proxy/agent traffic event by ID,
// including optional request/response body from the payload child table.
func (db *DB) GetMatrixAuditEvent(ctx context.Context, id string) (map[string]any, error) {
	var eid string
	var txID, connID, trafficSrc, ingressType, bumpStatus *string
	var srcIP, target *string
	var method, path_, hookDec, hookReason, hookRC, subjectID *string
	var complianceTags []string
	var statusCode, latency *int
	var ts any
	var details json.RawMessage
	var requestBody, responseBody *json.RawMessage

	err := db.pool.QueryRow(ctx, `
		SELECT e.id, e.details->>'transactionId', e.details->>'connectionId',
			e.details->>'trafficSource', e.details->>'ingressType', e.bump_status,
			e.source_ip, e.target_host, e.method, e.path, e.status_code, e.request_hook_decision, e.request_hook_reason,
			e.request_hook_reason_code, e.latency_ms, e.timestamp, e.compliance_tags, e.entity_id,
			e.details->>'userAgent', e.details,
			p.inline_request_body, p.inline_response_body
		FROM traffic_event e
		LEFT JOIN traffic_event_payload p ON p.traffic_event_id = e.id
		WHERE e.id = $1
	`, id).Scan(&eid, &txID, &connID, &trafficSrc, &ingressType, &bumpStatus,
		&srcIP, &target, &method, &path_, &statusCode, &hookDec, &hookReason,
		&hookRC, &latency, &ts, &complianceTags, &subjectID, new(*string), &details,
		&requestBody, &responseBody)
	if err != nil {
		return nil, err
	}

	result := map[string]any{
		"id": eid, "transactionId": txID, "connectionId": connID,
		"trafficSource": trafficSrc, "ingressType": ingressType, "bumpStatus": bumpStatus,
		"sourceIp": srcIP, "targetHost": target, "method": method, "path": path_,
		"statusCode": statusCode, "hookDecision": hookDec, "hookReason": hookReason,
		"hookReasonCode": hookRC, "latencyMs": latency, "timestamp": ts,
		"complianceTags": complianceTags, "entityId": subjectID,
	}
	if requestBody != nil {
		result["requestBody"] = *requestBody
	}
	if responseBody != nil {
		result["responseBody"] = *responseBody
	}
	return result, nil
}
