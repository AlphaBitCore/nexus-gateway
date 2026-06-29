package instruments

import (
	"testing"
	"time"
)

// TestAggregationKindFor pins the registry: the three distinct-cardinality
// metrics are AggregationDistinct, metadata-backed metrics carry their kind,
// and any unregistered name defaults to AggregationSum.
func TestAggregationKindFor(t *testing.T) {
	tests := []struct {
		name string
		want AggregationKind
	}{
		{MetricActiveEntities, AggregationDistinct},
		{MetricActiveOrganizations, AggregationDistinct},
		{MetricDistinctSources, AggregationDistinct},
		{MetricLatencyHistogram, AggregationHistogram},
		{MetricHookLatencyHist, AggregationHistogram},
		{MetricFirstSeen, AggregationTimestamp},
		{MetricLastSeen, AggregationTimestamp},
		{MetricRequestCount, AggregationSum},
		{MetricBilledCostUSD, AggregationSum},
		{"totally_unregistered_metric", AggregationSum},
	}
	for _, tt := range tests {
		if got := AggregationKindFor(tt.name); got != tt.want {
			t.Errorf("AggregationKindFor(%q) = %d, want %d", tt.name, got, tt.want)
		}
	}
}

// TestCombineValues verifies the fold: sum-kind adds, distinct-kind takes the
// max, and the order of (acc, v) does not matter for the max path.
func TestCombineValues(t *testing.T) {
	tests := []struct {
		name   string
		metric string
		acc, v float64
		want   float64
	}{
		{"sum adds", MetricRequestCount, 10, 25, 35},
		{"sum from zero", MetricPromptTokens, 0, 7, 7},
		{"unregistered sums", "unknown_metric", 3, 4, 7},
		{"distinct keeps larger (v>acc)", MetricActiveEntities, 5, 8, 8},
		{"distinct keeps larger (acc>v)", MetricActiveEntities, 8, 5, 8},
		{"distinct equal", MetricActiveOrganizations, 5, 5, 5},
		{"distinct from zero", MetricDistinctSources, 0, 3, 3},
	}
	for _, tt := range tests {
		if got := CombineValues(tt.metric, tt.acc, tt.v); got != tt.want {
			t.Errorf("%s: CombineValues(%q, %v, %v) = %v, want %v",
				tt.name, tt.metric, tt.acc, tt.v, got, tt.want)
		}
	}
}

// TestMergeRollupRowsDistinctMax is the core regression: three 5m
// buckets each reporting 5 distinct active entities for the same cell must
// merge to 5 (the per-bucket max), NOT 15 (the sum). A neighbouring sum-kind
// metric in the same input still sums, proving the registry routes per-metric.
func TestMergeRollupRowsDistinctMax(t *testing.T) {
	rows := []RollupRow{
		{MetricName: MetricActiveEntities, DimensionKey: "target_host=api.openai.com", Value: 5},
		{MetricName: MetricActiveEntities, DimensionKey: "target_host=api.openai.com", Value: 5},
		{MetricName: MetricActiveEntities, DimensionKey: "target_host=api.openai.com", Value: 5},
		// Sum-kind control in the same merge batch.
		{MetricName: MetricRequestCount, DimensionKey: "target_host=api.openai.com", Value: 5},
		{MetricName: MetricRequestCount, DimensionKey: "target_host=api.openai.com", Value: 5},
		{MetricName: MetricRequestCount, DimensionKey: "target_host=api.openai.com", Value: 5},
	}

	merged := MergeRollupRows(rows)

	got := map[string]float64{}
	for _, r := range merged {
		got[r.MetricName] = r.Value
	}
	if got[MetricActiveEntities] != 5 {
		t.Errorf("active_entities merged = %v, want 5 (max, not summed)", got[MetricActiveEntities])
	}
	if got[MetricRequestCount] != 15 {
		t.Errorf("request_count merged = %v, want 15 (sum)", got[MetricRequestCount])
	}
}

