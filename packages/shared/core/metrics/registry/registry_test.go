package registry

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestCounterEmitsSample(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRegistry(reg)
	c := r.NewCounter("cache.hits_total", []string{"cache"})

	c.With("snapshot_providers").Inc()
	c.With("snapshot_providers").Inc()
	c.With("snapshot_models").Inc()

	samples := r.Collect()
	if got := len(samples); got != 2 {
		t.Fatalf("expected 2 samples (one per label combo), got %d", got)
	}

	byDim := map[string]float64{}
	for _, s := range samples {
		byDim[s.DimensionKey] = s.Value
	}
	if byDim["cache=snapshot_providers"] != 2 {
		t.Errorf("snapshot_providers: got %v, want 2", byDim["cache=snapshot_providers"])
	}
	if byDim["cache=snapshot_models"] != 1 {
		t.Errorf("snapshot_models: got %v, want 1", byDim["cache=snapshot_models"])
	}
}

func TestGaugeEmitsLatestValue(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRegistry(reg)
	g := r.NewGauge("cache.size", []string{"cache"})

	g.With("snapshot_providers").Set(42)
	g.With("snapshot_providers").Set(50) // overwrites; gauge is latest, not cumulative

	samples := r.Collect()
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	if samples[0].Value != 50 {
		t.Errorf("expected 50, got %v", samples[0].Value)
	}
	if samples[0].Kind != KindGauge {
		t.Errorf("expected kind=gauge, got %v", samples[0].Kind)
	}
}

func TestHistogramEmitsBucketsInMetadata(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRegistry(reg)
	h := r.NewHistogram("hook.pipeline_ms", []string{"stage"})

	for _, ms := range []float64{10, 60, 80, 250} {
		h.With("request").Observe(ms)
	}

	samples := r.Collect()
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	s := samples[0]
	if s.Kind != KindHistogram {
		t.Fatalf("kind=%v, want histogram", s.Kind)
	}
	buckets, ok := s.Metadata["buckets"].([]int)
	if !ok {
		t.Fatalf("buckets missing or wrong type: %v", s.Metadata)
	}
	// Spec §6.4: [0,50) [50,100) [100,200) [200,500) [500,1000) [1000,+inf)
	want := []int{1, 2, 0, 1, 0, 0}
	for i := range want {
		if buckets[i] != want[i] {
			t.Errorf("bucket[%d] = %d, want %d (full=%v)", i, buckets[i], want[i], buckets)
		}
	}
}
