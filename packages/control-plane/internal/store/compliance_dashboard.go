package store

import (
	"context"
	"fmt"
	"time"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/domain"
)

// ComplianceDashboardKPIs holds the four headline numbers for the global
// compliance overview page. All values are derived from the Trinity layer stats
// so no extra queries are needed for these fields.
type ComplianceDashboardKPIs struct {
	TotalRequests      int     `json:"totalRequests"`
	TotalBlocked       int     `json:"totalBlocked"`
	OverallBlockRate   float64 `json:"overallBlockRate"`
	TLSCoveragePercent float64 `json:"tlsCoveragePercent"`
	HookErrorRate      float64 `json:"hookErrorRate"`
}

// ComplianceDashboardHookHealth holds global hook decision health for all three
// enforcement layers combined.
type ComplianceDashboardHookHealth struct {
	Total          int                   `json:"total"`
	ByDecision     HookDecisionBreakdown `json:"byDecision"`
	TopReasonCodes []LabelCount          `json:"topReasonCodes"`
	LatencyP50     *float64              `json:"latencyP50"`
	LatencyP95     *float64              `json:"latencyP95"`
	LatencyP99     *float64              `json:"latencyP99"`
}

// ComplianceDashboardTopBlocked holds the top-10 blocked entries grouped by
// target host, reason code, and source IP — all three enforcement layers.
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

// GetComplianceDashboard returns all data needed for the global compliance
// overview page in a single call. Strategy:
//   - Trinity per-layer decision breakdown: direct traffic_event GROUP BY source
//     (required — per-source APPROVE/MODIFY/REJECT breakdown is not in rollup)
//   - TLS bump coverage: rollup cascade (proxy + agent SubDimensions)
//   - Hook health + latency: rollup cascade (global, no SubDimension filter)
//   - Top-N blocked lists: direct traffic_event (not stored in rollup dimensions)
//
// KPIs are derived from the Trinity totals — no extra rollup queries needed.
func (db *DB) GetComplianceDashboard(ctx context.Context, start, end time.Time) (*ComplianceDashboardData, error) {
	// 1. Trinity (direct query).
	trinity, err := db.GetTrinityStats(ctx, start, end)
	if err != nil {
		return nil, fmt.Errorf("compliance dashboard trinity: %w", err)
	}

	// 2. TLS coverage from rollup (proxy + agent).
	bumpSuccess, bumpFailed, bumpExempt, bumpDisabled := 0, 0, 0, 0
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
		rows, err := db.QueryRollupCascade(ctx, q)
		if err != nil || len(rows) == 0 {
			continue
		}
		result := metrics.BuildResult(q, rows, metrics.SelectGranularity(start, end))
		s := result.Summary
		bumpSuccess += int(s[metrics.MetricBumpSuccessCount])
		bumpFailed += int(s[metrics.MetricBumpFailedCount])
		bumpExempt += int(s[metrics.MetricBumpExemptCount])
		bumpDisabled += int(s[metrics.MetricBumpDisabledCount])
	}
	bumpEligible := bumpSuccess + bumpFailed + bumpExempt + bumpDisabled
	tlsCoverage := 0.0
	if bumpEligible > 0 {
		tlsCoverage = float64(bumpSuccess) / float64(bumpEligible) * 100
	}

	// 3. Hook health from rollup (global, no SubDimension filter).
	hookHealth := ComplianceDashboardHookHealth{
		TopReasonCodes: []LabelCount{},
	}
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
	hRows, err := db.QueryRollupCascade(ctx, hq)
	if err == nil && len(hRows) > 0 {
		result := metrics.BuildResult(hq, hRows, metrics.SelectGranularity(start, end))
		s := result.Summary
		allow := int(s[metrics.MetricHookAllowCount])
		deny := int(s[metrics.MetricHookDenyCount])
		hookErr := int(s[metrics.MetricHookErrorCount])
		unknown := int(s[metrics.MetricHookUnknownCount])
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
		// Fallback: direct traffic_event scan for hook counts + latency.
		row := db.pool.QueryRow(ctx, `
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
			&hookHealth.LatencyP50, &hookHealth.LatencyP95, &hookHealth.LatencyP99); err == nil {
			hookHealth.Total = total
			hookHealth.ByDecision = HookDecisionBreakdown{Allow: allow, Deny: deny, Error: unknown}
		}
	}

	// Top-10 reason codes (always direct — not in rollup dimensions).
	reasonRows, err := db.pool.Query(ctx, `
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

	// 4. Top blocked — direct queries across all 3 sources.
	topBlocked := ComplianceDashboardTopBlocked{
		ByTarget:     []LabelCount{},
		ByReasonCode: []LabelCount{},
		BySourceIP:   []LabelCount{},
	}

	topBlockedByTarget, _ := db.queryTopBlocked(ctx, "target_host", start, end)
	if topBlockedByTarget != nil {
		topBlocked.ByTarget = topBlockedByTarget
	}

	topBlockedByReason, _ := db.queryTopBlocked(ctx, "request_hook_reason_code", start, end)
	if topBlockedByReason != nil {
		topBlocked.ByReasonCode = topBlockedByReason
	}

	topBlockedByIP, _ := db.queryTopBlocked(ctx, "source_ip", start, end)
	if topBlockedByIP != nil {
		topBlocked.BySourceIP = topBlockedByIP
	}

	// 5. Derive KPIs from Trinity totals.
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

// queryTopBlocked returns the top-10 values for a given traffic_event column
// among blocked (REJECT_HARD / BLOCK_SOFT) events across all three enforcement
// layers in the specified time window.
func (db *DB) queryTopBlocked(ctx context.Context, col string, start, end time.Time) ([]LabelCount, error) {
	q := fmt.Sprintf(`
		SELECT COALESCE(%s, 'unknown'), COUNT(*) AS cnt
		FROM traffic_event
		WHERE request_hook_decision IN ('REJECT_HARD', 'BLOCK_SOFT')
			AND source IN ('ai-gateway', 'compliance-proxy', 'agent')
			AND timestamp >= $1 AND timestamp <= $2
		GROUP BY %s
		ORDER BY cnt DESC
		LIMIT 10
	`, col, col)
	rows, err := db.pool.Query(ctx, q, start, end)
	if err != nil {
		return nil, fmt.Errorf("top blocked by %s: %w", col, err)
	}
	defer rows.Close()

	result := []LabelCount{}
	for rows.Next() {
		var lc LabelCount
		if rows.Scan(&lc.Label, &lc.Count) == nil {
			result = append(result, lc)
		}
	}
	return result, rows.Err()
}
