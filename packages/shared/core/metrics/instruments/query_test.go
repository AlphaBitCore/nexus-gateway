package instruments

import (
	"github.com/goccy/go-json"
	"testing"
	"time"
)

func TestBuildResultSummary(t *testing.T) {
	rows := []RollupRow{
		{MetricName: MetricRequestCount, Value: 10},
		{MetricName: MetricRequestCount, Value: 5},
		{MetricName: MetricPromptTokens, Value: 100},
		{MetricName: MetricPromptTokens, Value: 200},
		{MetricName: MetricCompletionTokens, Value: 50},
	}

	q := MetricsQuery{
		Metrics: []string{MetricRequestCount, MetricPromptTokens, MetricCompletionTokens},
	}

	result := BuildResult(q, rows, Granularity1h)

	if result.Granularity != "1h" {
		t.Errorf("Granularity = %q, want %q", result.Granularity, "1h")
	}
	if result.Source != "rollup" {
		t.Errorf("Source = %q, want %q", result.Source, "rollup")
	}
	if result.Summary == nil {
		t.Fatal("Summary is nil")
	}

	wantSums := map[string]float64{
		MetricRequestCount:     15,
		MetricPromptTokens:     300,
		MetricCompletionTokens: 50,
	}
	for metric, want := range wantSums {
		got := result.Summary[metric]
		if got != want {
			t.Errorf("Summary[%q] = %v, want %v", metric, got, want)
		}
	}

	if len(result.Series) != 0 {
		t.Errorf("Series should be empty, got %d", len(result.Series))
	}
	if len(result.Groups) != 0 {
		t.Errorf("Groups should be empty, got %d", len(result.Groups))
	}
}

func TestBuildResultGroups(t *testing.T) {
	rows := []RollupRow{
		{MetricName: MetricRequestCount, DimensionKey: "provider=openai", Value: 10},
		{MetricName: MetricRequestCount, DimensionKey: "provider=openai", Value: 5},
		{MetricName: MetricRequestCount, DimensionKey: "provider=anthropic", Value: 20},
		{MetricName: MetricPromptTokens, DimensionKey: "provider=openai", Value: 100},
		{MetricName: MetricPromptTokens, DimensionKey: "provider=anthropic", Value: 300},
	}

	q := MetricsQuery{
		Metrics:      []string{MetricRequestCount, MetricPromptTokens},
		DimensionKey: "provider",
	}

	result := BuildResult(q, rows, Granularity1d)

	if len(result.Groups) != 2 {
		t.Fatalf("Groups count = %d, want 2", len(result.Groups))
	}

	// Groups should be sorted by first metric (request_count) descending.
	// anthropic=20, openai=15
	if result.Groups[0].DimensionKey != "provider=anthropic" {
		t.Errorf("Groups[0].DimensionKey = %q, want %q", result.Groups[0].DimensionKey, "provider=anthropic")
	}
	if result.Groups[0].Values[MetricRequestCount] != 20 {
		t.Errorf("Groups[0] request_count = %v, want 20", result.Groups[0].Values[MetricRequestCount])
	}
	if result.Groups[0].Values[MetricPromptTokens] != 300 {
		t.Errorf("Groups[0] prompt_tokens = %v, want 300", result.Groups[0].Values[MetricPromptTokens])
	}

	if result.Groups[1].DimensionKey != "provider=openai" {
		t.Errorf("Groups[1].DimensionKey = %q, want %q", result.Groups[1].DimensionKey, "provider=openai")
	}
	if result.Groups[1].Values[MetricRequestCount] != 15 {
		t.Errorf("Groups[1] request_count = %v, want 15", result.Groups[1].Values[MetricRequestCount])
	}

	if result.Summary != nil {
		t.Error("Summary should be nil for grouped query")
	}
}

