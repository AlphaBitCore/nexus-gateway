package alerteval

import (
	"math"
	"testing"
	"time"
)

func TestNewSampleWindow_PanicsOnZeroCap(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for cap=0")
		}
	}()
	_ = NewSampleWindow(0)
}

func TestNewSampleWindow_PanicsOnNegativeCap(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for cap=-1")
		}
	}()
	_ = NewSampleWindow(-1)
}

func TestSampleWindow_EmptyPercentileReturnsZero(t *testing.T) {
	w := NewSampleWindow(10)
	v, n := w.Percentile(time.Minute, time.Now(), 95)
	if v != 0 || n != 0 {
		t.Errorf("empty window: got val=%v count=%d, want 0/0", v, n)
	}
}

func TestSampleWindow_PercentileNearestRank(t *testing.T) {
	w := NewSampleWindow(100)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Add 1..100 with strictly-increasing timestamps within the lookback.
	for i := 1; i <= 100; i++ {
		w.Add(now.Add(time.Duration(i)*time.Millisecond), float64(i))
	}
	// later "now" so all samples are inside lookback.
	queryNow := now.Add(time.Second)

	// Nearest-rank: idx = (p/100) * (len-1). For len=100 and p=95: 0.95*99=94.05 → idx 94 → value 95.
	v, n := w.Percentile(time.Second, queryNow, 95)
	if n != 100 {
		t.Errorf("count: %d", n)
	}
	if v != 95 {
		t.Errorf("p95: got %v want 95", v)
	}
	// p50: 0.5*99 = 49.5 → idx 49 → value 50
	v50, _ := w.Percentile(time.Second, queryNow, 50)
	if v50 != 50 {
		t.Errorf("p50: %v", v50)
	}
	// p0 → idx 0 → value 1 (min)
	vMin, _ := w.Percentile(time.Second, queryNow, 0)
	if vMin != 1 {
		t.Errorf("p0: %v", vMin)
	}
	// p100 → idx 99 → value 100 (max)
	vMax, _ := w.Percentile(time.Second, queryNow, 100)
	if vMax != 100 {
		t.Errorf("p100: %v", vMax)
	}
}

func TestSampleWindow_PercentileClampsOutOfRange(t *testing.T) {
	w := NewSampleWindow(10)
	now := time.Now()
	for i := 1; i <= 10; i++ {
		w.Add(now, float64(i))
	}
	// p=-5 should clamp to 0 → min
	vNeg, _ := w.Percentile(time.Minute, now, -5)
	if vNeg != 1 {
		t.Errorf("clamp negative: %v", vNeg)
	}
	// p=200 should clamp to 100 → max
	vBig, _ := w.Percentile(time.Minute, now, 200)
	if vBig != 10 {
		t.Errorf("clamp 200: %v", vBig)
	}
}

func TestSampleWindow_LookbackFiltersOldSamples(t *testing.T) {
	w := NewSampleWindow(20)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// "old" samples 1..5 outside the 10s lookback window
	for i := 1; i <= 5; i++ {
		w.Add(now.Add(-60*time.Second).Add(time.Duration(i)*time.Millisecond), float64(i))
	}
	// "fresh" samples 100..104 inside the lookback window
	for i := range 5 {
		w.Add(now.Add(-1*time.Second).Add(time.Duration(i)*time.Millisecond), float64(100+i))
	}
	v, n := w.Percentile(10*time.Second, now, 50)
	if n != 5 {
		t.Errorf("lookback filter count: %d, want 5 (fresh only)", n)
	}
	// median of {100,101,102,103,104} via nearest-rank: idx = 0.5*4 = 2 → 102
	if v != 102 {
		t.Errorf("p50 of fresh: %v", v)
	}
}

