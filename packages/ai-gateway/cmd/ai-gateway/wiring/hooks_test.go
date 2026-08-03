package wiring

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	hookwh "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/webhook"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// TestInitHookRegistry_webhookForwardInjectsRSTokenForOwnAIGuard proves the
// internal token + PublicURL threaded into InitHookRegistry reach the
// webhook-forward factory, so a hook aimed at this gateway's own AI-Guard
// compliance-webhook authenticates (fixing the every-request 401) while a hook
// aimed elsewhere stays unauthenticated.
func TestInitHookRegistry_webhookForwardInjectsRSTokenForOwnAIGuard(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-RS-Token")
		_, _ = w.Write([]byte(`{"decision":"approve"}`))
	}))
	defer srv.Close()

	// The shared webhook client built by InitHookRegistry has no SSRF guard,
	// so it can dial the 127.0.0.1 test server standing in for the gateway.
	// The empty second base mirrors production wiring when no private URL
	// could be derived — it must be filtered out, not matched.
	reg, err := InitHookRegistry(config.HTTPClientPoolConfig{TimeoutSec: 5}, "internal-secret", []string{srv.URL, ""})
	if err != nil {
		t.Fatalf("InitHookRegistry: %v", err)
	}
	factory := reg.Get("webhook-forward")
	if factory == nil {
		t.Fatal("webhook-forward factory not registered")
	}

	build := func(endpoint string) hookcore.Hook {
		h, err := factory(&hookcore.HookConfig{
			ID: "wh", ImplementationID: "webhook-forward",
			Config: map[string]any{"endpoint": endpoint},
		})
		if err != nil {
			t.Fatalf("build hook for %q: %v", endpoint, err)
		}
		return h
	}

	// Trusted AI-Guard compliance-webhook → token injected.
	seen = "unset"
	if _, err := build(srv.URL+hookwh.AIGuardComplianceWebhookPath).Execute(context.Background(), &hookcore.HookInput{Stage: "request"}); err != nil {
		t.Fatalf("execute trusted hook: %v", err)
	}
	if seen != "internal-secret" {
		t.Fatalf("trusted endpoint X-RS-Token = %q; want the injected token", seen)
	}

	// Any other webhook URL → no token (no leak of the internal secret).
	seen = "unset"
	if _, err := build(srv.URL+"/some/other/webhook").Execute(context.Background(), &hookcore.HookInput{Stage: "request"}); err != nil {
		t.Fatalf("execute other hook: %v", err)
	}
	if seen != "" {
		t.Fatalf("non-AI-Guard endpoint X-RS-Token = %q; want no token", seen)
	}
}

// TestInitHookRegistry_returnsNonNilRegistry verifies the hook registry
// is built with a default webhook pool config.
func TestInitHookRegistry_returnsNonNilRegistry(t *testing.T) {
	cfg := config.HTTPClientPoolConfig{
		TimeoutSec:          5,
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeoutSec:  90,
	}
	reg, err := InitHookRegistry(cfg, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg == nil {
		t.Fatal("expected non-nil hook registry")
	}
}

// TestInitHookRegistry_zeroConfig verifies zero-value config is accepted.
func TestInitHookRegistry_zeroConfig(t *testing.T) {
	reg, err := InitHookRegistry(config.HTTPClientPoolConfig{}, "", nil)
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
	reg, err := InitHookRegistry(config.HTTPClientPoolConfig{TimeoutSec: 5}, "", nil)
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
	reg, err := InitHookRegistry(config.HTTPClientPoolConfig{TimeoutSec: 5}, "", nil)
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
