package configstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func ptrStr(s string) *string { return &s }

func TestExtractCacheStore_Get_singleton_returnsRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	store := NewExtractCacheStoreWithPgxPool(mock)

	updatedAt := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT id, enabled, ttl_seconds, apply_freshness_rules`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "enabled", "ttl_seconds", "apply_freshness_rules", "updated_at", "updated_by",
		}).AddRow("singleton", false, 1800, false, updatedAt, ptrStr("admin@example.com")))

	row, err := store.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Enabled != false {
		t.Errorf("Enabled = %v, want false", row.Enabled)
	}
	if row.TTLSeconds != 1800 {
		t.Errorf("TTLSeconds = %d, want 1800", row.TTLSeconds)
	}
	if row.ApplyFreshnessRules != false {
		t.Errorf("ApplyFreshnessRules = %v, want false", row.ApplyFreshnessRules)
	}
}

// WireState is the contract the admin push AND the configreconcile drift loader
// both depend on (F-0102/F-0345): it must project EXACTLY the three behavioral
// fields and drop bookkeeping columns, or the loader's source-of-truth diff
// would thrash against the pushed blob every reconcile cycle.
func TestExtractCacheConfigRow_WireState_projectsThreeFieldsOnly(t *testing.T) {
	updatedBy := "admin@example.com"
	row := &ExtractCacheConfigRow{
		ID:                  "singleton",
		Enabled:             true,
		TTLSeconds:          3600,
		ApplyFreshnessRules: true,
		UpdatedAt:           time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC),
		UpdatedBy:           &updatedBy,
	}
	got := row.WireState()
	want := map[string]any{
		"enabled":             true,
		"ttlSeconds":          3600,
		"applyFreshnessRules": true,
	}
	if len(got) != len(want) {
		t.Fatalf("WireState has %d keys, want %d (bookkeeping columns must be excluded): %v", len(got), len(want), got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("WireState[%q] = %v, want %v", k, got[k], w)
		}
	}
	// Bookkeeping columns must NOT leak into the wire shape.
	for _, k := range []string{"id", "updatedAt", "updatedBy"} {
		if _, present := got[k]; present {
			t.Errorf("WireState leaked bookkeeping key %q", k)
		}
	}
}

func TestExtractCacheStore_Get_noRow_returnsDefaults(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	store := NewExtractCacheStoreWithPgxPool(mock)

	mock.ExpectQuery(`SELECT id, enabled, ttl_seconds, apply_freshness_rules`).
		WillReturnError(pgx.ErrNoRows)

	row, err := store.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !row.Enabled {
		t.Errorf("default Enabled = false, want true")
	}
	if row.TTLSeconds != extractCacheDefaultTTLSeconds {
		t.Errorf("default TTLSeconds = %d, want %d", row.TTLSeconds, extractCacheDefaultTTLSeconds)
	}
	if !row.ApplyFreshnessRules {
		t.Errorf("default ApplyFreshnessRules = false, want true")
	}
}

func TestExtractCacheStore_Get_scanError_propagates(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	store := NewExtractCacheStoreWithPgxPool(mock)

	dbErr := errors.New("connection refused")
	mock.ExpectQuery(`SELECT id, enabled, ttl_seconds, apply_freshness_rules`).
		WillReturnError(dbErr)

	_, err = store.Get(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, dbErr) {
		t.Errorf("err = %v, want wrap of %v", err, dbErr)
	}
}

func TestExtractCacheStore_Save_validInputs(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	store := NewExtractCacheStoreWithPgxPool(mock)

	updatedAt := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`INSERT INTO extract_cache_config`).
		WithArgs(true, 7200, false, "admin@example.com").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "enabled", "ttl_seconds", "apply_freshness_rules", "updated_at", "updated_by",
		}).AddRow("singleton", true, 7200, false, updatedAt, ptrStr("admin@example.com")))

	row, err := store.Save(context.Background(), ExtractCacheSaveInput{
		Enabled:             true,
		TTLSeconds:          7200,
		ApplyFreshnessRules: false,
		UpdatedBy:           "admin@example.com",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if row.TTLSeconds != 7200 {
		t.Errorf("TTLSeconds = %d, want 7200", row.TTLSeconds)
	}
}

func TestExtractCacheStore_Save_clampsOutOfRangeTTL(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	store := NewExtractCacheStoreWithPgxPool(mock)

	updatedAt := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// Out-of-range TTL (30s < min 60) → clamped to schema default 3600.
	mock.ExpectQuery(`INSERT INTO extract_cache_config`).
		WithArgs(true, extractCacheDefaultTTLSeconds, true, nil).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "enabled", "ttl_seconds", "apply_freshness_rules", "updated_at", "updated_by",
		}).AddRow("singleton", true, extractCacheDefaultTTLSeconds, true, updatedAt, (*string)(nil)))

	_, err = store.Save(context.Background(), ExtractCacheSaveInput{
		Enabled:             true,
		TTLSeconds:          30, // below min
		ApplyFreshnessRules: true,
		UpdatedBy:           "",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
}
