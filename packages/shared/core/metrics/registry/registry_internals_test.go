package registry

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// registry.go — pin observable behaviour of every Pin verb

// TestCounterPinAddAccumulatesValueOnVec asserts CounterPin.Add advances the
// underlying Prometheus counter on the exact label-values pin. ToFloat64
// reads from the live registry so a typo in pin wiring would surface as a
// missing or wrong-bucket increment.
func TestCounterPinAddAccumulatesValueOnVec(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRegistry(reg)
	c := r.NewCounter("widget.events_total", []string{"kind"})

	c.With("alpha").Add(2.5)
	c.With("alpha").Add(0.5)
	c.With("beta").Add(7)

	if got := testutil.ToFloat64(c.vec.WithLabelValues("alpha")); got != 3.0 {
		t.Fatalf("alpha cumulative = %v, want 3.0", got)
	}
	if got := testutil.ToFloat64(c.vec.WithLabelValues("beta")); got != 7.0 {
		t.Fatalf("beta cumulative = %v, want 7.0", got)
	}

	// Collect() must also reflect both label combos as separate Samples.
	samples := r.Collect()
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}
	byDim := map[string]float64{}
	for _, s := range samples {
		byDim[s.DimensionKey] = s.Value
	}
	if byDim["kind=alpha"] != 3.0 || byDim["kind=beta"] != 7.0 {
		t.Fatalf("Collect mismatch: %+v", byDim)
	}
}

// TestGaugePinIncDecAddSetCoversAllVerbs asserts every GaugePin verb mutates
// the underlying vec as documented: Set overwrites, Inc/Dec are ±1, Add is
// signed delta.
func TestGaugePinIncDecAddSetCoversAllVerbs(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRegistry(reg)
	g := r.NewGauge("pool.size", []string{"pool"})

	pin := g.With("readers")
	pin.Set(10)
	pin.Inc()    // 11
	pin.Inc()    // 12
	pin.Dec()    // 11
	pin.Add(2.5) // 13.5
	pin.Add(-3)  // 10.5

	if got := testutil.ToFloat64(g.vec.WithLabelValues("readers")); got != 10.5 {
		t.Fatalf("final gauge = %v, want 10.5", got)
	}
}

// TestNewCounterCacheHitReturnsSameInstance verifies the "subsequent calls
// return the cached instance" behaviour: a second NewCounter call with a
// DIFFERENT label list must still return the FIRST registration's labels.
func TestNewCounterCacheHitReturnsSameInstance(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRegistry(reg)
	first := r.NewCounter("cache.hits_total", []string{"cache"})
	second := r.NewCounter("cache.hits_total", []string{"ignored", "other"})
	if first != second {
		t.Fatalf("expected cached instance to be returned, got distinct pointers")
	}
	// The first registration's labels must win.
	if len(second.labels) != 1 || second.labels[0] != "cache" {
		t.Fatalf("cached labels = %v, want [cache]", second.labels)
	}
}

// TestNewGaugeCacheHitReturnsSameInstance mirrors the Counter cache check on
// Gauge.
func TestNewGaugeCacheHitReturnsSameInstance(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRegistry(reg)
	first := r.NewGauge("queue.depth", []string{"q"})
	second := r.NewGauge("queue.depth", []string{"different"})
	if first != second {
		t.Fatalf("expected cached gauge")
	}
}

// TestNewHistogramCacheHitReturnsSameInstance mirrors the cache check on
// Histogram.
func TestNewHistogramCacheHitReturnsSameInstance(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRegistry(reg)
	first := r.NewHistogram("hook.pipeline_ms", []string{"stage"})
	second := r.NewHistogram("hook.pipeline_ms", []string{"different"})
	if first != second {
		t.Fatalf("expected cached histogram")
	}
}

// TestBucketIndexBoundaries pins spec §6.4 mapping at every transition.
func TestBucketIndexBoundaries(t *testing.T) {
	cases := []struct {
		ms   float64
		want int
	}{
		{0, 0},
		{49.999, 0},
		{50, 1},
		{99.999, 1},
		{100, 2},
		{199.999, 2},
		{200, 3},
		{499.999, 3},
		{500, 4},
		{999.999, 4},
		{1000, 5},
		{1e9, 5},
	}
	for _, c := range cases {
		if got := bucketIndex(c.ms); got != c.want {
			t.Errorf("bucketIndex(%v) = %d, want %d", c.ms, got, c.want)
		}
	}
}

