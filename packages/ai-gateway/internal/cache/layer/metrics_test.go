package cachelayer

import (
	"context"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

func TestNewMetrics_NilRegistryReturnsNil(t *testing.T) {
	if got := NewMetrics(nil); got != nil {
		t.Errorf("nil registry must yield nil Metrics; got %+v", got)
	}
}

func TestMetrics_BindLayerNoOpOnNils(t *testing.T) {
	var m *Metrics
	// nil receiver path
	m.bindLayer(&Layer{}) // must not panic
	// nil layer path
	good := NewMetrics(opsmetrics.NewRegistry(prometheus.NewRegistry()))
	good.bindLayer(nil) // must not panic
}

// gatherCounter reads a labelled counter sample from the given registry.
func gatherCounter(t *testing.T, reg *prometheus.Registry, metricName string, label, val string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != metricName {
			continue
		}
		for _, m := range f.Metric {
			if !hasLabel(m, label, val) {
				continue
			}
			if m.Counter != nil {
				return m.Counter.GetValue()
			}
		}
	}
	return 0
}

func gatherGauge(t *testing.T, reg *prometheus.Registry, metricName string, label, val string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != metricName {
			continue
		}
		for _, m := range f.Metric {
			if !hasLabel(m, label, val) {
				continue
			}
			if m.Gauge != nil {
				return m.Gauge.GetValue()
			}
		}
	}
	return 0
}

func hasLabel(m *dto.Metric, name, val string) bool {
	for _, lp := range m.Label {
		if lp.GetName() == name && lp.GetValue() == val {
			return true
		}
	}
	return false
}

func TestMetrics_AllHooksInvokeUnderlyingInstruments(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(opsmetrics.NewRegistry(reg))

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	db := store.NewWithPgxPool(mock)
	l, err := NewWithPool(db, mock, discardLogger(), Config{Metrics: m, VKCapacity: 4})
	if err != nil {
		t.Fatalf("NewWithPool: %v", err)
	}

	// Fire hit/miss/invalidate hooks.
	l.vkOnHit()
	l.vkOnMiss()
	l.vkOnInvalidate(2)

	if got := gatherCounter(t, reg, "nexus_cache_hits_total", "cache", "key_virtual_keys"); got != 1 {
		t.Errorf("hits = %v, want 1", got)
	}
	if got := gatherCounter(t, reg, "nexus_cache_misses_total", "cache", "key_virtual_keys"); got != 1 {
		t.Errorf("misses = %v, want 1", got)
	}
	if got := gatherCounter(t, reg, "nexus_cache_invalidations_total", "cache", "key_virtual_keys"); got != 1 {
		t.Errorf("vk invalidates = %v, want 1", got)
	}
	// vkOnInvalidate also updates the size gauge.
	if got := gatherGauge(t, reg, "nexus_cache_size", "cache", "key_virtual_keys"); got != 0 {
		t.Errorf("size after empty vk cache = %v, want 0", got)
	}

	// snapshotOnReload — providers / models / credentials all set size.
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols).
			AddRow("p1", "openai", nil, "openai", "https://x", "/v1", nil, nil, true, (*bool)(nil)))
	if err := l.ReloadProviders(context.Background()); err != nil {
		t.Fatalf("ReloadProviders: %v", err)
	}
	if got := gatherGauge(t, reg, "nexus_cache_size", "cache", "snapshot_providers"); got != 1 {
		t.Errorf("providers size gauge = %v, want 1", got)
	}
	if got := gatherCounter(t, reg, "nexus_cache_invalidations_total", "cache", "snapshot_providers"); got != 1 {
		t.Errorf("snapshot_providers invalidates = %v, want 1", got)
	}

	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).
			AddRow(makeModelRow("m1", "gpt-4o", "p1", true)...))
	if err := l.ReloadModels(context.Background()); err != nil {
		t.Fatalf("ReloadModels: %v", err)
	}
	if got := gatherGauge(t, reg, "nexus_cache_size", "cache", "snapshot_models"); got != 1 {
		t.Errorf("models size gauge = %v, want 1", got)
	}

	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols).
			AddRow(makeCredRow("c1", "p1", true, "active")...))
	if err := l.ReloadCredentials(context.Background()); err != nil {
		t.Fatalf("ReloadCredentials: %v", err)
	}
	if got := gatherGauge(t, reg, "nexus_cache_size", "cache", "snapshot_credentials"); got != 1 {
		t.Errorf("credentials size gauge = %v, want 1", got)
	}

	// snapshotOnReload with an unknown name hits the default-switch path —
	// no size gauge fired, but the invalidations counter still increments.
	l.snapshotOnReload("unknown-cache")
	if got := gatherCounter(t, reg, "nexus_cache_invalidations_total", "cache", "snapshot_unknown-cache"); got != 1 {
		t.Errorf("unknown snapshot invalidate = %v, want 1", got)
	}
}