func TestSampleWindow_OverflowOverwritesOldest(t *testing.T) {
	// Ring of cap=3 receiving 6 samples — only last 3 remain.
	w := NewSampleWindow(3)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 6; i++ {
		w.Add(now.Add(time.Duration(i)*time.Millisecond), float64(i))
	}
	queryNow := now.Add(time.Second)
	v, n := w.Percentile(time.Second, queryNow, 0)
	if n != 3 {
		t.Errorf("count after overflow: %d, want 3", n)
	}
	// Min of {4,5,6} = 4
	if v != 4 {
		t.Errorf("min after overflow: got %v want 4 (samples 1,2,3 should be evicted)", v)
	}
}

func TestSampleWindow_ConcurrentAddSafe(t *testing.T) {
	// Race-detector check: 4 goroutines hammer Add + Percentile.
	w := NewSampleWindow(1000)
	done := make(chan struct{})
	now := time.Now()
	for g := range 4 {
		go func(g int) {
			for i := range 250 {
				w.Add(now.Add(time.Duration(g*250+i)*time.Millisecond), float64(g*250+i))
			}
			done <- struct{}{}
		}(g)
	}
	go func() {
		// Concurrent percentile reads.
		for range 100 {
			_, _ = w.Percentile(time.Minute, now.Add(2*time.Minute), 95)
		}
	}()
	for range 4 {
		<-done
	}
}

// --- Runtime ---------------------------------------------------------------

func TestRuntime_NewExposesRuleID(t *testing.T) {
	rt := NewRuntime("rule-x", time.Now())
	if rt.RuleID() != "rule-x" {
		t.Errorf("RuleID: %q", rt.RuleID())
	}
}

func TestRuntime_WindowLazyCreateAndReuse(t *testing.T) {
	rt := NewRuntime("r", time.Now())
	w1 := rt.Window("tk", 60)
	w2 := rt.Window("tk", 999) // capSeconds change ignored — same instance returned
	if w1 != w2 {
		t.Errorf("Window should return same instance for same key, got %p vs %p", w1, w2)
	}
	if w1 == nil {
		t.Fatal("Window returned nil")
	}
}

func TestRuntime_SampleWindowLazyCreate(t *testing.T) {
	rt := NewRuntime("r", time.Now())
	w1 := rt.SampleWindow("tk", 50)
	w2 := rt.SampleWindow("tk", 999)
	if w1 != w2 {
		t.Errorf("SampleWindow should return same instance for same key")
	}
}

func TestRuntime_EvictRemovesAll(t *testing.T) {
	rt := NewRuntime("r", time.Now())
	_ = rt.Window("tk", 10)
	_ = rt.SampleWindow("tk", 10)
	rt.SetCooldown("tk", time.Now().Add(time.Minute))
	rt.EvictWindow("tk")
	if len(rt.Targets()) != 0 {
		t.Errorf("Targets after evict: %+v", rt.Targets())
	}
	if len(rt.SampleTargets()) != 0 {
		t.Errorf("SampleTargets after evict: %+v", rt.SampleTargets())
	}
	if rt.HasFired("tk") {
		t.Errorf("HasFired should be false after evict")
	}
}

func TestRuntime_CooldownWindow(t *testing.T) {
	rt := NewRuntime("r", time.Now())
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := t0.Add(5 * time.Minute)
	rt.SetCooldown("tk", until)
	if !rt.IsCooldown("tk", t0.Add(1*time.Minute)) {
		t.Errorf("1m after set should still be in cooldown")
	}
	if !rt.IsCooldown("tk", t0.Add(5*time.Minute).Add(-1*time.Nanosecond)) {
		t.Errorf("instant before expiry should still be in cooldown")
	}
	if rt.IsCooldown("tk", t0.Add(5*time.Minute)) {
		t.Errorf("at expiry exactly: should be out of cooldown (Before is strict)")
	}
	if rt.IsCooldown("tk", t0.Add(10*time.Minute)) {
		t.Errorf("well past expiry should be out of cooldown")
	}
}