func TestBuildResultTimeSeries(t *testing.T) {
	t1 := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 4, 15, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	// Rows intentionally out of order to test sorting.
	rows := []RollupRow{
		{MetricName: MetricRequestCount, BucketStart: t3, Value: 30},
		{MetricName: MetricRequestCount, BucketStart: t1, Value: 10},
		{MetricName: MetricRequestCount, BucketStart: t2, Value: 20},
		{MetricName: MetricPromptTokens, BucketStart: t1, Value: 100},
		{MetricName: MetricPromptTokens, BucketStart: t2, Value: 200},
		{MetricName: MetricPromptTokens, BucketStart: t3, Value: 300},
		// Duplicate bucket to test summing.
		{MetricName: MetricRequestCount, BucketStart: t1, Value: 5},
	}

	q := MetricsQuery{
		Metrics:    []string{MetricRequestCount, MetricPromptTokens},
		TimeSeries: true,
	}

	result := BuildResult(q, rows, Granularity1h)

	if len(result.Series) != 3 {
		t.Fatalf("Series count = %d, want 3", len(result.Series))
	}

	// Verify chronological order.
	if !result.Series[0].BucketStart.Equal(t1) {
		t.Errorf("Series[0].BucketStart = %v, want %v", result.Series[0].BucketStart, t1)
	}
	if !result.Series[1].BucketStart.Equal(t2) {
		t.Errorf("Series[1].BucketStart = %v, want %v", result.Series[1].BucketStart, t2)
	}
	if !result.Series[2].BucketStart.Equal(t3) {
		t.Errorf("Series[2].BucketStart = %v, want %v", result.Series[2].BucketStart, t3)
	}

	// Verify values (t1 has 10+5=15 for request_count).
	if result.Series[0].Values[MetricRequestCount] != 15 {
		t.Errorf("Series[0] request_count = %v, want 15", result.Series[0].Values[MetricRequestCount])
	}
	if result.Series[0].Values[MetricPromptTokens] != 100 {
		t.Errorf("Series[0] prompt_tokens = %v, want 100", result.Series[0].Values[MetricPromptTokens])
	}
	if result.Series[1].Values[MetricRequestCount] != 20 {
		t.Errorf("Series[1] request_count = %v, want 20", result.Series[1].Values[MetricRequestCount])
	}
}

func TestBuildResultTopN(t *testing.T) {
	rows := []RollupRow{
		{MetricName: MetricRequestCount, DimensionKey: "provider=a", Value: 50},
		{MetricName: MetricRequestCount, DimensionKey: "provider=b", Value: 40},
		{MetricName: MetricRequestCount, DimensionKey: "provider=c", Value: 30},
		{MetricName: MetricRequestCount, DimensionKey: "provider=d", Value: 20},
		{MetricName: MetricRequestCount, DimensionKey: "provider=e", Value: 10},
	}

	q := MetricsQuery{
		Metrics:      []string{MetricRequestCount},
		DimensionKey: "provider",
		TopN:         3,
	}

	result := BuildResult(q, rows, Granularity1h)

	if len(result.Groups) != 3 {
		t.Fatalf("Groups count = %d, want 3", len(result.Groups))
	}

	// Verify top 3 by request_count descending.
	wantKeys := []string{"provider=a", "provider=b", "provider=c"}
	wantValues := []float64{50, 40, 30}
	for i, g := range result.Groups {
		if g.DimensionKey != wantKeys[i] {
			t.Errorf("Groups[%d].DimensionKey = %q, want %q", i, g.DimensionKey, wantKeys[i])
		}
		if g.Values[MetricRequestCount] != wantValues[i] {
			t.Errorf("Groups[%d] request_count = %v, want %v", i, g.Values[MetricRequestCount], wantValues[i])
		}
	}
}

func TestBuildResultHistogramMetadata(t *testing.T) {
	h1 := Histogram{10, 20, 30, 40, 50, 60}
	h2 := Histogram{5, 10, 15, 20, 25, 30}

	h1JSON, _ := json.Marshal(h1)
	h2JSON, _ := json.Marshal(h2)

	rows := []RollupRow{
		{MetricName: MetricLatencyHistogram, Value: 1, Metadata: h1JSON},
		{MetricName: MetricLatencyHistogram, Value: 1, Metadata: h2JSON},
		{MetricName: MetricRequestCount, Value: 100},
	}

	q := MetricsQuery{
		Metrics: []string{MetricLatencyHistogram, MetricRequestCount},
	}

	result := BuildResult(q, rows, Granularity1h)

	if result.Metadata == nil {
		t.Fatal("Metadata is nil")
	}

	raw, ok := result.Metadata[MetricLatencyHistogram]
	if !ok {
		t.Fatal("Metadata missing latency_histogram key")
	}

	merged, ok := raw.(Histogram)
	if !ok {
		t.Fatalf("Metadata[%q] type = %T, want Histogram", MetricLatencyHistogram, raw)
	}

	want := Histogram{15, 30, 45, 60, 75, 90}
	if merged != want {
		t.Errorf("Merged histogram = %v, want %v", merged, want)
	}
}
