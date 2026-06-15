// Package metrics defines rollup metric types, constants, histogram helpers,
// dimension utilities, and query/result structures used by the analytics
// pipeline (rollup jobs, aggregator, query service).
package instruments

import (
	"encoding/json"
	"fmt"
	"time"
)

// Metric name constants — Tier 1: Traffic

const (
	MetricRequestCount   = "request_count"
	MetricStatus2xxCount = "status_2xx_count"
	MetricStatus4xxCount = "status_4xx_count"
	MetricStatus5xxCount = "status_5xx_count"
	MetricTimeoutCount   = "timeout_count"
	MetricCacheHitCount  = "cache_hit_count"
	// MetricPromptTokens / MetricCompletionTokens / MetricTotalTokens /
	// MetricEstimatedCostUSD are GROSS — every traffic_event row contributes,
	// including failed requests and cache hits. Useful for "what would have
	// been billed" analytics. Quota / billing readers should use the Billed*
	// metrics below instead.
	MetricPromptTokens      = "prompt_tokens"
	MetricCompletionTokens  = "completion_tokens"
	MetricTotalTokens       = "total_tokens"
	MetricEstimatedCostUSD  = "estimated_cost_usd"
	MetricCacheSavedCostUSD = "cache_saved_cost_usd"
	MetricWastedCostUSD     = "wasted_cost_usd"
	// MetricBilledCostUSD / MetricBilledTokens are filtered: only counts
	// successful requests (status 2xx AND error_code IS NULL) AND excludes
	// cache hits. This is what quota.threshold + ai-gateway usage_cache
	// Backfill consume.
	MetricBilledCostUSD       = "billed_cost_usd"
	MetricBilledTokens        = "billed_total_tokens"
	MetricLatencySum          = "latency_sum"
	MetricLatencyCount        = "latency_count"
	MetricLatencyHistogram    = "latency_histogram"
	MetricTTFTSum             = "ttft_sum"
	MetricTTFTCount           = "ttft_count"
	MetricRoutingFallback     = "routing_fallback_count"
	MetricRoutingRuleHit      = "routing_rule_hit_count"
	MetricModelShiftCount     = "model_shift_count"
	MetricQualityAnomalyCount = "quality_anomaly_count"

	// Gateway response cache savings (= estimated_cost_usd for cache-hit rows).
	// Alias for MetricCacheSavedCostUSD — use either name; both refer to the same series.
	MetricGatewayCacheSavingsUSD = "cache_saved_cost_usd"

	// Provider prompt cache metrics.
	MetricCacheWriteCostUSD                  = "cache_write_cost_usd"
	MetricCacheReadSavingsUSD                = "cache_read_savings_usd"
	MetricCacheNetSavingsUSD                 = "cache_net_savings_usd"
	MetricCacheCreationTokens                = "cache_creation_tokens"
	MetricCacheReadTokens                    = "cache_read_tokens"
	MetricRequestsWithProviderPromptCacheHit = "requests_with_provider_prompt_cache_hit"

	// Internal-ops cost rollup metrics. Embedding = L2 lookup
	// cost (stamped on every row that triggered an embedding call, hit or
	// miss). AIGuard = classifier LLM cost (stamped on rows where
	// internal_purpose='ai-guard'). Both are GROSS (every contributing row
	// counted, including failures + cache hits) — analytics consumers
	// must NOT include these in user-quota / billing calculations.
	MetricEmbeddingCostUSD = "embedding_cost_usd"
	MetricAIGuardCostUSD   = "ai_guard_cost_usd"

	// Normalisation pipeline metrics.
	MetricNormalisedStripCount = "normalised_strip_count"
	MetricNormalisedStripBytes = "normalised_strip_bytes"
	MetricCacheMarkersInjected = "cache_markers_injected"

	// Latency phase metrics. Mirrors the agent-local rollup keys so
	// Hub-side and agent-local rollups share names. Hub rollup_5m job
	// accumulates these from traffic_event's per-row upstream/hooks
	// columns (see packages/nexus-hub/internal/jobs/rollup_5m.go).
	MetricLatencyUsSum             = "latency_us_sum"
	MetricLatencyUsCount           = "latency_us_count"
	MetricLatencyUpstreamTtfbSum   = "latency_upstream_ttfb_sum"
	MetricLatencyUpstreamTtfbCount = "latency_upstream_ttfb_count"
	MetricLatencyUpstreamSum       = "latency_upstream_total_sum"
	MetricLatencyUpstreamCount     = "latency_upstream_total_count"
	MetricLatencyHooksSum          = "latency_hooks_sum"
	MetricLatencyHooksCount        = "latency_hooks_count"
)

// Metric name constants — Tier 2: Compliance