// TestMergeRollupRowsDistinctVaryingMax confirms the distinct fold returns the
// largest constituent bucket, not the first or last.
func TestMergeRollupRowsDistinctVaryingMax(t *testing.T) {
	rows := []RollupRow{
		{MetricName: MetricDistinctSources, DimensionKey: "model=gpt", Value: 3},
		{MetricName: MetricDistinctSources, DimensionKey: "model=gpt", Value: 11},
		{MetricName: MetricDistinctSources, DimensionKey: "model=gpt", Value: 7},
	}
	merged := MergeRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged row, got %d", len(merged))
	}
	if merged[0].Value != 11 {
		t.Errorf("distinct_sources merged = %v, want 11 (max)", merged[0].Value)
	}
}

// TestMergeRollupRowsUnknownMetricStillSums proves the change is additive: a
// metric absent from the registry keeps the historical SUM behavior.
func TestMergeRollupRowsUnknownMetricStillSums(t *testing.T) {
	rows := []RollupRow{
		{MetricName: "experimental_counter", DimensionKey: "k=v", Value: 4},
		{MetricName: "experimental_counter", DimensionKey: "k=v", Value: 6},
	}
	merged := MergeRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged row, got %d", len(merged))
	}
	if merged[0].Value != 10 {
		t.Errorf("unknown metric merged = %v, want 10 (sum, unchanged behavior)", merged[0].Value)
	}
}

// TestMergeThingRollupRowsDistinctMax mirrors the fleet regression for the
// per-Thing pipeline: distinct counts for one Thing must not sum across
// buckets, while distinct counts for a different Thing stay isolated.
func TestMergeThingRollupRowsDistinctMax(t *testing.T) {
	rows := []ThingRollupRow{
		{ThingID: "thing-a", MetricName: MetricActiveOrganizations, DimensionKey: "", Value: 4},
		{ThingID: "thing-a", MetricName: MetricActiveOrganizations, DimensionKey: "", Value: 4},
		{ThingID: "thing-a", MetricName: MetricActiveOrganizations, DimensionKey: "", Value: 4},
		{ThingID: "thing-b", MetricName: MetricActiveOrganizations, DimensionKey: "", Value: 2},
		// Sum-kind control for thing-a.
		{ThingID: "thing-a", MetricName: MetricRequestCount, DimensionKey: "", Value: 4},
		{ThingID: "thing-a", MetricName: MetricRequestCount, DimensionKey: "", Value: 4},
	}

	merged := MergeThingRollupRows(rows)

	type key struct{ thing, metric string }
	got := map[key]float64{}
	for _, r := range merged {
		got[key{r.ThingID, r.MetricName}] = r.Value
	}
	if got[key{"thing-a", MetricActiveOrganizations}] != 4 {
		t.Errorf("thing-a active_organizations = %v, want 4 (max, not 12)", got[key{"thing-a", MetricActiveOrganizations}])
	}
	if got[key{"thing-b", MetricActiveOrganizations}] != 2 {
		t.Errorf("thing-b active_organizations = %v, want 2 (isolated)", got[key{"thing-b", MetricActiveOrganizations}])
	}
	if got[key{"thing-a", MetricRequestCount}] != 8 {
		t.Errorf("thing-a request_count = %v, want 8 (sum)", got[key{"thing-a", MetricRequestCount}])
	}
}

// TestMergeThingRollupRowsUnknownMetricStillSums proves additivity for the
// per-Thing twin.
func TestMergeThingRollupRowsUnknownMetricStillSums(t *testing.T) {
	rows := []ThingRollupRow{
		{ThingID: "t1", MetricName: "experimental_counter", Value: 2},
		{ThingID: "t1", MetricName: "experimental_counter", Value: 9},
	}
	merged := MergeThingRollupRows(rows)
	if len(merged) != 1 || merged[0].Value != 11 {
		t.Fatalf("unknown thing metric = %+v, want single row value 11 (sum)", merged)
	}
}

