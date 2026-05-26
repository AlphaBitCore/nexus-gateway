package streamcache

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetrics_NilReceiverIsNoOp asserts that every Metrics helper is
// safe on a nil receiver — the public contract documented in metrics.go.
// The whole point is that callers never have to nil-guard.
func TestMetrics_NilReceiverIsNoOp(t *testing.T) {
	var m *Metrics
	// None of these may panic.
	m.RecordLookup("hit")
	m.RecordWrite("stream", "ok", 123)
	m.IncBrokerActive()
	m.DecBrokerActive()
	m.IncSubscribers()
	m.DecSubscribers()
	m.IncReplayChunks()
}

// TestNewMetrics_RegistersAllInstruments asserts that NewMetrics
// registers six instruments under the nexus_aigw/cache namespace and
// returns a fully-wired *Metrics. Uses an isolated registry so the
// test cannot collide with other suites or DefaultRegisterer state.
func TestNewMetrics_RegistersAllInstruments(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
	if m.LookupsTotal == nil || m.WritesTotal == nil || m.BrokerSubscribers == nil ||
		m.BrokerActive == nil || m.ReplayChunks == nil || m.EntryBytes == nil {
		t.Fatal("NewMetrics returned partial *Metrics")
	}

	// CounterVec / Counter instruments are lazily exposed by Gather()
	// only after a first observation, so prime each one before
	// gathering.
	m.RecordLookup("hit")
	m.RecordWrite("stream", "ok", 1)
	m.IncReplayChunks()
	m.IncBrokerActive()
	m.IncSubscribers()

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	want := map[string]bool{
		"nexus_aigw_cache_lookups_total":       false,
		"nexus_aigw_cache_writes_total":        false,
		"nexus_aigw_cache_broker_subscribers":  false,
		"nexus_aigw_cache_broker_active":       false,
		"nexus_aigw_cache_replay_chunks_total": false,
		"nexus_aigw_cache_entry_bytes":         false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %s not registered", name)
		}
	}
}

// TestNewMetrics_NilRegistererUsesDefault — exercises the
// `reg == nil → DefaultRegisterer` branch. Cleanup is critical so a
// concurrent test run isn't left with stale registrations.
func TestNewMetrics_NilRegistererUsesDefault(t *testing.T) {
	// Save and restore the default registerer/gatherer around the test
	// so the registration is isolated.
	oldReg, oldGath := prometheus.DefaultRegisterer, prometheus.DefaultGatherer
	tmp := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = tmp
	prometheus.DefaultGatherer = tmp
	t.Cleanup(func() {
		prometheus.DefaultRegisterer = oldReg
		prometheus.DefaultGatherer = oldGath
	})

	m := NewMetrics(nil)
	if m == nil {
		t.Fatal("NewMetrics(nil) returned nil")
	}
	// Force exposure of all instruments so Gather observes them.
	m.RecordLookup("hit")
	m.RecordWrite("stream", "ok", 1)
	m.IncReplayChunks()
	m.IncBrokerActive()
	m.IncSubscribers()

	mfs, err := tmp.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(mfs) == 0 {
		t.Fatal("DefaultRegisterer fallback registered nothing")
	}
}

// TestMetrics_RecordLookup_IncrementsCorrectLabel asserts observable
// behaviour: each result label increments only its own counter.
func TestMetrics_RecordLookup_IncrementsCorrectLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.RecordLookup("hit")
	m.RecordLookup("hit")
	m.RecordLookup("miss")

	if got := testutil.ToFloat64(m.LookupsTotal.WithLabelValues("hit")); got != 2 {
		t.Errorf("hit count: got %v want 2", got)
	}
	if got := testutil.ToFloat64(m.LookupsTotal.WithLabelValues("miss")); got != 1 {
		t.Errorf("miss count: got %v want 1", got)
	}
	if got := testutil.ToFloat64(m.LookupsTotal.WithLabelValues("disabled")); got != 0 {
		t.Errorf("disabled count: got %v want 0", got)
	}
}

// TestMetrics_RecordWrite_OkObservesHistogram exercises the
// `reason == "ok" && bytes > 0` branch. The histogram count must
// increment only on successful writes with a positive byte count.
func TestMetrics_RecordWrite_OkObservesHistogram(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.RecordWrite("stream", "ok", 4096)
	m.RecordWrite("stream", "ok", 8192)
	// reason != "ok" → counter ticks but histogram is skipped
	m.RecordWrite("stream", "too_large", 9999999)
	// reason == "ok" but bytes == 0 → histogram skipped
	m.RecordWrite("response", "ok", 0)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var hist *uint64
	var writes float64
	for _, mf := range mfs {
		switch mf.GetName() {
		case "nexus_aigw_cache_entry_bytes":
			// Sum the histogram count across all metric series.
			for _, mtr := range mf.GetMetric() {
				c := mtr.GetHistogram().GetSampleCount()
				if hist == nil {
					hist = new(uint64)
				}
				*hist += c
			}
		case "nexus_aigw_cache_writes_total":
			for _, mtr := range mf.GetMetric() {
				writes += mtr.GetCounter().GetValue()
			}
		}
	}
	if hist == nil || *hist != 2 {
		t.Errorf("entry_bytes sample count: got %v want 2", hist)
	}
	if writes != 4 {
		t.Errorf("writes_total sum: got %v want 4", writes)
	}
}

// TestMetrics_GaugeHelpers_AdjustGauges exercises Inc/Dec on the two
// gauges. The 50% baseline was the nil-receiver branch only; this
// nails the non-nil branches.
func TestMetrics_GaugeHelpers_AdjustGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.IncBrokerActive()
	m.IncBrokerActive()
	m.DecBrokerActive()
	if got := testutil.ToFloat64(m.BrokerActive); got != 1 {
		t.Errorf("broker_active: got %v want 1", got)
	}

	m.IncSubscribers()
	m.IncSubscribers()
	m.IncSubscribers()
	m.DecSubscribers()
	if got := testutil.ToFloat64(m.BrokerSubscribers); got != 2 {
		t.Errorf("broker_subscribers: got %v want 2", got)
	}

	m.IncReplayChunks()
	m.IncReplayChunks()
	m.IncReplayChunks()
	if got := testutil.ToFloat64(m.ReplayChunks); got != 3 {
		t.Errorf("replay_chunks: got %v want 3", got)
	}
}

// TestMetrics_NamespacePrefix asserts every metric name carries the
// expected nexus_aigw_cache_ prefix — observable contract for prod
// scrape configs.
func TestMetrics_NamespacePrefix(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	// Force exposure.
	m.RecordLookup("hit")
	m.RecordWrite("stream", "ok", 1)
	m.IncReplayChunks()
	m.IncBrokerActive()
	m.IncSubscribers()

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(mfs) == 0 {
		t.Fatal("no metrics registered")
	}
	for _, mf := range mfs {
		if !strings.HasPrefix(mf.GetName(), "nexus_aigw_cache_") {
			t.Errorf("metric %s missing nexus_aigw_cache_ prefix", mf.GetName())
		}
	}
}