const (
	MetricHookAllowCount      = "hook_allow_count"
	MetricHookDenyCount       = "hook_deny_count"
	MetricHookErrorCount      = "hook_error_count"
	MetricHookUnknownCount    = "hook_unknown_count"
	MetricHookLatencySum      = "hook_latency_sum"
	MetricHookLatencyCount    = "hook_latency_count"
	MetricHookLatencyHist     = "hook_latency_histogram"
	MetricBumpSuccessCount    = "bump_success_count"
	MetricBumpFailedCount     = "bump_failed_count"
	MetricBumpExemptCount     = "bump_exempt_count"
	MetricBumpDisabledCount   = "bump_disabled_count"
	MetricProxyRequestCount   = "proxy_request_count"
	MetricClassificationCount = "classification_count"
	MetricRejectCount         = "reject_count"
)

// Metric name constants — Tier 2: Usage

const (
	MetricActiveEntities      = "active_entities" // unified: users + projects (replaces active_users, active_virtual_keys, active_devices)
	MetricActiveOrganizations = "active_organizations"
)

// Metric name constants — Tier 2: Discovery

const (
	MetricDistinctSources = "distinct_sources"
	MetricFirstSeen       = "first_seen"
	MetricLastSeen        = "last_seen"
)

// Metric name constants — Tier 3: Fleet

const (
	MetricDeviceFleetStatus = "device_fleet_status"
	MetricDeviceFleetOS     = "device_fleet_os"
	MetricAgentActionVolume = "agent_action_volume"
)

// BuildDimensionKey returns a "dimension=value" string, or "" if either
// argument is empty.
func BuildDimensionKey(dimension, value string) string {
	if dimension == "" || value == "" {
		return ""
	}
	return dimension + "=" + value
}

// BuildSubDimension returns a sub-dimension qualifier string. It always
// includes "source=<source>" and optionally appends
// ";compliance_tag=<tag>" when complianceTag is non-empty — rollup jobs
// UNNEST the compliance_tags array and pass the per-row tag here so the
// metric is sliceable by individual tag value. Returns "" if source is
// empty.
func BuildSubDimension(source, complianceTag string) string {
	if source == "" {
		return ""
	}
	s := "source=" + source
	if complianceTag != "" {
		s += ";compliance_tag=" + complianceTag
	}
	return s
}

// Histogram — 6 buckets: [0,50) [50,100) [100,200) [200,500) [500,1000) [1000,+∞)

// HistogramBucketCount is the number of latency buckets.
const HistogramBucketCount = 6

// HistogramBoundaries defines the upper bound (exclusive) for each bucket.
// The last boundary is effectively +infinity.
var HistogramBoundaries = [HistogramBucketCount]float64{50, 100, 200, 500, 1000, 1e9}

// Histogram holds counts for 6 latency buckets.
type Histogram [HistogramBucketCount]int64

// MergeHistograms returns the element-wise sum of two histograms.
func MergeHistograms(a, b Histogram) Histogram {
	var out Histogram
	for i := range out {
		out[i] = a[i] + b[i]
	}
	return out
}

// BucketForLatency returns the bucket index for a given latency in
// milliseconds.
func BucketForLatency(ms float64) int {
	for i, bound := range HistogramBoundaries {
		if ms < bound {
			return i
		}
	}
	return HistogramBucketCount - 1
}

// lowerBound returns the lower bound of bucket i.
func lowerBound(i int) float64 {
	if i == 0 {
		return 0
	}
	return HistogramBoundaries[i-1]
}

// Percentile estimates the p-th percentile (p in [0,1]) from the histogram
// using linear interpolation within the target bucket.
func (h Histogram) Percentile(p float64) float64 {
	var total int64
	for _, c := range h {
		total += c
	}
	if total == 0 {
		return 0
	}

	target := p * float64(total)
	var cumulative int64
	for i, c := range h {
		if c == 0 {
			continue
		}
		prev := cumulative
		cumulative += c
		if float64(cumulative) >= target {
			// Linear interpolation within this bucket.
			lo := lowerBound(i)
			hi := HistogramBoundaries[i]
			if hi > 1e8 {
				// Last bucket: use 2x lower bound as upper estimate.
				hi = lo * 2
				if hi == 0 {
					hi = 1
				}
			}
			fraction := (target - float64(prev)) / float64(c)
			return lo + fraction*(hi-lo)
		}
	}
	return HistogramBoundaries[HistogramBucketCount-2] // fallback
}

// histogramJSON is the JSON wire format for Histogram.
type histogramJSON struct {
	Buckets [HistogramBucketCount]int64 `json:"buckets"`
}

// MarshalJSON serializes the histogram as {"buckets": [...]}.
func (h Histogram) MarshalJSON() ([]byte, error) {
	return json.Marshal(histogramJSON{Buckets: h})
}

// ParseHistogramMetadata deserializes a histogram from {"buckets": [...]}.
func ParseHistogramMetadata(raw json.RawMessage) (Histogram, error) {
	var wrapper histogramJSON
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return Histogram{}, fmt.Errorf("parse histogram metadata: %w", err)
	}
	return Histogram(wrapper.Buckets), nil
}

// Granularity represents a rollup time-bucket size.
type Granularity string