// TestBuildResultTopDestinationsDeInflated is the read-path regression for the
// ~288x over-count. It reproduces the Top Destinations query shape: a 24-hour
// window read at 1h granularity, so 24 hourly rows land for one target host,
// each already reporting 5 distinct active entities. The grouped DeviceCount
// (active_entities) must come back as 5 (the per-bucket peak), not 120 (24x5).
// request_count, an additive metric, still sums across the 24 buckets.
func TestBuildResultTopDestinationsDeInflated(t *testing.T) {
	const buckets = 24
	const distinctPerBucket = 5
	const requestsPerBucket = 100

	var rows []RollupRow
	base := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	for i := range buckets {
		bs := base.Add(time.Duration(i) * time.Hour)
		rows = append(rows,
			RollupRow{BucketStart: bs, MetricName: MetricActiveEntities, DimensionKey: "target_host=api.openai.com", SubDimension: "source=agent", Value: distinctPerBucket},
			RollupRow{BucketStart: bs, MetricName: MetricRequestCount, DimensionKey: "target_host=api.openai.com", SubDimension: "source=agent", Value: requestsPerBucket},
		)
	}

	q := MetricsQuery{
		Metrics:      []string{MetricRequestCount, MetricActiveEntities},
		DimensionKey: "target_host",
		SubDimension: "source=agent",
		TopN:         50,
	}
	result := BuildResult(q, rows, Granularity1h)

	if len(result.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(result.Groups))
	}
	g := result.Groups[0]
	deviceCount := g.Values[MetricActiveEntities]
	if deviceCount != distinctPerBucket {
		t.Errorf("DeviceCount = %v, want %d (de-inflated max). The summed bug would yield %d",
			deviceCount, distinctPerBucket, buckets*distinctPerBucket)
	}
	if eventCount := g.Values[MetricRequestCount]; eventCount != buckets*requestsPerBucket {
		t.Errorf("EventCount = %v, want %d (additive sum)", eventCount, buckets*requestsPerBucket)
	}
}

// TestBuildResultSummaryDistinctMax verifies the flat-summary path also folds
// distinct metrics with max rather than summing across buckets.
func TestBuildResultSummaryDistinctMax(t *testing.T) {
	rows := []RollupRow{
		{MetricName: MetricActiveEntities, Value: 6},
		{MetricName: MetricActiveEntities, Value: 9},
		{MetricName: MetricActiveEntities, Value: 4},
		{MetricName: MetricRequestCount, Value: 10},
		{MetricName: MetricRequestCount, Value: 10},
	}
	q := MetricsQuery{Metrics: []string{MetricActiveEntities, MetricRequestCount}}
	result := BuildResult(q, rows, Granularity1h)
	if got := result.Summary[MetricActiveEntities]; got != 9 {
		t.Errorf("summary active_entities = %v, want 9 (max)", got)
	}
	if got := result.Summary[MetricRequestCount]; got != 20 {
		t.Errorf("summary request_count = %v, want 20 (sum)", got)
	}
}

// TestBuildResultTimeSeriesDistinctMax verifies the time-series path folds
// distinct metrics with max within a bucket (multiple dimension rows in the
// same bucket) instead of summing them.
func TestBuildResultTimeSeriesDistinctMax(t *testing.T) {
	bs := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	rows := []RollupRow{
		{BucketStart: bs, MetricName: MetricActiveEntities, DimensionKey: "model=a", Value: 3},
		{BucketStart: bs, MetricName: MetricActiveEntities, DimensionKey: "model=b", Value: 7},
		{BucketStart: bs, MetricName: MetricRequestCount, DimensionKey: "model=a", Value: 3},
		{BucketStart: bs, MetricName: MetricRequestCount, DimensionKey: "model=b", Value: 7},
	}
	q := MetricsQuery{Metrics: []string{MetricActiveEntities, MetricRequestCount}, TimeSeries: true}
	result := BuildResult(q, rows, Granularity5m)
	if len(result.Series) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(result.Series))
	}
	vals := result.Series[0].Values
	if vals[MetricActiveEntities] != 7 {
		t.Errorf("series active_entities = %v, want 7 (max across dims)", vals[MetricActiveEntities])
	}
	if vals[MetricRequestCount] != 10 {
		t.Errorf("series request_count = %v, want 10 (sum across dims)", vals[MetricRequestCount])
	}
}
