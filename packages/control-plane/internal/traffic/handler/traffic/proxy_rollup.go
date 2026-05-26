package traffic

import (
	"context"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/compliancestore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/domain"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// tryRollupComplianceCoverage attempts to serve compliance coverage stats from
// rollup data. Returns nil if rollup has no data.
func (h *Handler) tryRollupComplianceCoverage(ctx context.Context, start, end time.Time) (*compliancestore.ComplianceCoverageStats, error) {
	q := metrics.MetricsQuery{
		Metrics: []string{
			metrics.MetricBumpSuccessCount,
			metrics.MetricBumpFailedCount,
			metrics.MetricBumpExemptCount,
			metrics.MetricBumpDisabledCount,
			metrics.MetricProxyRequestCount,
		},
		SubDimension: "source=" + string(domain.DomainProxy),
		StartTime:    start,
		EndTime:      end,
	}
	result, err := h.queryMetricsOrFallback(ctx, q)
	if err != nil || result == nil {
		return nil, err
	}

	success := int(result.Summary[metrics.MetricBumpSuccessCount])
	failed := int(result.Summary[metrics.MetricBumpFailedCount])
	exempt := int(result.Summary[metrics.MetricBumpExemptCount])
	disabled := int(result.Summary[metrics.MetricBumpDisabledCount])
	total := int(result.Summary[metrics.MetricProxyRequestCount])

	// If proxy_request_count is not populated, derive from components.
	if total == 0 {
		total = success + failed + exempt + disabled
	}

	pct := 0.0
	if total > 0 {
		pct = float64(success) / float64(total) * 100
	}

	breakdown := map[string]int{
		"BUMP_SUCCESS":            success,
		"BUMP_FAILED_PASSTHROUGH": failed,
		"BUMP_EXEMPT_CONFIGURED":  exempt,
		"BUMP_DISABLED":           disabled,
	}

	return &compliancestore.ComplianceCoverageStats{
		CoveragePct: pct,
		Breakdown:   breakdown,
		Period:      compliancestore.TimePeriod{Start: start, End: end},
	}, nil
}

// tryRollupHookHealth attempts to serve hook health stats from rollup data.
// Returns nil if rollup has no data. Note: topReasonCodes are not available
// from rollup (reason_code is not a rollup dimension) and are left empty.
func (h *Handler) tryRollupHookHealth(ctx context.Context, start, end time.Time) (*compliancestore.HookHealthStats, error) {
	q := metrics.MetricsQuery{
		Metrics: []string{
			metrics.MetricHookAllowCount,
			metrics.MetricHookDenyCount,
			metrics.MetricHookErrorCount,
			metrics.MetricHookUnknownCount,
			metrics.MetricHookLatencyHist,
		},
		SubDimension: "source=" + string(domain.DomainProxy),
		StartTime:    start,
		EndTime:      end,
	}
	result, err := h.queryMetricsOrFallback(ctx, q)
	if err != nil || result == nil {
		return nil, err
	}

	allow := int(result.Summary[metrics.MetricHookAllowCount])
	deny := int(result.Summary[metrics.MetricHookDenyCount])
	hookErr := int(result.Summary[metrics.MetricHookErrorCount])
	unknown := int(result.Summary[metrics.MetricHookUnknownCount])
	total := allow + deny + hookErr + unknown

	s := &compliancestore.HookHealthStats{
		Total: total,
		ByDecision: compliancestore.HookDecisionBreakdown{
			Allow:   allow,
			Deny:    deny,
			Error:   hookErr,
			Unknown: unknown,
		},
		TopReasonCodes: []compliancestore.LabelCount{},
		Period:         compliancestore.TimePeriod{Start: start, End: end},
	}

	// Extract latency percentiles from histogram metadata.
	if result.Metadata != nil {
		if histRaw, ok := result.Metadata[metrics.MetricHookLatencyHist]; ok {
			if hist, ok := histRaw.(metrics.Histogram); ok {
				p50 := hist.Percentile(0.50)
				p95 := hist.Percentile(0.95)
				p99 := hist.Percentile(0.99)
				s.LatencyP50 = &p50
				s.LatencyP95 = &p95
				s.LatencyP99 = &p99
			}
		}
	}

	return s, nil
}

// tryRollupRejectStats attempts to serve reject stats from rollup data.
// Returns nil if rollup has no data. Note: top source IPs and reason codes
// require dimensions not available in rollup and are returned as empty slices.
func (h *Handler) tryRollupRejectStats(ctx context.Context, start, end time.Time) (*compliancestore.RejectStats, error) {
	// Query total reject count (global).
	q := metrics.MetricsQuery{
		Metrics:      []string{metrics.MetricRejectCount},
		SubDimension: "source=" + string(domain.DomainProxy),
		StartTime:    start,
		EndTime:      end,
	}
	result, err := h.queryMetricsOrFallback(ctx, q)
	if err != nil || result == nil {
		return nil, err
	}

	totalRejects := int(result.Summary[metrics.MetricRejectCount])

	// Query top targets by reject count, grouped by target_host.
	tq := metrics.MetricsQuery{
		Metrics:      []string{metrics.MetricRejectCount},
		DimensionKey: "target_host",
		SubDimension: "source=" + string(domain.DomainProxy),
		TopN:         10,
		StartTime:    start,
		EndTime:      end,
	}
	topResult, _ := h.queryMetricsOrFallback(ctx, tq)

	topTargets := []compliancestore.LabelCount{}
	if topResult != nil {
		for _, g := range topResult.Groups {
			_, val := metrics.ParseDimensionKey(g.DimensionKey)
			count := int(g.Values[metrics.MetricRejectCount])
			topTargets = append(topTargets, compliancestore.LabelCount{Label: val, Count: count})
		}
	}

	return &compliancestore.RejectStats{
		TotalRejects:   totalRejects,
		TopTargets:     topTargets,
		TopReasonCodes: []compliancestore.LabelCount{}, // not available from rollup
		BySource:       []compliancestore.LabelCount{}, // not available from rollup
		Period:         compliancestore.TimePeriod{Start: start, End: end},
	}, nil
}
