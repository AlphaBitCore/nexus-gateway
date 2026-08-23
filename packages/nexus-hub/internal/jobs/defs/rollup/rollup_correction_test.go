package rollup

import (
	"context"
	"testing"
	"time"
)

// fakeCorrectionRollup / fakeCorrectionMerge record which buckets the
// correction pass drove, so a test can assert the window it actually walked
// rather than the number it was configured with.
type fakeCorrectionRollup struct{ buckets []time.Time }

func (f *fakeCorrectionRollup) processBucket(_ context.Context, bucketStart time.Time, _ bool) error {
	f.buckets = append(f.buckets, bucketStart)
	return nil
}

type fakeCorrectionMerge struct{ buckets []time.Time }

func (f *fakeCorrectionMerge) mergeBucket(_ context.Context, bucketStart, _ time.Time, _ bool) error {
	f.buckets = append(f.buckets, bucketStart)
	return nil
}

func TestRollupCorrection_Identity(t *testing.T) {
	r5m := NewRollup5m(nil, 0, testLogger(), false)
	m1h := NewRollupMerge1h(nil, 0, testLogger())
	m1d := NewRollupMerge1d(nil, 0, testLogger())
	m1mo := NewRollupMerge1mo(nil, 0, testLogger())

	j := NewRollupCorrection(nil, r5m, m1h, m1d, m1mo, 7, 0, 24*time.Hour, testLogger())
	if j.ID() != "rollup-correction" {
		t.Errorf("ID = %q, want rollup-correction", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h", j.Interval())
	}
	if j.lookbackDays != 7 {
		t.Errorf("lookbackDays = %d, want 7", j.lookbackDays)
	}
}

// TestRollupCorrection_ClampToRetention pins the window away from the buckets
// the retention job is entitled to purge. At the shipped defaults the two
// overlapped: retention deletes metric_rollup_5m rows older than
// now-7d (a wall-clock instant partway through that UTC day) while a 7-day
// correction window starts at that day's midnight, so both jobs wrote the same
// buckets on the same tick. That contention deadlocked the 2026-08-07
// correction run and, by leaving the watermark unadvanced, stalled vendor-bill
// reconciliation behind it.
func TestRollupCorrection_ClampToRetention(t *testing.T) {
	tests := []struct {
		name          string
		lookbackDays  int
		retentionDays int
		want          int
	}{
		{
			name:          "shipped defaults no longer reach the purge boundary",
			lookbackDays:  7,
			retentionDays: 7,
			want:          6,
		},
		{
			name:          "a window already inside the horizon is left alone",
			lookbackDays:  3,
			retentionDays: 30,
			want:          3,
		},
		{
			name:          "retention disabled means nothing can race the window",
			lookbackDays:  30,
			retentionDays: 0,
			want:          30,
		},
		{
			name:          "yesterday is rebuilt even under a retention too short to be safe",
			lookbackDays:  7,
			retentionDays: 1,
			want:          1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := NewRollupCorrection(nil, nil, nil, nil, nil, tc.lookbackDays, tc.retentionDays, 24*time.Hour, testLogger())
			if got := j.clampToRetention(j.lookbackDays); got != tc.want {
				t.Errorf("clampToRetention(%d) with %d-day 5m retention = %d, want %d",
					tc.lookbackDays, tc.retentionDays, got, tc.want)
			}
		})
	}
}

// TestRollupCorrection_ClampedWindowDrivesTheRun proves the clamp reaches the
// aggregation and is not merely a computed number: with a 7-day request against
// 7-day retention, the oldest day touched must be 6 days back, not 7.
func TestRollupCorrection_ClampedWindowDrivesTheRun(t *testing.T) {
	r5m := &fakeCorrectionRollup{}
	m1h := &fakeCorrectionMerge{}
	m1d := &fakeCorrectionMerge{}
	m1mo := &fakeCorrectionMerge{}

	now := time.Date(2026, 8, 7, 2, 56, 49, 0, time.UTC)
	j := NewRollupCorrection(nil, nil, nil, nil, nil, 7, 7, 24*time.Hour, testLogger())
	if err := runCorrection(context.Background(), r5m, m1h, m1d, m1mo,
		j.clampToRetention(j.lookbackDays), now, testLogger()); err != nil {
		t.Fatalf("runCorrection: %v", err)
	}

	// 2026-07-31 is the day retention's cutoff (2026-07-31T02:56:49Z) cuts
	// through, and is exactly the day the deadlock struck.
	purgeContested := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	for _, got := range m1d.buckets {
		if got.Equal(purgeContested) {
			t.Fatalf("rebuilt %s, the day retention is concurrently purging", got.Format("2006-01-02"))
		}
	}
	oldest := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if len(m1d.buckets) == 0 || !m1d.buckets[0].Equal(oldest) {
		t.Fatalf("oldest rebuilt day = %v, want %v", m1d.buckets, oldest)
	}
	if len(m1d.buckets) != 6 {
		t.Fatalf("rebuilt %d days, want the clamped 6", len(m1d.buckets))
	}
}

func TestRollupCorrection_IntervalDefault(t *testing.T) {
	j := NewRollupCorrection(nil, nil, nil, nil, nil, 0, 0, 0, testLogger())
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h default", j.Interval())
	}
	// lookbackDays <= 0 falls back to the package default.
	if j.lookbackDays != correctionLookbackDays {
		t.Errorf("lookbackDays = %d, want default %d", j.lookbackDays, correctionLookbackDays)
	}
}