func TestRuntime_IsCooldownUnsetTarget(t *testing.T) {
	rt := NewRuntime("r", time.Now())
	if rt.IsCooldown("never-set", time.Now()) {
		t.Errorf("unset key should not be in cooldown")
	}
	if rt.HasFired("never-set") {
		t.Errorf("unset key HasFired should be false")
	}
}

func TestRuntime_TargetsSnapshot(t *testing.T) {
	rt := NewRuntime("r", time.Now())
	for _, k := range []string{"a", "b", "c"} {
		_ = rt.Window(k, 10)
	}
	got := rt.Targets()
	if len(got) != 3 {
		t.Errorf("Targets: %+v", got)
	}
}

func TestRuntime_WarmupRemaining(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rt := NewRuntime("r", start)
	if rt.WarmupRemaining(0, start) != 0 {
		t.Errorf("minWarmup=0 disables gate")
	}
	if rt.WarmupRemaining(-5, start) != 0 {
		t.Errorf("negative minWarmup disables gate")
	}
	if got := rt.WarmupRemaining(60, start.Add(10*time.Second)); got != 50 {
		t.Errorf("10s in 60s warmup: got %d, want 50", got)
	}
	if got := rt.WarmupRemaining(60, start.Add(60*time.Second)); got != 0 {
		t.Errorf("at warmup expiry: got %d, want 0", got)
	}
	if got := rt.WarmupRemaining(60, start.Add(120*time.Second)); got != 0 {
		t.Errorf("past warmup: got %d, want 0", got)
	}
}

// --- Window additional boundary cases (existing tests cover happy path) ----

func TestWindow_PanicsOnZeroCap(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = NewWindow(0)
}

func TestWindow_SumZeroLookbackReturnsZero(t *testing.T) {
	w := NewWindow(10)
	now := time.Now()
	w.Add(now, 5, 5)
	a, b := w.Sum(0, now)
	if a != 0 || b != 0 {
		t.Errorf("Sum(0, _) should return zeros: %v %v", a, b)
	}
}

func TestWindow_LookbackLargerThanCapacityIsClamped(t *testing.T) {
	// Window has cap=5 seconds; asking for 60s lookback must clamp.
	w := NewWindow(5)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		w.Add(t0.Add(time.Duration(i)*time.Second), 1, 0)
	}
	// 60s lookback ending at t0+4 → clamped to 5 buckets → sum = 5.
	a, _ := w.Sum(60*time.Second, t0.Add(4*time.Second))
	if a != 5 {
		t.Errorf("oversized lookback: a=%v want 5", a)
	}
}

func TestWindow_AdvanceClearsEntireRingWhenGapHuge(t *testing.T) {
	w := NewWindow(10)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		w.Add(t0.Add(time.Duration(i)*time.Second), 100, 100)
	}
	// Jump 1 hour ahead — entire ring must reset.
	tFuture := t0.Add(time.Hour)
	a, b := w.Sum(10*time.Second, tFuture)
	if a != 0 || b != 0 {
		t.Errorf("after huge gap, ring should be cleared: a=%v b=%v", a, b)
	}
}

func TestWindow_ConcurrentAddRace(t *testing.T) {
	w := NewWindow(60)
	now := time.Now()
	done := make(chan struct{})
	for range 4 {
		go func() {
			for i := range 100 {
				w.Add(now.Add(time.Duration(i)*time.Second/1000), 1, 1)
			}
			done <- struct{}{}
		}()
	}
	go func() {
		for range 100 {
			_, _ = w.Sum(time.Minute, now)
		}
	}()
	for range 4 {
		<-done
	}
}

// Sanity: percentile of a single sample is the sample value regardless of p.
func TestSampleWindow_SingleSample(t *testing.T) {
	w := NewSampleWindow(1)
	now := time.Now()
	w.Add(now, 42)
	for _, p := range []float64{0, 50, 95, 100} {
		v, n := w.Percentile(time.Minute, now.Add(time.Second), p)
		if n != 1 || v != 42 || math.IsNaN(v) {
			t.Errorf("single sample p%v: v=%v n=%d", p, v, n)
		}
	}
}
