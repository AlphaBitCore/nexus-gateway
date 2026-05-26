package compliancestore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/domain"
)

// TimePeriod holds a closed [Start, End] time interval.
type TimePeriod struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// LabelCount is a generic label+count pair for top-N lists.
type LabelCount struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// HookDecisionBreakdown holds per-decision counts.
type HookDecisionBreakdown struct {
	Allow   int `json:"allow"`
	Deny    int `json:"deny"`
	Error   int `json:"error"`
	Unknown int `json:"unknown"`
}

// ComplianceCoverageStats holds coverage percentage and per-bump_status breakdown.
type ComplianceCoverageStats struct {
	CoveragePct float64        `json:"coveragePercent"`
	Breakdown   map[string]int `json:"breakdown"`
	Period      TimePeriod     `json:"period"`
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
	Source         string     // ai-gateway | compliance-proxy | agent | "" (all)
	HookDecision   string     // APPROVE | MODIFY | BLOCK_SOFT | REJECT_HARD | ""
	ComplianceTags []string   // tag overlap filter, empty = no filter
	SourceIP       string     // substring match, empty = no filter
	TargetHost     string     // substring match, empty = no filter
	Start          *time.Time
	End            *time.Time
	Limit          int
	Offset         int
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

// ComplianceDashboardKPIs holds the four headline numbers for the global
// compliance overview page.
type ComplianceDashboardKPIs struct {
	TotalRequests      int     `json:"totalRequests"`
	TotalBlocked       int     `json:"totalBlocked"`
	OverallBlockRate   float64 `json:"overallBlockRate"`
	TLSCoveragePercent float64 `json:"tlsCoveragePercent"`
	HookErrorRate      float64 `json:"hookErrorRate"`
}

// ComplianceDashboardHookHealth holds global hook decision health.
type ComplianceDashboardHookHealth struct {
	Total          int                   `json:"total"`
	ByDecision     HookDecisionBreakdown `json:"byDecision"`
	TopReasonCodes []LabelCount          `json:"topReasonCodes"`
	LatencyP50     *float64              `json:"latencyP50"`
	LatencyP95     *float64              `json:"latencyP95"`
	LatencyP99     *float64              `json:"latencyP99"`
}

// ComplianceDashboardTopBlocked holds the top-10 blocked entries.
type ComplianceDashboardTopBlocked struct {
	ByTarget     []LabelCount `json:"byTarget"`
	ByReasonCode []LabelCount `json:"byReasonCode"`
	BySourceIP   []LabelCount `json:"bySourceIp"`
}

// ComplianceDashboardData is the single response shape for GET /compliance/overview.
type ComplianceDashboardData struct {
	Period     TimePeriod                    `json:"period"`
	KPIs       ComplianceDashboardKPIs       `json:"kpis"`
	Trinity    TrinityStats                  `json:"trinity"`
	HookHealth ComplianceDashboardHookHealth `json:"hookHealth"`
	TopBlocked ComplianceDashboardTopBlocked `json:"topBlocked"`
}

// ListMatrixAuditEvents returns paginated compliance-proxy/agent traffic events.
// Kill-switch operational signals are excluded from results.
func (s *Store) ListMatrixAuditEvents(ctx context.Context, start, end *time.Time, limit, offset int) ([]MatrixAuditRow, int, error) {
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
	queryArgs := append(args, limit, offset) //nolint:gocritic
	rows, err := s.pool.Query(ctx, query, queryArgs...)
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
	if err := s.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return result, 0, err
	}
	return result, total, rows.Err()
}