// TestHistogramObserveDistributesAcrossAllBuckets exercises every bucket.
func TestHistogramObserveDistributesAcrossAllBuckets(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRegistry(reg)
	h := r.NewHistogram("dist.ms", []string{"k"})
	for _, ms := range []float64{10, 60, 150, 300, 700, 5000} {
		h.With("v").Observe(ms)
	}
	samples := r.Collect()
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	got := samples[0].Metadata["buckets"].([]int)
	want := []int{1, 1, 1, 1, 1, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bucket[%d] = %d, want %d (full=%v)", i, got[i], want[i], got)
		}
	}
}

// TestDtoMetricValueZeroWhenNeitherCounterNorGauge covers the fall-through
// branch in dtoMetric.value.
func TestDtoMetricValueZeroWhenNeitherCounterNorGauge(t *testing.T) {
	d := &dtoMetric{pb: dto.Metric{}} // both Counter and Gauge nil
	if v := d.value(); v != 0 {
		t.Fatalf("empty dtoMetric.value() = %v, want 0", v)
	}
	g := 7.5
	d2 := &dtoMetric{pb: dto.Metric{Gauge: &dto.Gauge{Value: &g}}}
	if v := d2.value(); v != 7.5 {
		t.Fatalf("gauge dtoMetric.value() = %v, want 7.5", v)
	}
}

// TestDtoMetricLabelReturnsEmptyForMissing covers the "not found" branch.
func TestDtoMetricLabelReturnsEmptyForMissing(t *testing.T) {
	name := "foo"
	value := "bar"
	d := &dtoMetric{pb: dto.Metric{
		Label: []*dto.LabelPair{{Name: &name, Value: &value}},
	}}
	if got := d.label("foo"); got != "bar" {
		t.Fatalf("present label = %q, want bar", got)
	}
	if got := d.label("missing"); got != "" {
		t.Fatalf("missing label = %q, want empty", got)
	}
}

// dedup.go — Tick early-return when record still inside window

// TestDedupTickSkipsRecordsStillInsideWindow exercises the continue branch.
func TestDedupTickSkipsRecordsStillInsideWindow(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	d := NewDedup(clock, 60*time.Second, 10)

	d.Submit(DiagEvent{MessageHash: "h1"})
	d.Submit(DiagEvent{MessageHash: "h1"}) // bump repeat to 2

	if got := d.Tick(); len(got) != 0 {
		t.Fatalf("Tick before window expiry must return nothing, got %d events", len(got))
	}

	now = now.Add(61 * time.Second)
	emitted := d.Tick()
	if len(emitted) != 1 || emitted[0].RepeatCount != 2 {
		t.Fatalf("Tick after window must emit summary RepeatCount=2, got %+v", emitted)
	}
}

// TestDedupTickRemovesSingleOccurrenceWithoutEmitting covers the path where
// a record's window expires with RepeatCount == 1.
func TestDedupTickRemovesSingleOccurrenceWithoutEmitting(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	d := NewDedup(clock, 60*time.Second, 10)

	d.Submit(DiagEvent{MessageHash: "lonely"})
	now = now.Add(61 * time.Second)
	if emitted := d.Tick(); len(emitted) != 0 {
		t.Fatalf("Tick on single-occurrence must emit nothing, got %d", len(emitted))
	}
	out := d.Submit(DiagEvent{MessageHash: "lonely"})
	if len(out) != 1 || out[0].RepeatCount != 1 {
		t.Fatalf("post-Tick resubmit must be a fresh first-occurrence, got %+v", out)
	}
}

// histogram concurrency — Observe must not race

// TestHistogramObserveIsRaceFreeAndCountsAllSamples drives concurrent Observe.
func TestHistogramObserveIsRaceFreeAndCountsAllSamples(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRegistry(reg)
	h := r.NewHistogram("conc.ms", []string{"k"})

	const N = 200
	var done int32
	for i := range N {
		go func(v float64) {
			h.With("v").Observe(v)
			atomic.AddInt32(&done, 1)
		}(float64(i))
	}
	for atomic.LoadInt32(&done) < N {
		runtime.Gosched()
	}

	samples := r.Collect()
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	buckets := samples[0].Metadata["buckets"].([]int)
	total := 0
	for _, b := range buckets {
		total += b
	}
	if total != N {
		t.Fatalf("bucket total = %d, want %d (full=%v)", total, N, buckets)
	}
}
