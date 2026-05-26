package rollup

import (
	"testing"
	"time"
)

func TestRollupMerge1h_Identity(t *testing.T) {
	j := NewRollupMerge1h(nil, 5*time.Minute, testLogger())
	if j.ID() != "merge-1h" {
		t.Errorf("ID = %q, want merge-1h", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != 5*time.Minute {
		t.Errorf("Interval = %v, want 5m", j.Interval())
	}
}

func TestRollupMerge1d_Identity(t *testing.T) {
	j := NewRollupMerge1d(nil, time.Hour, testLogger())
	if j.ID() != "merge-1d" {
		t.Errorf("ID = %q, want merge-1d", j.ID())
	}
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h", j.Interval())
	}
}

func TestRollupMerge1mo_Identity(t *testing.T) {
	j := NewRollupMerge1mo(nil, 24*time.Hour, testLogger())
	if j.ID() != "merge-1mo" {
		t.Errorf("ID = %q, want merge-1mo", j.ID())
	}
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h", j.Interval())
	}
}

func TestRollupMerge_IntervalDefaults(t *testing.T) {
	if got := NewRollupMerge1h(nil, 0, testLogger()).Interval(); got != 5*time.Minute {
		t.Errorf("merge-1h default = %v, want 5m", got)
	}
	if got := NewRollupMerge1d(nil, 0, testLogger()).Interval(); got != time.Hour {
		t.Errorf("merge-1d default = %v, want 1h", got)
	}
	if got := NewRollupMerge1mo(nil, 0, testLogger()).Interval(); got != 24*time.Hour {
		t.Errorf("merge-1mo default = %v, want 24h", got)
	}
}

func TestPickColdStartWatermark(t *testing.T) {
	now := time.Date(2026, 4, 21, 10, 0, 0, 0, time.UTC)
	lookback := 6 * time.Hour
	bucket := time.Hour

	tests := []struct {
		name           string
		earliestSource time.Time
		haveSource     bool
		want           time.Time
	}{
		{
			name:       "no source data — falls back to now - initLookback",
			haveSource: false,
			want:       time.Date(2026, 4, 21, 4, 0, 0, 0, time.UTC),
		},
		{
			name:           "source recent (within lookback) — uses default lookback, not source",
			earliestSource: time.Date(2026, 4, 21, 7, 15, 0, 0, time.UTC),
			haveSource:     true,
			want:           time.Date(2026, 4, 21, 4, 0, 0, 0, time.UTC),
		},
		{
			name:           "source older than lookback — backfills from earliest source minus one bucket",
			earliestSource: time.Date(2026, 4, 18, 8, 17, 0, 0, time.UTC),
			haveSource:     true,
			want:           time.Date(2026, 4, 18, 7, 0, 0, 0, time.UTC),
		},
		{
			// When source's earliest bucket equals the default lookback point, we must
			// still backfill from fromSource (one bucket earlier) so the loop's first
			// iteration processes the 04:00 bucket instead of skipping to 05:00.
			name:           "source exactly at lookback boundary — backfills one bucket earlier",
			earliestSource: time.Date(2026, 4, 21, 4, 0, 0, 0, time.UTC),
			haveSource:     true,
			want:           time.Date(2026, 4, 21, 3, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pickColdStartWatermark(now, lookback, bucket, tc.earliestSource, tc.haveSource)
			if !got.Equal(tc.want) {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNextMonth(t *testing.T) {
	cases := []struct {
		in, want time.Time
	}{
		{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 1, 15, 12, 30, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := nextMonth(c.in); !got.Equal(c.want) {
			t.Errorf("nextMonth(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
