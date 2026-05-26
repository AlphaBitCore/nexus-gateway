package aggregators

import (
	"testing"
	"time"

	alerteval "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

func TestEvalCountInWindow_FiresAtThreshold(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("ip:1.2.3.4", 300)
	for range 25 {
		w.Add(now, 1, 0)
	}
	d := EvalCountInWindow(rt, "ip:1.2.3.4", 300, 20, now, "test message")
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
}

func TestEvalCountInWindow_NoFireBelowThreshold(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("ip:1.2.3.4", 300)
	for range 5 {
		w.Add(now, 1, 0)
	}
	d := EvalCountInWindow(rt, "ip:1.2.3.4", 300, 20, now, "test message")
	if d != nil {
		t.Errorf("expected no decision (5 < 20), got %+v", d)
	}
}

func TestEvalRatioInWindow_FiresAboveThreshold(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("thing:foo", 300)
	for range 20 {
		// 10 numerator, 20 denominator → 50%, > 5%
		w.Add(now, 0, 1)
	}
	for range 10 {
		w.Add(now, 1, 1)
	}
	d := EvalRatioInWindow(rt, "thing:foo", 300, 5, 20, now, "test")
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
}

func TestEvalRatioInWindow_NoFireBelowMinSamples(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("thing:foo", 300)
	// 9 events all matching — 100% but below minSamples=20
	for range 9 {
		w.Add(now, 1, 1)
	}
	d := EvalRatioInWindow(rt, "thing:foo", 300, 5, 20, now, "test")
	if d != nil {
		t.Errorf("expected no decision (samples<min), got %+v", d)
	}
}

func TestEvalSumInWindow_FiresAtThreshold(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("vk:abc", 300)
	w.Add(now, 12.50, 1)
	d := EvalSumInWindow(rt, "vk:abc", 300, 10.0, now, "cost spike")
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
}

func TestEvalCompareToBaseline_FiresOnSpike(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("vk:abc", 3900)
	// Baseline 5min ago: 10 req per ~5min window over the past hour
	for i := range 12 {
		w.Add(now.Add(-time.Duration(i*5+10)*time.Minute), 1, 0) // ~120 baseline events spread over baseline window
	}
	// Burst in the last 5 min: 200 req
	for i := range 200 {
		w.Add(now.Add(-time.Duration(i)*time.Second), 1, 0)
	}
	d := EvalCompareToBaseline(rt, "vk:abc", 300, 3600, 10, 50, now, "vk spike")
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
}