const (
	Granularity5m  Granularity = "5m"
	Granularity1h  Granularity = "1h"
	Granularity1d  Granularity = "1d"
	Granularity1mo Granularity = "1mo"
)

// TableName returns the PostgreSQL table name for this granularity.
func (g Granularity) TableName() string {
	return "metric_rollup_" + string(g)
}

// BucketDuration returns the duration of a single bucket for this
// granularity. For monthly granularity, 30 days is used as an approximation.
func (g Granularity) BucketDuration() time.Duration {
	switch g {
	case Granularity5m:
		return 5 * time.Minute
	case Granularity1h:
		return time.Hour
	case Granularity1d:
		return 24 * time.Hour
	case Granularity1mo:
		return 30 * 24 * time.Hour
	default:
		return time.Hour
	}
}

// TruncateTime truncates t to the start of its bucket boundary.
func (g Granularity) TruncateTime(t time.Time) time.Time {
	switch g {
	case Granularity5m:
		return t.Truncate(5 * time.Minute)
	case Granularity1h:
		return t.Truncate(time.Hour)
	case Granularity1d:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	case Granularity1mo:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	default:
		return t.Truncate(time.Hour)
	}
}

// SelectGranularity chooses the best granularity for the given time range:
//
//	≤6h    → 5m  (fine-grained, 7-day retention)
//	6h–90d → 1h  (90-day retention, avoids incomplete-day issues)
//	90d–1y → 1d  (365-day retention)
//	>1y    → 1mo (5-year retention)
func SelectGranularity(start, end time.Time) Granularity {
	span := end.Sub(start)
	switch {
	case span <= 6*time.Hour:
		return Granularity5m
	case span <= 90*24*time.Hour:
		return Granularity1h
	case span <= 365*24*time.Hour:
		return Granularity1d
	default:
		return Granularity1mo
	}
}

// Query and result types

// MetricsQuery describes a metrics read request.
type MetricsQuery struct {
	Metrics      []string  `json:"metrics"`
	DimensionKey string    `json:"dimension_key"`
	SubDimension string    `json:"sub_dimension"`
	StartTime    time.Time `json:"start_time"`
	EndTime      time.Time `json:"end_time"`
	TopN         int       `json:"top_n"`
	TimeSeries   bool      `json:"time_series"`
}

// MetricsResult holds the response for a metrics query.
type MetricsResult struct {
	Granularity string             `json:"granularity"`
	Source      string             `json:"source"`
	Summary     map[string]float64 `json:"summary"`
	Series      []MetricsBucket    `json:"series,omitempty"`
	Groups      []MetricsGroup     `json:"groups,omitempty"`
	Metadata    map[string]any     `json:"metadata,omitempty"`
}

// MetricsBucket is a single time-series data point.
// JSON tags use camelCase to match the rest of the admin API surface.
type MetricsBucket struct {
	BucketStart time.Time          `json:"bucketStart"`
	Values      map[string]float64 `json:"values"`
	Meta        map[string]any     `json:"meta,omitempty"`
}

// MetricsGroup holds aggregated values for a single dimension value.
type MetricsGroup struct {
	DimensionKey string             `json:"dimensionKey"`
	Values       map[string]float64 `json:"values"`
	Meta         map[string]any     `json:"meta,omitempty"`
}

// RollupRow represents a single row in a metric_rollup_* table.
type RollupRow struct {
	ID           string          `json:"id"`
	BucketStart  time.Time       `json:"bucket_start"`
	MetricName   string          `json:"metric_name"`
	DimensionKey string          `json:"dimension_key"`
	SubDimension string          `json:"sub_dimension"`
	Value        float64         `json:"value"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// ThingRollupRow represents a single row in a thing_metric_rollup_* table.
// Mirrors RollupRow with ThingID promoted to a top-level column so per-Thing
// dashboards can filter without dim-key gymnastics. Only data plane Things
// (agent / compliance-proxy / ai-gateway) emit rows.
type ThingRollupRow struct {
	ID           string          `json:"id"`
	BucketStart  time.Time       `json:"bucket_start"`
	ThingID      string          `json:"thing_id"`
	MetricName   string          `json:"metric_name"`
	DimensionKey string          `json:"dimension_key"`
	SubDimension string          `json:"sub_dimension"`
	Value        float64         `json:"value"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// TimestampMeta tracks first_seen / last_seen as RFC 3339 strings.
type TimestampMeta struct {
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
}

// MergeTimestampMeta returns the union: MIN of FirstSeen, MAX of LastSeen.
// Empty strings in either input are treated as absent.
func MergeTimestampMeta(a, b TimestampMeta) TimestampMeta {
	out := a
	if b.FirstSeen != "" && (out.FirstSeen == "" || b.FirstSeen < out.FirstSeen) {
		out.FirstSeen = b.FirstSeen
	}
	if b.LastSeen != "" && (out.LastSeen == "" || b.LastSeen > out.LastSeen) {
		out.LastSeen = b.LastSeen
	}
	return out
}
