package cachelayer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// expectAllSnapshotLoads queues the 4 SELECTs Start dispatches in
// parallel. Caller sets the rows that come back.
func expectAllSnapshotLoads(mock pgxmock.PgxPoolIface) {
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols))
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols))
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols))
	mock.ExpectQuery(`FROM provider_pricing`).
		WillReturnRows(pgxmock.NewRows(pricingCols))
}

func TestNew_RejectsNilDB(t *testing.T) {
	if _, err := New(nil, nil, Config{}); err == nil ||
		!strings.Contains(err.Error(), "db must not be nil") {
		t.Fatalf("want nil-db error; got %v", err)
	}
}

func TestNewWithPool_RejectsNilArgs(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	db := store.NewWithPgxPool(mock)

	if _, err := NewWithPool(nil, mock, nil, Config{}); err == nil ||
		!strings.Contains(err.Error(), "db must not be nil") {
		t.Fatalf("want nil-db error; got %v", err)
	}
	if _, err := NewWithPool(db, nil, nil, Config{}); err == nil ||
		!strings.Contains(err.Error(), "pool must not be nil") {
		t.Fatalf("want nil-pool error; got %v", err)
	}
}

func TestNew_AppliesDefaultsAndAcceptsNilLogger(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	db := store.NewWithPgxPool(mock)

	l, err := NewWithPool(db, mock, nil, Config{}) // nil logger → slog.Default
	if err != nil {
		t.Fatalf("NewWithPool: %v", err)
	}
	if l.log == nil {
		t.Fatal("expected default logger when nil supplied")
	}
	// Caches must be non-nil so Start does not panic.
	if l.providers == nil || l.models == nil || l.credentials == nil || l.vkeys == nil {
		t.Fatal("snapshot caches not constructed")
	}
}

func TestNewWithPool_HonoursCustomLogger(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	l, err := NewWithPool(store.NewWithPgxPool(mock), mock, custom, Config{
		VKCapacity: 5,
		VKTTL:      2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewWithPool: %v", err)
	}
	if l.log != custom {
		t.Fatal("custom logger not retained")
	}
}

func TestStart_HappyPath_PopulatesEverySnapshot(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols).
			AddRow("p1", "openai", strPtr("OpenAI"), "openai",
				"https://api.openai.com", "/v1", strPtr("2024-01"), strPtr("us-east-1"), true))
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).
			AddRow(makeModelRow("m1", "gpt-4o", "p1", true)...))
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols).
			AddRow(makeCredRow("c1", "p1", true, "active")...))
	// provider_pricing was retired; LookupCachePricing now reads from
	// the Models snapshot. The model row above ("gpt-4o" with
	// inP=3.0, crP=0.3, cwP=3.75 from makeModelRow) drives the lookup.

	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	s := l.Stats()
	if s.ProvidersSize != 1 || s.ModelsSize != 1 || s.CredentialsSize != 1 {
		t.Errorf("snapshot sizes wrong: %+v", s)
	}
	// LookupCachePricing keys on Model.code now, so use "gpt-4o" instead
	// of "anything" (which wouldn't exist in the Models snapshot).
	got := l.LookupCachePricing("openai", "p1", "gpt-4o")
	if got == nil || got.InputUSDPerM != 3.0 || got.CacheReadUSDPerM != 0.3 || got.CacheWriteUSDPerM != 3.75 {
		t.Errorf("pricing lookup wrong after Start: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestStart_AggregatesPerLoaderErrors(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnError(errors.New("boom-providers"))
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnError(errors.New("boom-models"))
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnError(errors.New("boom-credentials"))
	mock.ExpectQuery(`FROM provider_pricing`).
		WillReturnError(errors.New("boom-pricing"))

	err := l.Start(context.Background())
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	for _, want := range []string{"providers", "models", "credentials"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error missing %q: %v", want, err)
		}
	}
}

