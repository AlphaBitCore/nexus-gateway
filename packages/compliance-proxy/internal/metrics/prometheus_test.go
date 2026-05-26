// Tests for the metrics package: exercise Register's two branches (nil
// no-op and full bind) and pin the observable contract that each of the
// ten package-level handles is non-nil, points at the correctly-named
// Prometheus instrument, and produces samples through the underlying
// CounterVec/GaugeVec/HistogramVec.
//
// Pattern mirrors packages/compliance-proxy/internal/config/cache/metrics_test.go
// and packages/compliance-proxy/internal/audit/coverage_gaps_test.go: use a
// fresh *prometheus.Registry per call to avoid cross-test registration
// collisions, then Gather() to assert behaviour by Prometheus name.
package metrics

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRegister_NilRegistryIsNoOp guards the early-return at the top of
// Register — a nil registry must leave every package-level handle as it
// was. This is the contract callers depend on when opting out of metrics
// (e.g. unit tests that construct cert.Cache without wiring an opsmetrics
// registry).
func TestRegister_NilRegistryIsNoOp(t *testing.T) {
	// Pre-seed all ten handles via a real Register call so we can detect
	// any mutation by the subsequent Register(nil).
	Register(registry.NewRegistry(prometheus.NewRegistry()))

	before := snapshotHandles()
	Register(nil)
	after := snapshotHandles()

	for name, beforePtr := range before {
		if after[name] != beforePtr {
			t.Errorf("Register(nil) mutated handle %q (before=%p after=%p)",
				name, beforePtr, after[name])
		}
	}
}

// TestRegister_BindsAllTenHandles asserts that a non-nil registry binds
// every one of the ten package-level handles. This is the observable
// contract main.go relies on at boot — a missed handle would surface as
// a nil-deref panic the first time the data plane tried to record a
// metric.
func TestRegister_BindsAllTenHandles(t *testing.T) {
	Register(registry.NewRegistry(prometheus.NewRegistry()))

	for name, ptr := range snapshotHandles() {
		if ptr == nil {
			t.Errorf("handle %q is nil after Register; main.go would panic on first emit", name)
		}
	}
}

// TestRegister_RegistersPrometheusInstrumentsByName verifies that each
// dotted opsmetrics name maps to the expected snake_case Prometheus
// instrument and that the instrument is actually present in the
// underlying prometheus.Registerer (so /metrics scrapes return it).
//
// This pins the spec §6.3 catalog mapping that ops dashboards depend on
// (e.g. dashboard PromQL `tunnels_active` must keep working after any
// rename of `ConnectionsActive`).
func TestRegister_RegistersPrometheusInstrumentsByName(t *testing.T) {
	promReg := prometheus.NewRegistry()
	Register(registry.NewRegistry(promReg))

	// prometheus.*Vec only surfaces in Gather() output once at least one
	// labelled cell exists. Touch every instrument once so the assertion
	// below validates the name mapping rather than the lazy-init quirk.
	ConnectionsActive.With().Set(0)
	ConnectionsTotal.With("ok").Add(0)
	CertCacheHits.With("L1").Add(0)
	CertCacheMisses.With().Add(0)
	CertCacheSize.With().Set(0)
	CertSignMs.With().Observe(0)
	CertPrewarmMs.With().Set(0)
	RedisAvailable.With().Set(0)
	PinningPassthroughTotal.With("allow").Add(0)
	KillSwitchActive.With().Set(0)

	want := []string{
		"nexus_tunnels_active",
		"nexus_tunnels_total",
		"nexus_cert_cache_hits_total",
		"nexus_cert_cache_misses_total",
		"nexus_cert_cache_size",
		"nexus_cert_sign_ms",
		"nexus_cert_prewarm_duration_ms",
		"nexus_redis_available",
		"nexus_pinning_passthrough_total",
		"nexus_killswitch_active",
	}

	families, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]bool{}
	for _, f := range families {
		got[f.GetName()] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("expected Prometheus instrument %q to be registered; gathered=%v",
				name, sortedKeys(got))
		}
	}
}

// TestRegister_GaugesAreObservableAfterBind drives each gauge handle
// through a Set/Inc/Dec cycle to confirm the binding actually points at
// a working *prometheus.GaugeVec. A handle that bound but failed to
// register would still be non-nil yet panic on first Set.
func TestRegister_GaugesAreObservableAfterBind(t *testing.T) {
	promReg := prometheus.NewRegistry()
	Register(registry.NewRegistry(promReg))

	ConnectionsActive.With().Set(7)
	CertCacheSize.With().Set(123)
	CertPrewarmMs.With().Set(45)
	RedisAvailable.With().Set(1)
	KillSwitchActive.With().Set(1)

	cases := []struct {
		prom string
		want float64
	}{
		{"nexus_tunnels_active", 7},
		{"nexus_cert_cache_size", 123},
		{"nexus_cert_prewarm_duration_ms", 45},
		{"nexus_redis_available", 1},
		{"nexus_killswitch_active", 1},
	}
	for _, c := range cases {
		got := testutil.ToFloat64(mustGauge(t, promReg, c.prom))
		if got != c.want {
			t.Errorf("gauge %q: got %v want %v", c.prom, got, c.want)
		}
	}
}