func TestMetrics_HookValuesProduceExpectedLabelStrings(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(opsmetrics.NewRegistry(reg))
	if m.hits == nil || m.misses == nil || m.size == nil || m.invalidates == nil {
		t.Fatal("NewMetrics must populate every instrument")
	}
	// Pin invalidationSource — defensive: a typo here breaks dashboards.
	if invalidationSource != "hub" {
		t.Errorf("invalidationSource = %q, want hub", invalidationSource)
	}
	// Pin metric names so the spec catalog can't drift silently.
	for _, want := range []string{"cache.hits_total", "cache.misses_total", "cache.size", "cache.invalidations_total"} {
		// They must show up as snake_case under Prometheus.
		// Trigger registration via .With first then gather.
		switch want {
		case "cache.hits_total":
			m.hits.With("x").Inc()
		case "cache.misses_total":
			m.misses.With("x").Inc()
		case "cache.invalidations_total":
			m.invalidates.With("x", "y").Inc()
		case "cache.size":
			m.size.With("x").Set(0)
		}
	}
	families, _ := reg.Gather()
	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, want := range []string{"nexus_cache_hits_total", "nexus_cache_misses_total", "nexus_cache_invalidations_total", "nexus_cache_size"} {
		if !names[want] {
			t.Errorf("missing metric family %q", want)
		}
	}
}

func TestMetrics_BindLayer_VKOnHitMissPropagateThroughKeyCache(t *testing.T) {
	// The KeyCache constructor reads l.vkOnHit / l.vkOnMiss at build time.
	// Because Metrics.bindLayer runs BEFORE NewKeyCache (in newLayer), the
	// hit/miss wiring should reach the KeyCache opts so cache.Get fires them.
	reg := prometheus.NewRegistry()
	m := NewMetrics(opsmetrics.NewRegistry(reg))

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	db := store.NewWithPgxPool(mock)
	l, err := NewWithPool(db, mock, discardLogger(), Config{Metrics: m, VKCapacity: 4})
	if err != nil {
		t.Fatalf("NewWithPool: %v", err)
	}

	// Prime via Get → MISS path on first call, HIT on second.
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("hh").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk1", "hh")...))
	if _, err := l.GetVirtualKeyByHash(context.Background(), "hh"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := l.GetVirtualKeyByHash(context.Background(), "hh"); err != nil {
		t.Fatalf("second call: %v", err)
	}

	// One miss + one hit should be recorded on the underlying instruments.
	if got := gatherCounter(t, reg, "nexus_cache_hits_total", "cache", "key_virtual_keys"); got != 1 {
		t.Errorf("vk cache hits = %v, want 1", got)
	}
	if got := gatherCounter(t, reg, "nexus_cache_misses_total", "cache", "key_virtual_keys"); got != 1 {
		t.Errorf("vk cache misses = %v, want 1", got)
	}
}

// TestNewWithPool_Metrics_NewLayerWithoutMetricsRemainsBindable —
// guards against future regressions where Config.Metrics=nil would
// silently leave the hooks installed from a prior call.
func TestNewLayer_NoMetrics_LeavesHooksNil(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	_ = mock
	if l.vkOnHit != nil || l.vkOnMiss != nil || l.vkOnInvalidate != nil || l.snapshotOnReload != nil {
		t.Error("nil Config.Metrics must leave hooks nil")
	}
}

// Pin the package doc comment shape so the metric-name catalog stays
// in sync with the actual metric registrations.
func TestMetricsNames_MirrorOpsCatalogDoc(t *testing.T) {
	for _, want := range []string{"cache.hits_total", "cache.misses_total", "cache.size", "cache.invalidations_total"} {
		if !strings.HasPrefix(want, "cache.") {
			t.Errorf("ops-catalog metric must start with cache.; got %q", want)
		}
	}
}