func TestReloadSnapshots_RoundTripsAndFiresMetricsHook(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.MatchExpectationsInOrder(false)

	// Capture metric callback fires.
	var fires atomic.Int64
	l.snapshotOnReload = func(string) { fires.Add(1) }

	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols).
			AddRow("p1", "openai", nil, "openai", "https://x", "/v1", nil, nil, true))
	if err := l.ReloadProviders(context.Background()); err != nil {
		t.Fatalf("ReloadProviders: %v", err)
	}
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols))
	if err := l.ReloadModels(context.Background()); err != nil {
		t.Fatalf("ReloadModels: %v", err)
	}
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols))
	if err := l.ReloadCredentials(context.Background()); err != nil {
		t.Fatalf("ReloadCredentials: %v", err)
	}
	if err := l.ReloadProviderPricing(context.Background()); err != nil {
		t.Fatalf("ReloadProviderPricing: %v", err)
	}
	if got := fires.Load(); got != 3 { // 3 snapshot reloads; pricing reload bypasses the hook
		t.Errorf("snapshot reload fires = %d, want 3", got)
	}
}

func TestReloadSnapshots_PropagateErrorAndSkipHook(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	var hookCalled bool
	l.snapshotOnReload = func(string) { hookCalled = true }
	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`FROM "Provider"`).WillReturnError(errors.New("p-err"))
	if err := l.ReloadProviders(context.Background()); err == nil {
		t.Error("ReloadProviders should surface error")
	}
	mock.ExpectQuery(`FROM "Model" m`).WillReturnError(errors.New("m-err"))
	if err := l.ReloadModels(context.Background()); err == nil {
		t.Error("ReloadModels should surface error")
	}
	mock.ExpectQuery(`FROM "Credential"`).WillReturnError(errors.New("c-err"))
	if err := l.ReloadCredentials(context.Background()); err == nil {
		t.Error("ReloadCredentials should surface error")
	}
	if hookCalled {
		t.Error("snapshotOnReload must not fire on loader error")
	}
}

func TestInvalidateAndPurgeVirtualKeys(t *testing.T) {
	mock, l := newMockLayer(t, Config{VKCapacity: 4, VKTTL: time.Minute})

	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("h1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk1", "h1")...))
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("h2").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk2", "h2")...))

	// Prime the VK cache with two entries.
	if _, err := l.GetVirtualKeyByHash(context.Background(), "h1"); err != nil {
		t.Fatalf("get h1: %v", err)
	}
	if _, err := l.GetVirtualKeyByHash(context.Background(), "h2"); err != nil {
		t.Fatalf("get h2: %v", err)
	}

	// Invalidate one + one missing.
	var invalidateFired atomic.Int64
	l.vkOnInvalidate = func(_ int) { invalidateFired.Add(1) }

	if got := l.InvalidateVirtualKeys("h1", "missing"); got != 1 {
		t.Errorf("invalidate count = %d, want 1", got)
	}
	if l.invalidationCount.Load() != 1 {
		t.Errorf("invalidationCount = %d, want 1", l.invalidationCount.Load())
	}
	if invalidateFired.Load() != 1 {
		t.Error("vkOnInvalidate must fire when entries removed")
	}

	// Invalidate-with-zero-removals must NOT fire the hook.
	invalidateFired.Store(0)
	if got := l.InvalidateVirtualKeys("never-cached"); got != 0 {
		t.Errorf("invalidate count = %d, want 0", got)
	}
	if invalidateFired.Load() != 0 {
		t.Error("hook must not fire when nothing removed")
	}

	// Purge: must drop the remaining h2 entry and fire the hook with size.
	invalidateFired.Store(0)
	l.PurgeVirtualKeys()
	if l.vkeys.Size() != 0 {
		t.Errorf("post-purge size = %d, want 0", l.vkeys.Size())
	}
	if invalidateFired.Load() != 1 {
		t.Error("Purge must fire invalidate hook when entries existed")
	}

	// Empty purge: no hook fires.
	invalidateFired.Store(0)
	l.PurgeVirtualKeys()
	if invalidateFired.Load() != 0 {
		t.Error("empty Purge must not fire hook")
	}
}

func TestStart_FiresSnapshotOnReloadHookExactlyThreeTimesOnSuccess(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.MatchExpectationsInOrder(false)
	var calls []string
	l.snapshotOnReload = func(name string) { calls = append(calls, name) }

	expectAllSnapshotLoads(mock)
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("snapshotOnReload calls = %v, want 3", calls)
	}
	got := map[string]bool{}
	for _, n := range calls {
		got[n] = true
	}
	for _, want := range []string{"providers", "models", "credentials"} {
		if !got[want] {
			t.Errorf("missing reload hook for %q", want)
		}
	}
}

