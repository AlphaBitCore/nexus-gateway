package instruments

import (
	"encoding/json"
	"testing"
)

func TestMergeRollupRows(t *testing.T) {
	rows := []RollupRow{
		{MetricName: "request_count", DimensionKey: "provider=openai", SubDimension: "", Value: 10},
		{MetricName: "request_count", DimensionKey: "provider=openai", SubDimension: "", Value: 25},
		{MetricName: "request_count", DimensionKey: "provider=anthropic", SubDimension: "", Value: 5},
	}

	merged := MergeRollupRows(rows)

	if len(merged) != 2 {
		t.Fatalf("expected 2 merged rows, got %d", len(merged))
	}

	// Verify insertion order is preserved.
	if merged[0].DimensionKey != "provider=openai" {
		t.Errorf("first row dimension_key = %q, want %q", merged[0].DimensionKey, "provider=openai")
	}
	if merged[0].Value != 35 {
		t.Errorf("merged openai value = %v, want 35", merged[0].Value)
	}
	if merged[1].DimensionKey != "provider=anthropic" {
		t.Errorf("second row dimension_key = %q, want %q", merged[1].DimensionKey, "provider=anthropic")
	}
	if merged[1].Value != 5 {
		t.Errorf("merged anthropic value = %v, want 5", merged[1].Value)
	}
}

func TestMergeRollupRowsHistogram(t *testing.T) {
	h1 := Histogram{10, 20, 30, 40, 50, 60}
	h2 := Histogram{1, 2, 3, 4, 5, 6}
	want := Histogram{11, 22, 33, 44, 55, 66}

	meta1, err := json.Marshal(h1)
	if err != nil {
		t.Fatalf("marshal h1: %v", err)
	}
	meta2, err := json.Marshal(h2)
	if err != nil {
		t.Fatalf("marshal h2: %v", err)
	}

	rows := []RollupRow{
		{MetricName: MetricLatencyHistogram, DimensionKey: "provider=openai", Value: 0, Metadata: meta1},
		{MetricName: MetricLatencyHistogram, DimensionKey: "provider=openai", Value: 0, Metadata: meta2},
	}

	merged := MergeRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged row, got %d", len(merged))
	}

	got, err := ParseHistogramMetadata(merged[0].Metadata)
	if err != nil {
		t.Fatalf("parse merged histogram: %v", err)
	}
	if got != want {
		t.Errorf("merged histogram = %v, want %v", got, want)
	}
}

func TestMergeRollupRowsTimestamp(t *testing.T) {
	meta1, _ := json.Marshal(TimestampMeta{
		FirstSeen: "2026-04-10T00:00:00Z",
		LastSeen:  "2026-04-12T00:00:00Z",
	})
	meta2, _ := json.Marshal(TimestampMeta{
		FirstSeen: "2026-04-08T00:00:00Z",
		LastSeen:  "2026-04-11T00:00:00Z",
	})

	rows := []RollupRow{
		{MetricName: MetricFirstSeen, DimensionKey: "provider=openai", Value: 0, Metadata: meta1},
		{MetricName: MetricFirstSeen, DimensionKey: "provider=openai", Value: 0, Metadata: meta2},
	}

	merged := MergeRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged row, got %d", len(merged))
	}

	var got TimestampMeta
	if err := json.Unmarshal(merged[0].Metadata, &got); err != nil {
		t.Fatalf("unmarshal timestamp meta: %v", err)
	}
	if got.FirstSeen != "2026-04-08T00:00:00Z" {
		t.Errorf("FirstSeen = %q, want %q", got.FirstSeen, "2026-04-08T00:00:00Z")
	}
	if got.LastSeen != "2026-04-12T00:00:00Z" {
		t.Errorf("LastSeen = %q, want %q", got.LastSeen, "2026-04-12T00:00:00Z")
	}
}

func TestIsHistogramMetric(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{MetricLatencyHistogram, true},
		{MetricHookLatencyHist, true},
		{"request_count", false},
		{"prompt_tokens", false},
	}
	for _, tt := range tests {
		got := IsHistogramMetric(tt.name)
		if got != tt.want {
			t.Errorf("IsHistogramMetric(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestIsTimestampMetric(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{MetricFirstSeen, true},
		{MetricLastSeen, true},
		{"request_count", false},
		{MetricLatencyHistogram, false},
	}
	for _, tt := range tests {
		got := IsTimestampMetric(tt.name)
		if got != tt.want {
			t.Errorf("IsTimestampMetric(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestParseDimensionKey(t *testing.T) {
	tests := []struct {
		input     string
		wantDim   string
		wantValue string
	}{
		{"provider=openai", "provider", "openai"},
		{"model=gpt-4", "model", "gpt-4"},
		{"user=user@example.com", "user", "user@example.com"},
		{"", "", ""},
		{"nodimension", "nodimension", ""},
	}
	for _, tt := range tests {
		dim, val := ParseDimensionKey(tt.input)
		if dim != tt.wantDim || val != tt.wantValue {
			t.Errorf("ParseDimensionKey(%q) = (%q, %q), want (%q, %q)",
				tt.input, dim, val, tt.wantDim, tt.wantValue)
		}
	}
}