// GetMatrixAuditEvent returns a single proxy/agent traffic event by ID,
// including optional request/response body from the payload child table.
func (s *Store) GetMatrixAuditEvent(ctx context.Context, id string) (map[string]any, error) {
	var eid string
	var txID, connID, trafficSrc, ingressType, bumpStatus *string
	var srcIP, target *string
	var method, path_, hookDec, hookReason, hookRC, subjectID *string
	var complianceTags []string
	var statusCode, latency *int
	var ts any
	var details json.RawMessage
	var requestBody, responseBody *json.RawMessage

	err := s.pool.QueryRow(ctx, `
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

// ListComplianceAuditEvents returns paginated traffic events across all three
// compliance enforcement layers. Kill-switch signals are excluded.
func (s *Store) ListComplianceAuditEvents(ctx context.Context, p ComplianceAuditParams) ([]ComplianceAuditRow, int, error) {
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
	queryArgs := append(args, p.Limit, p.Offset) //nolint:gocritic
	rows, err := s.pool.Query(ctx, query, queryArgs...)
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
	if err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM traffic_event "+where, args...).Scan(&total); err != nil {
		return result, 0, err
	}
	return result, total, rows.Err()
}

// GetTrinityStats returns per-layer compliance stats for all three enforcement points.
func (s *Store) GetTrinityStats(ctx context.Context, start, end time.Time) (*TrinityStats, error) {
	rows, err := s.pool.Query(ctx, `
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

// GetComplianceCoverage returns compliance coverage stats for a time range.
// Reads from rollup tables first (O(buckets)); falls back to direct traffic_event scan.
func (s *Store) GetComplianceCoverage(ctx context.Context, start, end time.Time) (*ComplianceCoverageStats, error) {
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
	if s.metrics != nil {
		for _, q := range complianceMetrics {
			rollupRows, err := s.metrics.QueryRollupCascade(ctx, q)
			if err != nil || len(rollupRows) == 0 {
				continue
			}
			rollupOK = true
			result := metrics.BuildResult(q, rollupRows, metrics.SelectGranularity(start, end))
			sm := result.Summary
			totalSuccess += int(sm[metrics.MetricBumpSuccessCount])
			totalFailed += int(sm[metrics.MetricBumpFailedCount])
			totalExempt += int(sm[metrics.MetricBumpExemptCount])
			totalDisabled += int(sm[metrics.MetricBumpDisabledCount])
			totalRequests += int(sm[metrics.MetricRequestCount])
		}
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
	dbRows, err := s.pool.Query(ctx, `
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

// GetHookHealth returns hook decision stats for a time range.
func (s *Store) GetHookHealth(ctx context.Context, start, end time.Time) (*HookHealthStats, error) {
	hs := HookHealthStats{
		Period:         TimePeriod{Start: start, End: end},
		TopReasonCodes: []LabelCount{},
	}

	rollupUsed := false
	if s.metrics != nil {
		hookMetrics := []string{
			metrics.MetricHookAllowCount,
			metrics.MetricHookDenyCount,
			metrics.MetricHookErrorCount,
			metrics.MetricHookUnknownCount,
			metrics.MetricLatencyHistogram,
		}
		q := metrics.MetricsQuery{Metrics: hookMetrics, StartTime: start, EndTime: end}
		rollupRows, err := s.metrics.QueryRollupCascade(ctx, q)
		if err == nil && len(rollupRows) > 0 {
			rollupUsed = true
			result := metrics.BuildResult(q, rollupRows, metrics.SelectGranularity(start, end))
			sm := result.Summary
			hs.ByDecision.Allow = int(sm[metrics.MetricHookAllowCount])
			hs.ByDecision.Deny = int(sm[metrics.MetricHookDenyCount])
			hs.ByDecision.Error = int(sm[metrics.MetricHookErrorCount])
			hs.ByDecision.Unknown = int(sm[metrics.MetricHookUnknownCount])
			hs.Total = hs.ByDecision.Allow + hs.ByDecision.Deny + hs.ByDecision.Error + hs.ByDecision.Unknown

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
							hs.LatencyP50 = &p50
							hs.LatencyP95 = &p95
							hs.LatencyP99 = &p99
						}
					}
				}
			}
		}
	}

	if !rollupUsed {
		// Fallback: direct traffic_event scan for decision counts.
		if err := s.pool.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE request_hook_decision IS NOT NULL),
				COUNT(*) FILTER (WHERE request_hook_decision = 'APPROVE'),
				COUNT(*) FILTER (WHERE request_hook_decision IN ('REJECT_HARD', 'BLOCK_SOFT')),
				COUNT(*) FILTER (WHERE request_hook_decision NOT IN ('APPROVE', 'REJECT_HARD', 'BLOCK_SOFT', 'MODIFY', 'ABSTAIN') AND request_hook_decision IS NOT NULL),
				COUNT(*) FILTER (WHERE request_hook_decision IS NOT NULL
					AND request_hook_decision NOT IN ('APPROVE', 'REJECT_HARD', 'BLOCK_SOFT'))
			FROM traffic_event WHERE timestamp >= $1 AND timestamp <= $2
		`, start, end).Scan(&hs.Total, &hs.ByDecision.Allow, &hs.ByDecision.Deny, &hs.ByDecision.Error, &hs.ByDecision.Unknown); err != nil {
			return nil, fmt.Errorf("hook health decisions: %w", err)
		}

		// Fallback: direct latency percentiles.
		if err := s.pool.QueryRow(ctx, `
			SELECT
				percentile_cont(0.50) WITHIN GROUP (ORDER BY latency_ms),
				percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms),
				percentile_cont(0.99) WITHIN GROUP (ORDER BY latency_ms)
			FROM traffic_event
			WHERE request_hook_decision IS NOT NULL AND latency_ms IS NOT NULL
				AND timestamp >= $1 AND timestamp <= $2
		`, start, end).Scan(&hs.LatencyP50, &hs.LatencyP95, &hs.LatencyP99); err != nil {
			return nil, fmt.Errorf("hook health latency: %w", err)
		}
	}

	// Top-10 reason codes: always direct (not stored in rollup).
	reasonRows, err := s.pool.Query(ctx, `
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
		hs.TopReasonCodes = append(hs.TopReasonCodes, lc)
	}
	if err := reasonRows.Err(); err != nil {
		return nil, fmt.Errorf("hook health reason codes rows: %w", err)
	}
	return &hs, nil
}

// GetComplianceDashboard returns all data needed for the global compliance overview page.
func (s *Store) GetComplianceDashboard(ctx context.Context, start, end time.Time) (*ComplianceDashboardData, error) {
	// 1. Trinity (direct query)
	trinity, err := s.GetTrinityStats(ctx, start, end)
	if err != nil {
		return nil, fmt.Errorf("compliance dashboard trinity: %w", err)
	}

	// 2. TLS coverage from rollup (proxy + agent)
	bumpSuccess, bumpFailed, bumpExempt, bumpDisabled := 0, 0, 0, 0
	if s.metrics != nil {
		for _, subDim := range []string{
			"source=" + string(domain.DomainProxy),
			"source=" + string(domain.DomainAgent),
		} {
			q := metrics.MetricsQuery{
				Metrics: []string{
					metrics.MetricBumpSuccessCount,
					metrics.MetricBumpFailedCount,
					metrics.MetricBumpExemptCount,
					metrics.MetricBumpDisabledCount,
				},
				SubDimension: subDim,
				StartTime:    start,
				EndTime:      end,
			}
			rollupRows, err := s.metrics.QueryRollupCascade(ctx, q)
			if err != nil || len(rollupRows) == 0 {
				continue
			}
			result := metrics.BuildResult(q, rollupRows, metrics.SelectGranularity(start, end))
			sm := result.Summary
			bumpSuccess += int(sm[metrics.MetricBumpSuccessCount])
			bumpFailed += int(sm[metrics.MetricBumpFailedCount])
			bumpExempt += int(sm[metrics.MetricBumpExemptCount])
			bumpDisabled += int(sm[metrics.MetricBumpDisabledCount])
		}
	}
	bumpEligible := bumpSuccess + bumpFailed + bumpExempt + bumpDisabled
	tlsCoverage := 0.0
	if bumpEligible > 0 {
		tlsCoverage = float64(bumpSuccess) / float64(bumpEligible) * 100
	}

	// 3. Hook health from rollup (global)
	hookHealth := ComplianceDashboardHookHealth{
		TopReasonCodes: []LabelCount{},
	}
	if s.metrics != nil {
		hq := metrics.MetricsQuery{
			Metrics: []string{
				metrics.MetricHookAllowCount,
				metrics.MetricHookDenyCount,
				metrics.MetricHookErrorCount,
				metrics.MetricHookUnknownCount,
				metrics.MetricHookLatencyHist,
			},
			StartTime: start,
			EndTime:   end,
		}
		hRows, err := s.metrics.QueryRollupCascade(ctx, hq)
		if err == nil && len(hRows) > 0 {
			result := metrics.BuildResult(hq, hRows, metrics.SelectGranularity(start, end))
			sm := result.Summary
			allow := int(sm[metrics.MetricHookAllowCount])
			deny := int(sm[metrics.MetricHookDenyCount])
			hookErr := int(sm[metrics.MetricHookErrorCount])
			unknown := int(sm[metrics.MetricHookUnknownCount])
			hookHealth.Total = allow + deny + hookErr + unknown
			hookHealth.ByDecision = HookDecisionBreakdown{Allow: allow, Deny: deny, Error: hookErr, Unknown: unknown}
			if result.Metadata != nil {
				if histRaw, ok := result.Metadata[metrics.MetricHookLatencyHist]; ok {
					if hist, ok2 := histRaw.(metrics.Histogram); ok2 {
						p50 := hist.Percentile(0.50)
						p95 := hist.Percentile(0.95)
						p99 := hist.Percentile(0.99)
						hookHealth.LatencyP50 = &p50
						hookHealth.LatencyP95 = &p95
						hookHealth.LatencyP99 = &p99
					}
				}
			}
		} else {
			s.fallbackHookHealth(ctx, start, end, &hookHealth)
		}
	} else {
		s.fallbackHookHealth(ctx, start, end, &hookHealth)
	}

	// Top-10 reason codes (always direct)
	reasonRows, err := s.pool.Query(ctx, `
		SELECT COALESCE(request_hook_reason_code, 'unknown'), COUNT(*) AS cnt
		FROM traffic_event
		WHERE request_hook_decision IN ('REJECT_HARD', 'BLOCK_SOFT')
			AND request_hook_reason_code IS NOT NULL
			AND timestamp >= $1 AND timestamp <= $2
		GROUP BY request_hook_reason_code
		ORDER BY cnt DESC
		LIMIT 10
	`, start, end)
	if err == nil {
		defer reasonRows.Close()
		for reasonRows.Next() {
			var lc LabelCount
			if reasonRows.Scan(&lc.Label, &lc.Count) == nil {
				hookHealth.TopReasonCodes = append(hookHealth.TopReasonCodes, lc)
			}
		}
	}

	// 4. Top blocked
	topBlocked := ComplianceDashboardTopBlocked{
		ByTarget:     []LabelCount{},
		ByReasonCode: []LabelCount{},
		BySourceIP:   []LabelCount{},
	}
	if v, _ := s.queryTopBlocked(ctx, "target_host", start, end); v != nil {
		topBlocked.ByTarget = v
	}
	if v, _ := s.queryTopBlocked(ctx, "request_hook_reason_code", start, end); v != nil {
		topBlocked.ByReasonCode = v
	}
	if v, _ := s.queryTopBlocked(ctx, "source_ip", start, end); v != nil {
		topBlocked.BySourceIP = v
	}

	// 5. KPIs from Trinity totals
	totalReqs := trinity.AIGateway.TotalEvents + trinity.ComplianceProxy.TotalEvents + trinity.Agent.TotalEvents
	totalBlocked := trinity.AIGateway.BlockCount + trinity.ComplianceProxy.BlockCount + trinity.Agent.BlockCount
	overallBlockRate := 0.0
	if totalReqs > 0 {
		overallBlockRate = float64(totalBlocked) / float64(totalReqs)
	}
	hookErrRate := 0.0
	if hookHealth.Total > 0 {
		hookErrRate = float64(hookHealth.ByDecision.Error) / float64(hookHealth.Total)
	}

	return &ComplianceDashboardData{
		Period: TimePeriod{Start: start, End: end},
		KPIs: ComplianceDashboardKPIs{
			TotalRequests:      totalReqs,
			TotalBlocked:       totalBlocked,
			OverallBlockRate:   overallBlockRate,
			TLSCoveragePercent: tlsCoverage,
			HookErrorRate:      hookErrRate,
		},
		Trinity:    *trinity,
		HookHealth: hookHealth,
		TopBlocked: topBlocked,
	}, nil
}

func (s *Store) fallbackHookHealth(ctx context.Context, start, end time.Time, h *ComplianceDashboardHookHealth) {
	row := s.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE request_hook_decision IS NOT NULL),
			COUNT(*) FILTER (WHERE request_hook_decision = 'APPROVE'),
			COUNT(*) FILTER (WHERE request_hook_decision IN ('REJECT_HARD', 'BLOCK_SOFT')),
			COUNT(*) FILTER (WHERE request_hook_decision = 'MODIFY'),
			COUNT(*) FILTER (WHERE request_hook_decision = 'ABSTAIN'),
			COUNT(*) FILTER (WHERE request_hook_decision IS NOT NULL
				AND request_hook_decision NOT IN ('APPROVE', 'REJECT_HARD', 'BLOCK_SOFT', 'MODIFY', 'ABSTAIN')),
			percentile_cont(0.50) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE latency_ms IS NOT NULL AND request_hook_decision IS NOT NULL),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE latency_ms IS NOT NULL AND request_hook_decision IS NOT NULL),
			percentile_cont(0.99) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE latency_ms IS NOT NULL AND request_hook_decision IS NOT NULL)
		FROM traffic_event
		WHERE timestamp >= $1 AND timestamp <= $2
	`, start, end)
	var total, allow, deny, modify_, abstain, unknown int
	if err := row.Scan(&total, &allow, &deny, &modify_, &abstain, &unknown,
		&h.LatencyP50, &h.LatencyP95, &h.LatencyP99); err == nil {
		h.Total = total
		h.ByDecision = HookDecisionBreakdown{Allow: allow, Deny: deny, Error: unknown}
	}
}

func (s *Store) queryTopBlocked(ctx context.Context, col string, start, end time.Time) ([]LabelCount, error) {
	q := fmt.Sprintf(`
		SELECT COALESCE(%s, 'unknown'), COUNT(*) AS cnt
		FROM traffic_event
		WHERE request_hook_decision IN ('REJECT_HARD', 'BLOCK_SOFT')
			AND source IN ('ai-gateway', 'compliance-proxy', 'agent')
			AND timestamp >= $1 AND timestamp <= $2
		GROUP BY 1
		ORDER BY cnt DESC
		LIMIT 10
	`, col)
	rows, err := s.pool.Query(ctx, q, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []LabelCount
	for rows.Next() {
		var lc LabelCount
		if rows.Scan(&lc.Label, &lc.Count) == nil {
			result = append(result, lc)
		}
	}
	return result, rows.Err()
}
