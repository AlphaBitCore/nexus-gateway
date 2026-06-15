package instruments

// AggregationKind classifies how a metric's scalar Value combines when two
// rollup rows that share a logical identity — (metric, dimension,
// sub-dimension), plus thing_id for the per-Thing pipeline — are folded
// together. Folding happens in two places that must agree: the merge cascade
// (5m→1h→1d→1mo, MergeRollupRows / MergeThingRollupRows) and the read-side
// aggregation that collapses every bucket in a query window (BuildResult).
//
// The default for any name absent from the registry is AggregationSum, which
// is exactly the historical blanket-sum behavior. Introducing the registry is
// therefore additive: only the explicitly classified metrics below change.
type AggregationKind int

const (
	// AggregationSum adds the two values. Correct for additive counters and
	// money — request_count, *_tokens, *_cost_usd, latency_sum, etc. This is
	// the zero value and the default for unregistered metrics.
	AggregationSum AggregationKind = iota

	// AggregationMax keeps the larger value. For gauge-like series whose
	// coarse-tier value must never exceed any constituent finer-tier value.
	AggregationMax

	// AggregationDistinct is a (currently approximated) mergeable cardinality.
	// Each finer bucket stores the count of distinct identifiers observed in
	// that bucket (Value = len(set)). The true union cardinality across
	// buckets cannot be recovered from per-bucket counts alone without a
	// sketch (e.g. HyperLogLog) on the emit side. Until the emit side carries
	// such a sketch, distinct metrics merge by MAX — the largest single-bucket
	// distinct count — which is a guaranteed LOWER BOUND on the true union but
	// eliminates the catastrophic over-count that blanket-summing produced: a
	// 24-hour read at 1h granularity summed up to 288 per-5m-bucket counts
	// (12 five-minute buckets × 24 hours). See metrics-rollup-architecture.md
	// §5 "Distinct-cardinality merge" for the trade-off and the HLL follow-up.
	AggregationDistinct

	// AggregationHistogram folds the six-element latency-bucket array stored in
	// the metadata field (element-wise add); the scalar Value is unused and
	// must never be summed.
	AggregationHistogram

	// AggregationTimestamp folds first_seen / last_seen in the metadata field
	// (MIN of first_seen, MAX of last_seen); the scalar Value is unused.
	AggregationTimestamp
)

// metricAggregationKinds maps a metric name to its aggregation kind. Names
// absent from this map default to AggregationSum via AggregationKindFor, so the
// registry is additive — only the metrics listed here diverge from summing.
var metricAggregationKinds = map[string]AggregationKind{
	// Distinct-cardinality (count-of-set) metrics. Summing these across the
	// merge cascade or across a multi-bucket read window over-counts wildly;
	// they merge by MAX instead (a lower bound — see AggregationDistinct).
	MetricActiveEntities:      AggregationDistinct,
	MetricActiveOrganizations: AggregationDistinct,
	MetricDistinctSources:     AggregationDistinct,

	// Metadata-backed metrics. Registered here so the registry is the single
	// source of truth for "is this metric summed?"; IsHistogramMetric and
	// IsTimestampMetric derive from these entries, and the merge routes their
	// metadata through the dedicated mergers rather than touching Value.
	MetricLatencyHistogram: AggregationHistogram,
	MetricHookLatencyHist:  AggregationHistogram,
	MetricFirstSeen:        AggregationTimestamp,
	MetricLastSeen:         AggregationTimestamp,
}

// AggregationKindFor returns the registered aggregation kind for a metric,
// defaulting to AggregationSum for any unregistered name.
func AggregationKindFor(name string) AggregationKind {
	if k, ok := metricAggregationKinds[name]; ok {
		return k
	}
	return AggregationSum
}

// CombineValues folds two scalar rollup values for the named metric according
// to its aggregation kind. Sum-kind metrics (the default) add; every other
// kind keeps the larger value and never sums. Histogram and timestamp metrics
// carry no meaningful scalar Value (it is zero), so routing them through the
// max path here is harmless — their real payload is merged separately from
// metadata by the dedicated mergers, and callers must not rely on CombineValues
// to combine that payload.
func CombineValues(name string, acc, v float64) float64 {
	if AggregationKindFor(name) == AggregationSum {
		return acc + v
	}
	if v > acc {
		return v
	}
	return acc
}
