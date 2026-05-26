package wiring

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// TestInitHookRegistry_returnsNonNilRegistry verifies the hook registry
// is built with a default webhook pool config.
func TestInitHookRegistry_returnsNonNilRegistry(t *testing.T) {
	cfg := config.HTTPClientPoolConfig{
		TimeoutSec:          5,
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeoutSec:  90,
	}
	reg, err := InitHookRegistry(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg == nil {
		t.Fatal("expected non-nil hook registry")
	}
}

// TestInitHookRegistry_zeroConfig verifies zero-value config is accepted.
func TestInitHookRegistry_zeroConfig(t *testing.T) {
	reg, err := InitHookRegistry(config.HTTPClientPoolConfig{})
	if err != nil {
		t.Fatalf("unexpected error with zero config: %v", err)
	}
	if reg == nil {
		t.Fatal("expected non-nil hook registry")
	}
}

// TestInitNormEngine_returnsNonNil verifies a non-nil wirerewrite Engine.
func TestInitNormEngine_returnsNonNil(t *testing.T) {
	eng := InitNormEngine(discardLogger())
	if eng == nil {
		t.Fatal("expected non-nil norm engine")
	}
}

// TestInitHookConfigCache_nilDBPath verifies nil DB produces a non-nil cache.
func TestInitHookConfigCache_nilDBPath(t *testing.T) {
	reg, err := InitHookRegistry(config.HTTPClientPoolConfig{TimeoutSec: 5})
	if err != nil {
		t.Fatal(err)
	}
	cache := InitHookConfigCache(nil, reg, discardLogger())
	if cache == nil {
		t.Fatal("expected non-nil HookConfigCache even with nil DB")
	}
}

// TestInitPayloadCaptureStore_nilDB verifies nil DB produces a non-nil store
// with default config.
func TestInitPayloadCaptureStore_nilDB(t *testing.T) {
	pcs := InitPayloadCaptureStore(context.Background(), nil)
	if pcs == nil {
		t.Fatal("expected non-nil PayloadCaptureStore for nil DB")
	}
}

// TestInitPayloadCaptureStore_withDBNoRow verifies DB path with missing row
// uses default config (no error, no panic).
func TestInitPayloadCaptureStore_withDBNoRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("payload_capture.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))

	db := store.NewWithPgxPool(mock)
	pcs := InitPayloadCaptureStore(context.Background(), db)
	if pcs == nil {
		t.Fatal("expected non-nil PayloadCaptureStore with DB no-row path")
	}
}

// TestInitPayloadCaptureStore_withDBRow verifies DB path applies JSON config.
func TestInitPayloadCaptureStore_withDBRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	payload, _ := json.Marshal(map[string]any{
		"storeRequestBody":  true,
		"storeResponseBody": false,
	})
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("payload_capture.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(payload))

	db := store.NewWithPgxPool(mock)
	pcs := InitPayloadCaptureStore(context.Background(), db)
	if pcs == nil {
		t.Fatal("expected non-nil PayloadCaptureStore")
	}
	cfg := pcs.Get()
	if !cfg.StoreRequestBody {
		t.Error("expected StoreRequestBody=true from DB row")
	}
}

// TestInitPayloadCaptureStore_loadErrorUsesDefaults verifies that when
// LoadPayloadCaptureConfig returns an error, InitPayloadCaptureStore logs
// a warning and returns a store with default config (line 129 in hooks.go).
func TestInitPayloadCaptureStore_loadErrorUsesDefaults(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	// Return a DB error on the system_metadata query → LoadPayloadCaptureConfig errors.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("payload_capture.config").
		WillReturnError(errTest("db unavailable"))

	db := store.NewWithPgxPool(mock)
	pcs := InitPayloadCaptureStore(context.Background(), db)
	if pcs == nil {
		t.Fatal("expected non-nil store even when load fails")
	}
	// On load error, store is initialized with DefaultConfig.
	cfg := pcs.Get()
	def := payloadcapture.DefaultConfig()
	if cfg.MaxInlineBodyBytes != def.MaxInlineBodyBytes {
		t.Errorf("expected default config on load error, got %+v", cfg)
	}
}

// TestInitHookConfigCache_withDBNilPoolReturnsNonNil verifies that when
// DB is provided but no hooks are in the cache yet (Reload not called),
// the cache is still non-nil and usable.
func TestInitHookConfigCache_withDBNilPoolReturnsNonNil(t *testing.T) {
	reg, err := InitHookRegistry(config.HTTPClientPoolConfig{TimeoutSec: 5})
	if err != nil {
		t.Fatal(err)
	}
	// Pass a DB backed by pgxmock. The cache itself does not query on
	// construction — only on Reload() which we don't call here.
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)
	cache := InitHookConfigCache(db, reg, discardLogger())
	if cache == nil {
		t.Fatal("expected non-nil HookConfigCache with non-nil DB")
	}
}