func TestStats_ReportsLiveValues(t *testing.T) {
	mock, l := newMockLayer(t, Config{VKCapacity: 8, VKTTL: time.Minute})
	mock.MatchExpectationsInOrder(false)

	// Two providers, one model, one cred.
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols).
			AddRow("p1", "openai", nil, "openai", "https://x", "/v1", nil, nil, true).
			AddRow("p2", "anthropic", nil, "anthropic", "https://y", "/v1", nil, nil, true))
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).
			AddRow(makeModelRow("m1", "gpt-4o", "p1", true)...))
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols).
			AddRow(makeCredRow("c1", "p1", true, "active")...))
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Hit + miss the VK cache to populate Stats fields.
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("h1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk1", "h1")...))
	if _, err := l.GetVirtualKeyByHash(context.Background(), "h1"); err != nil {
		t.Fatalf("vk get: %v", err)
	}
	// Second call is a cache HIT (no new ExpectQuery).
	if _, err := l.GetVirtualKeyByHash(context.Background(), "h1"); err != nil {
		t.Fatalf("vk get hit: %v", err)
	}
	l.InvalidateVirtualKeys("h1")

	s := l.Stats()
	if s.ProvidersSize != 2 {
		t.Errorf("ProvidersSize = %d, want 2", s.ProvidersSize)
	}
	if s.ModelsSize != 1 {
		t.Errorf("ModelsSize = %d, want 1", s.ModelsSize)
	}
	if s.CredentialsSize != 1 {
		t.Errorf("CredentialsSize = %d, want 1", s.CredentialsSize)
	}
	if s.VKHits == 0 {
		t.Error("VKHits must be >0 after a cache hit")
	}
	if s.VKMisses == 0 {
		t.Error("VKMisses must be >0 after the priming miss")
	}
	if s.TotalInvalidates != 1 {
		t.Errorf("TotalInvalidates = %d, want 1", s.TotalInvalidates)
	}
}

// TestNew_SuccessPathThroughRealPgxpool covers the New() success
// branch: pgxpool.NewWithConfig builds a real *pgxpool.Pool with no
// network dial, so we can drive New(db, log, cfg) without a live
// Postgres. The pool is never queried — only the constructor field
// assignment matters for coverage.
// TestNewWithPool_NegativeVKCapacityWraps drives the NewKeyCache error
// branch in newLayer (the `if err != nil { return nil, fmt.Errorf(...)
// }` after the build-vk-cache call). The default-zero guard turns 0
// into 10000, but a negative explicit value reaches the LRU constructor
// which rejects it.
func TestNewWithPool_NegativeVKCapacityWraps(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	db := store.NewWithPgxPool(mock)
	_, err = NewWithPool(db, mock, discardLogger(), Config{VKCapacity: -1})
	if err == nil {
		t.Fatal("expected vk-cache build error")
	}
	if !strings.Contains(err.Error(), "build vk cache") {
		t.Errorf("missing wrap prefix; got %v", err)
	}
}

func TestNew_SuccessPathThroughRealPgxpool(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://noone:noone@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)

	db := &store.DB{Pool: pool}
	l, err := New(db, discardLogger(), Config{VKCapacity: 2, VKTTL: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if l == nil || l.providers == nil || l.models == nil || l.credentials == nil || l.vkeys == nil {
		t.Fatal("New must construct all caches")
	}
}

func TestNew_WithMetrics_BindsHooks(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	m := NewMetrics(reg)

	db := store.NewWithPgxPool(mock)
	l, err := NewWithPool(db, mock, discardLogger(), Config{Metrics: m})
	if err != nil {
		t.Fatalf("NewWithPool: %v", err)
	}
	if l.vkOnHit == nil || l.vkOnMiss == nil || l.vkOnInvalidate == nil || l.snapshotOnReload == nil {
		t.Fatal("Metrics.bindLayer must wire every hook slot")
	}
}