// TestRegister_CountersIncWithLabels exercises the label arity of the
// three label-bearing counters. A label-arity mismatch between
// prometheus.go and the call sites would panic at the first Inc — this
// test catches that regression before it ships.
func TestRegister_CountersIncWithLabels(t *testing.T) {
	promReg := prometheus.NewRegistry()
	Register(registry.NewRegistry(promReg))

	ConnectionsTotal.With("ok").Inc()
	ConnectionsTotal.With("ok").Inc()
	ConnectionsTotal.With("err").Inc()
	CertCacheHits.With("L1").Inc()
	CertCacheHits.With("L2").Inc()
	CertCacheMisses.With().Inc()
	PinningPassthroughTotal.With("allow").Inc()

	if got := counterValue(t, promReg, "nexus_tunnels_total", "result", "ok"); got != 2 {
		t.Errorf("tunnels_total{result=ok} = %v, want 2", got)
	}
	if got := counterValue(t, promReg, "nexus_tunnels_total", "result", "err"); got != 1 {
		t.Errorf("tunnels_total{result=err} = %v, want 1", got)
	}
	if got := counterValue(t, promReg, "nexus_cert_cache_hits_total", "layer", "L1"); got != 1 {
		t.Errorf("cert_cache_hits_total{layer=L1} = %v, want 1", got)
	}
	if got := counterValue(t, promReg, "nexus_cert_cache_hits_total", "layer", "L2"); got != 1 {
		t.Errorf("cert_cache_hits_total{layer=L2} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(mustCounter(t, promReg, "nexus_cert_cache_misses_total")); got != 1 {
		t.Errorf("cert_cache_misses_total = %v, want 1", got)
	}
	if got := counterValue(t, promReg, "nexus_pinning_passthrough_total", "status", "allow"); got != 1 {
		t.Errorf("pinning_passthrough_total{status=allow} = %v, want 1", got)
	}
}

// TestRegister_HistogramObserveRecords confirms that the CertSignMs
// histogram is functional after binding by recording an observation and
// verifying the sample count surfaced through Prometheus is 1 (not 0,
// not panic).
func TestRegister_HistogramObserveRecords(t *testing.T) {
	promReg := prometheus.NewRegistry()
	Register(registry.NewRegistry(promReg))

	CertSignMs.With().Observe(42)

	families, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got uint64
	for _, f := range families {
		if f.GetName() != "nexus_cert_sign_ms" {
			continue
		}
		for _, m := range f.GetMetric() {
			if h := m.GetHistogram(); h != nil {
				got = h.GetSampleCount()
			}
		}
	}
	if got != 1 {
		t.Errorf("cert_sign_ms sample count = %d, want 1", got)
	}
}

// TestRegister_IdempotentReBindWithSameRegistry guards the contract that
// Register may be re-called with the SAME registry without panicking and
// without losing previously recorded values. registry.NewCounter etc.
// cache by name, so the second Register returns the same underlying
// Prometheus instrument.
func TestRegister_IdempotentReBindWithSameRegistry(t *testing.T) {
	promReg := prometheus.NewRegistry()
	reg := registry.NewRegistry(promReg)
	Register(reg)

	ConnectionsTotal.With("ok").Inc()

	// Second Register with the same registry.Registry must not panic
	// and must rebind handles to the same underlying instruments.
	Register(reg)

	ConnectionsTotal.With("ok").Inc()

	// Total across both Inc calls must be 2 — a re-Register that
	// created a new vec would reset to 1.
	if got := counterValue(t, promReg, "nexus_tunnels_total", "result", "ok"); got != 2 {
		t.Errorf("counter lost data across idempotent Register; got %v, want 2", got)
	}
}

// snapshotHandles returns a name→pointer map of every package-level
// metric handle. Used by the nil-no-op test to detect mutation without
// hardcoding ten separate equality checks.
func snapshotHandles() map[string]any {
	return map[string]any{
		"ConnectionsActive":       any(ConnectionsActive),
		"ConnectionsTotal":        any(ConnectionsTotal),
		"CertCacheHits":           any(CertCacheHits),
		"CertCacheMisses":         any(CertCacheMisses),
		"CertCacheSize":           any(CertCacheSize),
		"CertSignMs":              any(CertSignMs),
		"CertPrewarmMs":           any(CertPrewarmMs),
		"RedisAvailable":          any(RedisAvailable),
		"PinningPassthroughTotal": any(PinningPassthroughTotal),
		"KillSwitchActive":        any(KillSwitchActive),
	}
}

// mustGauge fetches a single-label-free Gauge by Prometheus name and
// returns it as a prometheus.Collector that testutil.ToFloat64 can read.
// Used by TestRegister_GaugesAreObservableAfterBind to assert per-gauge
// values without re-walking Gather() output for each case.
func mustGauge(t *testing.T, reg *prometheus.Registry, name string) prometheus.Collector {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name && f.GetType().String() == "GAUGE" {
			// Build a one-shot gauge mirror so ToFloat64 can read it.
			g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name + "_mirror"})
			for _, m := range f.GetMetric() {
				g.Set(m.GetGauge().GetValue())
			}
			return g
		}
	}
	t.Fatalf("gauge %q not found in registry", name)
	return nil
}

// mustCounter mirrors mustGauge for unlabelled counters.
func mustCounter(t *testing.T, reg *prometheus.Registry, name string) prometheus.Collector {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name && f.GetType().String() == "COUNTER" {
			c := prometheus.NewCounter(prometheus.CounterOpts{Name: name + "_mirror"})
			for _, m := range f.GetMetric() {
				c.Add(m.GetCounter().GetValue())
			}
			return c
		}
	}
	t.Fatalf("counter %q not found in registry", name)
	return nil
}

// counterValue reads a single labelled cell from a labelled counter via
// Gather(). Returns 0 if the cell is absent — a return of 0 with no
// other counter cells present typically means the test never produced
// the expected Inc.
func counterValue(t *testing.T, reg *prometheus.Registry, metric, label, value string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != metric {
			continue
		}
		for _, m := range f.GetMetric() {
			match := false
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					match = true
					break
				}
			}
			if match {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// sortedKeys returns map keys joined by comma for friendlier failure
// messages.
func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}
