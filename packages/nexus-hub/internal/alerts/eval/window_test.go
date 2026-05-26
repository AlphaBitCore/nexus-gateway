package alerteval

import (
	"testing"
	"time"
)

func TestWindow_AddAndSum_SingleBucket(t *testing.T) {
	w := NewWindow(60)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w.Add(now, 1, 1)
	w.Add(now, 0, 1)
	a, b := w.Sum(60*time.Second, now)
	if a != 1 || b != 2 {
		t.Errorf("Sum = (%v, %v), want (1, 2)", a, b)
	}
}

func TestWindow_AdvancesAndDropsOldBuckets(t *testing.T) {
	w := NewWindow(10)
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w.Add(t0, 1, 1)
	t1 := t0.Add(11 * time.Second)
	w.Add(t1, 5, 5)
	a, b := w.Sum(10*time.Second, t1)
	if a != 5 || b != 5 {
		t.Errorf("after eviction Sum = (%v, %v), want (5, 5)", a, b)
	}
}

func TestWindow_SumBoundedByLookback(t *testing.T) {
	w := NewWindow(60)
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := range 30 {
		w.Add(t0.Add(time.Duration(i)*time.Second), 1, 0)
	}
	a, _ := w.Sum(10*time.Second, t0.Add(29*time.Second))
	if a != 10 {
		t.Errorf("last-10s Sum = %v, want 10", a)
	}
}

func TestWindow_OutOfOrderTooOld_Dropped(t *testing.T) {
	w := NewWindow(10)
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w.Add(t0, 1, 0)
	// Try to add a bucket older than the ring can hold.
	w.Add(t0.Add(-30*time.Second), 99, 0)
	a, _ := w.Sum(10*time.Second, t0)
	if a != 1 {
		t.Errorf("out-of-order add should have been dropped; Sum = %v want 1", a)
	}
}
