package instruments

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMergeHistograms(t *testing.T) {
	a := Histogram{1, 2, 3, 4, 5, 6}
	b := Histogram{10, 20, 30, 40, 50, 60}
	got := MergeHistograms(a, b)
	want := Histogram{11, 22, 33, 44, 55, 66}
	if got != want {
		t.Errorf("MergeHistograms(%v, %v) = %v, want %v", a, b, got, want)
	}
}

func TestMergeHistogramsZero(t *testing.T) {
	a := Histogram{5, 10, 15, 20, 25, 30}
	var zero Histogram
	got := MergeHistograms(a, zero)
	if got != a {
		t.Errorf("MergeHistograms(%v, zero) = %v, want %v", a, got, a)
	}
}

func TestHistogramPercentile(t *testing.T) {
	// 100 requests: 20 in [0,50), 30 in [50,100), 50 in [100,200)
	h := Histogram{20, 30, 50, 0, 0, 0}

	tests := []struct {
		name    string
		p       float64
		wantMin float64
		wantMax float64
	}{
		{"P50", 0.50, 50, 100},  // 50th percentile falls in bucket [50,100)
		{"P20", 0.20, 0, 50},    // 20th percentile falls in bucket [0,50)
		{"P95", 0.95, 100, 200}, // 95th percentile falls in bucket [100,200)
		{"P99", 0.99, 100, 200}, // 99th percentile falls in bucket [100,200)
		{"P100", 1.0, 100, 200}, // 100th percentile falls in last populated bucket
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := h.Percentile(tt.p)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("Percentile(%v) = %v, want in [%v, %v]", tt.p, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestHistogramPercentileEmpty(t *testing.T) {
	var h Histogram
	got := h.Percentile(0.5)
	if got != 0 {
		t.Errorf("Percentile on empty histogram = %v, want 0", got)
	}
}

func TestBucketForLatency(t *testing.T) {
	tests := []struct {
		ms   float64
		want int
	}{
		{0, 0},
		{25, 0},
		{49.9, 0},
		{50, 1},
		{75, 1},
		{100, 2},
		{199, 2},
		{200, 3},
		{499, 3},
		{500, 4},
		{999, 4},
		{1000, 5},
		{5000, 5},
		{100000, 5},
	}
	for _, tt := range tests {
		got := BucketForLatency(tt.ms)
		if got != tt.want {
			t.Errorf("BucketForLatency(%v) = %v, want %v", tt.ms, got, tt.want)
		}
	}
}

func TestBuildDimensionKey(t *testing.T) {
	tests := []struct {
		dimension string
		value     string
		want      string
	}{
		{"provider", "openai", "provider=openai"},
		{"model", "gpt-4", "model=gpt-4"},
		{"provider", "", ""},
		{"", "openai", ""},
	}
	for _, tt := range tests {
		got := BuildDimensionKey(tt.dimension, tt.value)
		if got != tt.want {
			t.Errorf("BuildDimensionKey(%q, %q) = %q, want %q", tt.dimension, tt.value, got, tt.want)
		}
	}
}

func TestBuildSubDimension(t *testing.T) {
	tests := []struct {
		source        string
		complianceTag string
		want          string
	}{
		{"vk", "", "source=vk"},
		{"vk", "severity:confidential", "source=vk;compliance_tag=severity:confidential"},
		{"device", "severity:public", "source=device;compliance_tag=severity:public"},
		{"", "", ""},
		{"", "severity:confidential", ""},
	}
	for _, tt := range tests {
		got := BuildSubDimension(tt.source, tt.complianceTag)
		if got != tt.want {
			t.Errorf("BuildSubDimension(%q, %q) = %q, want %q", tt.source, tt.complianceTag, got, tt.want)
		}
	}
}

func TestSelectGranularity(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	// Thresholds match SelectGranularity in types.go: ≤6h→5m, 6h–90d→1h, 90d–365d→1d, >365d→1mo.
	tests := []struct {
		name string
		span time.Duration
		want Granularity
	}{
		{"1h → 5m", 1 * time.Hour, Granularity5m},
		{"6h → 5m", 6 * time.Hour, Granularity5m},
		{"6h1m → 1h", 6*time.Hour + time.Minute, Granularity1h},
		{"3d → 1h", 3 * 24 * time.Hour, Granularity1h},
		{"7d → 1h", 7 * 24 * time.Hour, Granularity1h},
		{"7d1m → 1h", 7*24*time.Hour + time.Minute, Granularity1h},
		{"30d → 1h", 30 * 24 * time.Hour, Granularity1h},
		{"90d → 1h", 90 * 24 * time.Hour, Granularity1h},
		{"90d1m → 1d", 90*24*time.Hour + time.Minute, Granularity1d},
		{"365d → 1d", 365 * 24 * time.Hour, Granularity1d},
		{"365d1m → 1mo", 365*24*time.Hour + time.Minute, Granularity1mo},
		{"366d → 1mo", 366 * 24 * time.Hour, Granularity1mo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := now.Add(-tt.span)
			got := SelectGranularity(start, now)
			if got != tt.want {
				t.Errorf("SelectGranularity(span=%v) = %v, want %v", tt.span, got, tt.want)
			}
		})
	}
}

func TestTruncateTime(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 37, 42, 0, time.UTC)

	tests := []struct {
		gran Granularity
		want time.Time
	}{
		{Granularity5m, time.Date(2026, 4, 15, 14, 35, 0, 0, time.UTC)},
		{Granularity1h, time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC)},
		{Granularity1d, time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)},
		{Granularity1mo, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(string(tt.gran), func(t *testing.T) {
			got := tt.gran.TruncateTime(ts)
			if !got.Equal(tt.want) {
				t.Errorf("%s.TruncateTime(%v) = %v, want %v", tt.gran, ts, got, tt.want)
			}
		})
	}
}

