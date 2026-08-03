package instruments

import (
	"testing"

	"github.com/goccy/go-json"
)

func TestBuildErrorClassValue_RoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		class ErrorClass
	}{
		{"plain", ErrorClass{"invalid_request", "4xx", "openai", "gpt-4o-mini"}},
		{"empty code and provider", ErrorClass{"", "5xx", "", "claude-opus-4-8"}},
		{"pipe in model", ErrorClass{"invalid_request", "4xx", "openai", "evil|model|name"}},
		{"percent in model", ErrorClass{"invalid_request", "4xx", "openai", "50%off"}},
		{"escape-marker collision", ErrorClass{"invalid_request", "4xx", "openai", "%7C|%25"}},
		{"all empty but range", ErrorClass{"", "4xx", "", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := BuildErrorClassValue(tc.class)
			got, ok := ParseErrorClassValue(v)
			if !ok {
				t.Fatalf("ParseErrorClassValue(%q) not ok", v)
			}
			if got != tc.class {
				t.Fatalf("round trip mismatch: %+v -> %q -> %+v", tc.class, v, got)
			}
		})
	}
}

func TestParseErrorClassValue_RejectsWrongArity(t *testing.T) {
	for _, v := range []string{"", "a", "a|b", "a|b|c", "a|b|c|d|e"} {
		if _, ok := ParseErrorClassValue(v); ok {
			t.Errorf("ParseErrorClassValue(%q) = ok, want reject", v)
		}
	}
}

// TestErrorClassSeen_AggregationKind pins the merge behavior: the seen series
// must fold MIN(first)/MAX(last) through the cascade, and the count series
// must stay on the additive default — a mis-registered kind would silently
// corrupt first/last-seen (summed strings are meaningless) or drop counts.
func TestErrorClassSeen_AggregationKind(t *testing.T) {
	if got := AggregationKindFor(MetricTrafficErrorClassSeen); got != AggregationTimestamp {
		t.Errorf("seen kind = %v, want AggregationTimestamp", got)
	}
	if !IsTimestampMetric(MetricTrafficErrorClassSeen) {
		t.Error("IsTimestampMetric(seen) = false, want true")
	}
	if got := AggregationKindFor(MetricTrafficErrorClassCount); got != AggregationSum {
		t.Errorf("count kind = %v, want AggregationSum", got)
	}
}

// TestMergeRollupRows_ErrorClassSeries proves the merge cascade (5m→1h→1d→1mo)
// inherits the error-class series with correct semantics: count rows sum,
// seen rows fold MIN(first_seen)/MAX(last_seen) — the exact behavior the
// Control Plane's rollup-first error-governance read depends on for
// class counts and first/last-seen columns at coarse granularities.
func TestMergeRollupRows_ErrorClassSeries(t *testing.T) {
	dim := BuildDimensionKey(DimensionErrorClass,
		BuildErrorClassValue(ErrorClass{ErrorCode: "RATE_LIMITED", StatusRange: "4xx", Provider: "openai", Model: "gpt-4o-mini"}))
	m1, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-07-17T10:01:00Z", LastSeen: "2026-07-17T10:03:00Z"})
	m2, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-07-17T10:00:30Z", LastSeen: "2026-07-17T10:04:59Z"})

	merged := MergeRollupRows([]RollupRow{
		{MetricName: MetricTrafficErrorClassCount, DimensionKey: dim, SubDimension: "source=vk", Value: 7},
		{MetricName: MetricTrafficErrorClassCount, DimensionKey: dim, SubDimension: "source=vk", Value: 5},
		{MetricName: MetricTrafficErrorClassSeen, DimensionKey: dim, SubDimension: "source=vk", Metadata: m1},
		{MetricName: MetricTrafficErrorClassSeen, DimensionKey: dim, SubDimension: "source=vk", Metadata: m2},
	})
	if len(merged) != 2 {
		t.Fatalf("merged rows = %d, want 2", len(merged))
	}
	for _, r := range merged {
		switch r.MetricName {
		case MetricTrafficErrorClassCount:
			if r.Value != 12 {
				t.Errorf("count merged = %v, want 12 (sum)", r.Value)
			}
		case MetricTrafficErrorClassSeen:
			var ts TimestampMeta
			if err := json.Unmarshal(r.Metadata, &ts); err != nil {
				t.Fatalf("unmarshal merged seen: %v", err)
			}
			if ts.FirstSeen != "2026-07-17T10:00:30Z" || ts.LastSeen != "2026-07-17T10:04:59Z" {
				t.Errorf("seen merged = %+v, want MIN first / MAX last", ts)
			}
		}
	}
}