func TestHistogramJSON(t *testing.T) {
	h := Histogram{10, 20, 30, 40, 50, 60}

	data, err := h.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	parsed, err := ParseHistogramMetadata(json.RawMessage(data))
	if err != nil {
		t.Fatalf("ParseHistogramMetadata: %v", err)
	}
	if parsed != h {
		t.Errorf("round-trip: got %v, want %v", parsed, h)
	}
}

func TestParseHistogramMetadataInvalid(t *testing.T) {
	_, err := ParseHistogramMetadata(json.RawMessage(`{"buckets": "bad"}`))
	if err == nil {
		t.Error("expected error for invalid histogram metadata")
	}
}

func TestMergeTimestampMeta(t *testing.T) {
	a := TimestampMeta{FirstSeen: "2026-04-10T00:00:00Z", LastSeen: "2026-04-12T00:00:00Z"}
	b := TimestampMeta{FirstSeen: "2026-04-08T00:00:00Z", LastSeen: "2026-04-14T00:00:00Z"}
	got := MergeTimestampMeta(a, b)
	if got.FirstSeen != "2026-04-08T00:00:00Z" {
		t.Errorf("FirstSeen = %q, want %q", got.FirstSeen, "2026-04-08T00:00:00Z")
	}
	if got.LastSeen != "2026-04-14T00:00:00Z" {
		t.Errorf("LastSeen = %q, want %q", got.LastSeen, "2026-04-14T00:00:00Z")
	}
}

func TestMergeTimestampMetaEmpty(t *testing.T) {
	a := TimestampMeta{FirstSeen: "2026-04-10T00:00:00Z", LastSeen: "2026-04-12T00:00:00Z"}
	var empty TimestampMeta
	got := MergeTimestampMeta(a, empty)
	if got.FirstSeen != a.FirstSeen {
		t.Errorf("FirstSeen = %q, want %q", got.FirstSeen, a.FirstSeen)
	}
	if got.LastSeen != a.LastSeen {
		t.Errorf("LastSeen = %q, want %q", got.LastSeen, a.LastSeen)
	}
}

func TestGranularityTableName(t *testing.T) {
	tests := []struct {
		gran Granularity
		want string
	}{
		{Granularity5m, "metric_rollup_5m"},
		{Granularity1h, "metric_rollup_1h"},
		{Granularity1d, "metric_rollup_1d"},
		{Granularity1mo, "metric_rollup_1mo"},
	}
	for _, tt := range tests {
		got := tt.gran.TableName()
		if got != tt.want {
			t.Errorf("%s.TableName() = %q, want %q", tt.gran, got, tt.want)
		}
	}
}

func TestGranularityBucketDuration(t *testing.T) {
	tests := []struct {
		gran Granularity
		want time.Duration
	}{
		{Granularity5m, 5 * time.Minute},
		{Granularity1h, time.Hour},
		{Granularity1d, 24 * time.Hour},
		{Granularity1mo, 30 * 24 * time.Hour},
	}
	for _, tt := range tests {
		got := tt.gran.BucketDuration()
		if got != tt.want {
			t.Errorf("%s.BucketDuration() = %v, want %v", tt.gran, got, tt.want)
		}
	}
}
