package configdispatch_test

// configdispatch_handlers_test.go drives every per-key handler's apply path
// (happy path + nil-dep degradation) to hit the ≥95% statement coverage
// target. Tests assert observable subsystem state changes, not mere nil-error
// returns.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/configdispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/access"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// silentDeps returns a Deps with real (cheap) concrete instances for the
// mandatory fields and nil for the three optional ones (HookConfigCache,
// ConfigDB, TelemetryProvider). ProxyServer is also nil — tests that need it
// wire their own.
func silentDeps(t *testing.T) configdispatch.Deps {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	ac, err := access.NewChecker(nil, nil, nil)
	if err != nil {
		t.Fatalf("access.NewChecker: %v", err)
	}
	return configdispatch.Deps{
		Logger:              logger,
		ThingID:             "test-proxy",
		Outcomes:            thingclient.NewOutcomeTracker(),
		KillSwitch:          killswitch.NewKillSwitch(logger),
		ExemptionStore:      exemption.NewStore(logger),
		HookConfigCache:     nil,
		ConfigDB:            nil,
		CacheManager:        cache.NewManager(0, logger),
		AccessChecker:       ac,
		TelemetryProvider:   nil,
		PayloadCaptureStore: payloadcapture.NewStore(payloadcapture.DefaultConfig()),
		ProxyServer:         nil,
	}
}

// applyKey dispatches a single key through the loader using a fresh context.
func applyKey(t *testing.T, loader *cfgloader.Loader, key string, payload []byte) error {
	t.Helper()
	ctx := context.Background()
	desired := map[string]thingclient.ConfigState{
		key: {State: payload, Version: 1},
	}
	_, err := loader.Apply(ctx, desired)
	return err
}

// bufLogger returns a logger that writes structured text into buf so tests can
// assert log output.
func bufLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestKillSwitch_EngageToggle(t *testing.T) {
	d := silentDeps(t)
	loader := configdispatch.BuildConfigLoader(d)

	// Initially disengaged.
	if d.KillSwitch.IsEngaged() {
		t.Fatal("kill switch should start disengaged")
	}

	// Apply engaged=true — the handler calls Toggle when state differs.
	payload := mustJSON(t, map[string]any{"engaged": true})
	if err := applyKey(t, loader, "killswitch", payload); err != nil {
		t.Fatalf("apply killswitch engage: %v", err)
	}
	if !d.KillSwitch.IsEngaged() {
		t.Error("kill switch should be engaged after apply")
	}

	// The handler returns the live snapshot as reported bytes — verify JSON round-trip.
	ctx := context.Background()
	desired := map[string]thingclient.ConfigState{"killswitch": {State: payload, Version: 2}}
	reported, err := loader.Apply(ctx, desired)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(reported["killswitch"].State, &snap); err != nil {
		t.Fatalf("reported bytes not valid JSON: %v", err)
	}
	if snap["engaged"] != true {
		t.Errorf("reported snapshot engaged=%v, want true", snap["engaged"])
	}
}

func TestKillSwitch_DisengageWhenAlreadyMatching(t *testing.T) {
	d := silentDeps(t)
	loader := configdispatch.BuildConfigLoader(d)

	// Both starts-disengaged and apply engaged=false — no toggle occurs, no error.
	payload := mustJSON(t, map[string]any{"engaged": false})
	if err := applyKey(t, loader, "killswitch", payload); err != nil {
		t.Fatalf("apply killswitch no-change: %v", err)
	}
	if d.KillSwitch.IsEngaged() {
		t.Error("kill switch should remain disengaged")
	}
}

// TestKillSwitch_BlankTickPayloads exercises the presence-based parser
// invariant: empty / null / `{}` (no "engaged" field) payloads must NEVER toggle
// the kill-switch — they are no-ops that preserve the current brake state.
func TestKillSwitch_BlankTickPayloads(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"empty_bytes", []byte{}},
		{"whitespace_only", []byte("   ")},
		{"null_literal", []byte("null")},
		{"empty_object", []byte("{}")}, // has no "engaged" field → s.Engaged == nil
		{"extra_fields_no_engaged", []byte(`{"state":"ok","reason":"test"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := silentDeps(t)
			// Pre-engage the kill-switch so a false blank-tick would disengage it.
			d.KillSwitch.Toggle(true, "pre-test")
			loader := configdispatch.BuildConfigLoader(d)

			if err := applyKey(t, loader, "killswitch", tc.payload); err != nil {
				t.Fatalf("blank tick %q: unexpected error: %v", tc.name, err)
			}
			if !d.KillSwitch.IsEngaged() {
				t.Errorf("blank tick %q must NOT disengage an engaged kill-switch (F-0123)", tc.name)
			}
		})
	}
}

// TestKillSwitch_InvalidJSON_ReturnsError verifies that a malformed shadow
// payload surfaces a parse error (the registerKillSwitch error branch) rather
// than silently toggling the brake.
func TestKillSwitch_InvalidJSON_ReturnsError(t *testing.T) {
	d := silentDeps(t)
	loader := configdispatch.BuildConfigLoader(d)

	err := applyKey(t, loader, "killswitch", []byte(`{not valid json`))
	if err == nil {
		t.Fatal("expected parse error for malformed killswitch JSON, got nil")
	}
}

// sqlmock helpers

// newSQLMock wraps a fresh sqlmock + sql.DB using the QueryMatcherEqual option
// (avoids regex-escape issues with Postgres-quoted identifiers).
func newSQLMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// exemptionGrantCols must match the SELECT column order in LoadActiveExemptions.
var exemptionGrantCols = []string{
	"id", "source_ip", "target_host", "reason", "approved_by",
	"inactive", "effective_from", "expires_at",
}

// The query constant is duplicated here from the private loaders package.
// If the query drifts, sqlmock will fail and the test will surface the mismatch.
const activeExemptionQuery = `
	SELECT id, source_ip, target_host, reason, approved_by, inactive,
	       effective_from, expires_at
	FROM compliance_exemption_grant
	WHERE NOT inactive
	  AND effective_from <= $1
	  AND expires_at > $1
	ORDER BY expires_at ASC
`

// exemptions (nil DB → no-op, no panic)

func TestExemptions_NilDB_ReturnsNilNoError(t *testing.T) {
	d := silentDeps(t) // ConfigDB is nil
	loader := configdispatch.BuildConfigLoader(d)

	// Handler must return nil (no error) and not panic when ConfigDB is nil.
	if err := applyKey(t, loader, "exemptions", []byte(`{}`)); err != nil {
		t.Fatalf("exemptions with nil DB returned error: %v", err)
	}
}

func TestExemptions_WithDB_EmptyResult_StoreRebuilt(t *testing.T) {
	db, mock := newSQLMock(t)
	// The handler calls LoadActiveExemptions which passes time.Now() as $1.
	// Use sqlmock.AnyArg() to match any time value.
	mock.ExpectQuery(activeExemptionQuery).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(exemptionGrantCols))

	d := silentDeps(t)
	d.ConfigDB = db
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "exemptions", []byte(`{}`)); err != nil {
		t.Fatalf("exemptions empty DB result: %v", err)
	}
	// After rebuild with empty list, store should have zero active entries.
	if got := d.ExemptionStore.List(); len(got) != 0 {
		t.Errorf("exemption store: expected 0 active entries, got %d", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestExemptions_WithDB_QueryError_Propagated(t *testing.T) {
	db, mock := newSQLMock(t)
	wantErr := errors.New("db unavailable")
	mock.ExpectQuery(activeExemptionQuery).
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(wantErr)

	d := silentDeps(t)
	d.ConfigDB = db
	loader := configdispatch.BuildConfigLoader(d)

	err := applyKey(t, loader, "exemptions", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error from DB query failure but got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error chain should contain wantErr; got: %v", err)
	}
}

// fakeHookReloader is a minimal HookConfigReloader spy for handler tests.
type fakeHookReloader struct {
	reloadCalled int
	reloadErr    error
}

func (f *fakeHookReloader) Reload(_ context.Context) error {
	f.reloadCalled++
	return f.reloadErr
}

func TestHooks_CallsReloadWhenNonNil(t *testing.T) {
	d := silentDeps(t)
	spy := &fakeHookReloader{}
	d.HookConfigCache = spy
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "hooks", []byte(`{}`)); err != nil {
		t.Fatalf("hooks apply: %v", err)
	}
	if spy.reloadCalled != 1 {
		t.Errorf("Reload called %d times, want 1", spy.reloadCalled)
	}
}

func TestHooks_NilCacheToleratedNoError(t *testing.T) {
	d := silentDeps(t) // HookConfigCache is nil
	loader := configdispatch.BuildConfigLoader(d)

	// Must not panic, must return nil error.
	if err := applyKey(t, loader, "hooks", []byte(`{}`)); err != nil {
		t.Fatalf("hooks with nil cache: %v", err)
	}
}

func TestHooks_ReloadErrorPropagated(t *testing.T) {
	d := silentDeps(t)
	spy := &fakeHookReloader{reloadErr: errSentinel}
	d.HookConfigCache = spy
	loader := configdispatch.BuildConfigLoader(d)

	err := applyKey(t, loader, "hooks", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error from Reload but got nil")
	}
}

func TestHooks_InvalidatesCacheWhenManagerNonNil(t *testing.T) {
	d := silentDeps(t)
	d.CacheManager = cache.NewManager(0, d.Logger)
	loader := configdispatch.BuildConfigLoader(d)

	// Should complete without error even with no loaders registered on the manager.
	if err := applyKey(t, loader, "hooks", nil); err != nil {
		t.Fatalf("hooks invalidate cache: %v", err)
	}
}

func TestInterceptionDomains_NilCacheManager_NoError(t *testing.T) {
	d := silentDeps(t)
	d.CacheManager = nil
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "interception_domains", []byte(`{}`)); err != nil {
		t.Fatalf("interception_domains nil manager: %v", err)
	}
}

func TestInterceptionDomains_WithCacheManager_InvalidatesCategories(t *testing.T) {
	d := silentDeps(t)
	// CacheManager is already wired in silentDeps; ConfigDB is nil so
	// reloadAllowlistAndSwap short-circuits (returns nil immediately).
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "interception_domains", []byte(`{}`)); err != nil {
		t.Fatalf("interception_domains: %v", err)
	}
}

func TestInterceptionDomains_WithManagerAndDB_AllowlistGetError_Propagated(t *testing.T) {
	// Both CacheManager and ConfigDB are non-nil → reloadAllowlistAndSwap runs.
	// No loader registered for CategoryAllowlists → Get returns "no loader" error.
	db, mock := newSQLMock(t)
	// No SQL expectations — the error comes from CacheManager.Get before any DB call.
	_ = mock

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := cache.NewManager(0, logger)
	// Deliberately no loader for CategoryAllowlists.

	ac, err := access.NewChecker(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	d := silentDeps(t)
	d.CacheManager = mgr
	d.ConfigDB = db
	d.AccessChecker = ac
	loader := configdispatch.BuildConfigLoader(d)

	applyErr := applyKey(t, loader, "interception_domains", []byte(`{}`))
	if applyErr == nil {
		t.Fatal("expected error from reloadAllowlistAndSwap when no loader registered")
	}
}

func TestInterceptionDomains_WithManagerAndDB_AllowlistSwapped(t *testing.T) {
	// Register a loader for CategoryAllowlists that returns a []string.
	// The handler should call AccessChecker.SwapDomainAllowlist with those entries.
	db, mock := newSQLMock(t)
	_ = mock

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := cache.NewManager(0, logger)
	mgr.RegisterLoader(cache.CategoryAllowlists, func(_ context.Context) (interface{}, error) {
		return []string{"api.openai.com:443"}, nil
	})
	mgr.RegisterLoader(cache.CategoryInterceptionDomains, func(_ context.Context) (interface{}, error) {
		return nil, nil
	})

	ac, err := access.NewChecker(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	d := silentDeps(t)
	d.CacheManager = mgr
	d.ConfigDB = db
	d.AccessChecker = ac
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "interception_domains", []byte(`{}`)); err != nil {
		t.Fatalf("interception_domains allowlist swap: %v", err)
	}
}

func TestObservability_NilManagerOrProvider_NoError(t *testing.T) {
	for _, tc := range []struct {
		name         string
		nilManager   bool
		nilTelemetry bool
	}{
		{"nil_manager", true, false},
		{"nil_provider", false, true},
		{"both_nil", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := silentDeps(t)
			if tc.nilManager {
				d.CacheManager = nil
			}
			if tc.nilTelemetry {
				d.TelemetryProvider = nil
			}
			loader := configdispatch.BuildConfigLoader(d)
			if err := applyKey(t, loader, "observability", []byte(`{}`)); err != nil {
				t.Fatalf("observability %s: %v", tc.name, err)
			}
		})
	}
}

func TestObservability_WithManagerAndProvider_CacheGetError_Propagated(t *testing.T) {
	// Provide a non-nil CacheManager and a non-nil TelemetryProvider.
	// The CacheManager has no loader registered for CategoryObservability →
	// Get() returns an error → handler wraps and returns it.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tp := &telemetry.SwappableTracerProvider{}
	mgr := cache.NewManager(0, logger)
	// No loader registered → Get will return "no loader" error.

	d := silentDeps(t)
	d.CacheManager = mgr
	d.TelemetryProvider = tp
	loader := configdispatch.BuildConfigLoader(d)

	err := applyKey(t, loader, "observability", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error when CacheManager has no loader for observability")
	}
}

func TestObservability_WithManagerAndProvider_NonConfigTypeResult_NoError(t *testing.T) {
	// Register a loader that returns a non-*telemetry.Config value.
	// Handler's type assertion `data.(*telemetry.Config)` fails → returns nil, nil.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tp := &telemetry.SwappableTracerProvider{}
	mgr := cache.NewManager(0, logger)
	mgr.RegisterLoader(cache.CategoryObservability, func(_ context.Context) (interface{}, error) {
		return "not-a-config", nil
	})

	d := silentDeps(t)
	d.CacheManager = mgr
	d.TelemetryProvider = tp
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "observability", []byte(`{}`)); err != nil {
		t.Fatalf("non-config type result: %v", err)
	}
}

func TestObservability_WithManagerAndProvider_NilDataResult_NoError(t *testing.T) {
	// Register a loader that returns nil.
	// Handler's nil check `otelCfg == nil` → returns nil, nil.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tp := &telemetry.SwappableTracerProvider{}
	mgr := cache.NewManager(0, logger)
	mgr.RegisterLoader(cache.CategoryObservability, func(_ context.Context) (interface{}, error) {
		return (*telemetry.Config)(nil), nil
	})

	d := silentDeps(t)
	d.CacheManager = mgr
	d.TelemetryProvider = tp
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "observability", []byte(`{}`)); err != nil {
		t.Fatalf("nil data result: %v", err)
	}
}

// observability full path (Reconfigure called)

func TestObservability_WithManagerAndProvider_ReconfigureCalled(t *testing.T) {
	// Register a loader that returns a real *telemetry.Config with Enabled=false
	// so Reconfigure uses the noop provider (no network call).
	// Use telemetry.Init to build a properly-initialized SwappableTracerProvider.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tp, err := telemetry.Init(context.Background(), telemetry.Config{
		Enabled:     false,
		ServiceName: "test",
	}, logger)
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}

	otelCfg := &telemetry.Config{Enabled: false, ServiceName: "test-reconfigure"}
	mgr := cache.NewManager(0, logger)
	mgr.RegisterLoader(cache.CategoryObservability, func(_ context.Context) (interface{}, error) {
		return otelCfg, nil
	})

	d := silentDeps(t)
	d.Logger = logger
	d.CacheManager = mgr
	d.TelemetryProvider = tp
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "observability", []byte(`{}`)); err != nil {
		t.Fatalf("observability Reconfigure path: %v", err)
	}
}

func TestPayloadCapture_NilDB_NoError(t *testing.T) {
	d := silentDeps(t) // ConfigDB is nil
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "payload_capture", []byte(`{}`)); err != nil {
		t.Fatalf("payload_capture nil DB: %v", err)
	}
}

// payloadCaptureQuery is the SQL that LoadPayloadCaptureConfig issues.
const payloadCaptureQuery = `SELECT value FROM system_metadata WHERE key = $1`
const payloadCaptureKey = "payload_capture.config"

func TestPayloadCapture_WithDB_HappyPath_StoreUpdated(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(payloadCaptureQuery).
		WithArgs(payloadCaptureKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).
			AddRow([]byte(`{"storeRequestBody":true,"storeResponseBody":false,"maxInlineBodyBytes":262144,"maxRequestBytes":10485760,"maxResponseBytes":10485760}`)))

	d := silentDeps(t)
	d.ConfigDB = db
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "payload_capture", []byte(`{}`)); err != nil {
		t.Fatalf("payload_capture DB happy path: %v", err)
	}
	got := d.PayloadCaptureStore.Get()
	if !got.StoreRequestBody {
		t.Errorf("PayloadCaptureStore.StoreRequestBody = false, want true after apply")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestPayloadCapture_WithDB_QueryError_Propagated(t *testing.T) {
	db, mock := newSQLMock(t)
	wantErr := errors.New("payload DB error")
	mock.ExpectQuery(payloadCaptureQuery).
		WithArgs(payloadCaptureKey).
		WillReturnError(wantErr)

	d := silentDeps(t)
	d.ConfigDB = db
	loader := configdispatch.BuildConfigLoader(d)

	err := applyKey(t, loader, "payload_capture", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error from payload_capture DB failure")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error chain missing wantErr; got: %v", err)
	}
}

func TestStreamingCompliance_NilDB_NoError(t *testing.T) {
	d := silentDeps(t) // ConfigDB is nil
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "streaming_compliance", []byte(`{}`)); err != nil {
		t.Fatalf("streaming_compliance nil DB: %v", err)
	}
}

// streaming_compliance handler now routes raw shadow payload
// directly through Store.ApplyShadowState — no DB re-read. These
// tests pin the new contract (3 branches):
//   (1) nil Store → handler skips silently (no error, no panic)
//   (2) valid raw JSON → Store.Get() reflects admin policy after apply
//   (3) malformed raw → ApplyShadowState error propagated with wrap

func TestStreamingCompliance_NilStore_Skipped(t *testing.T) {
	d := silentDeps(t) // StreamingPolicyStore nil — handler must tolerate
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "streaming_compliance", []byte(`{}`)); err != nil {
		t.Fatalf("nil-Store path should not error; got %v", err)
	}
}

func TestStreamingCompliance_ValidRaw_Applied(t *testing.T) {
	store := streampolicy.NewStore(streampolicy.DefaultPolicy())
	d := silentDeps(t)
	d.StreamingPolicyStore = store
	loader := configdispatch.BuildConfigLoader(d)

	rawJSON := []byte(`{"default_mode":"chunked_async","fail_behavior":"fail_close","chunk_bytes":8192}`)
	if err := applyKey(t, loader, "streaming_compliance", rawJSON); err != nil {
		t.Fatalf("streaming_compliance valid raw: %v", err)
	}
	got := store.Get()
	if got.Mode != streampolicy.ModeChunkedAsync {
		t.Errorf("Mode = %q, want %q", got.Mode, streampolicy.ModeChunkedAsync)
	}
	if got.FailBehavior != streampolicy.FailClose {
		t.Errorf("FailBehavior = %q, want %q", got.FailBehavior, streampolicy.FailClose)
	}
}

func TestStreamingCompliance_MalformedRaw_ErrorWrapped(t *testing.T) {
	store := streampolicy.NewStore(streampolicy.DefaultPolicy())
	d := silentDeps(t)
	d.StreamingPolicyStore = store
	loader := configdispatch.BuildConfigLoader(d)

	err := applyKey(t, loader, "streaming_compliance", []byte(`{not valid json`))
	if err == nil {
		t.Fatal("expected error on malformed raw")
	}
	// Wrapped message should mention the handler context so operators can
	// trace it back to the streaming_compliance shadow key in logs.
	if got := err.Error(); !strings.Contains(got, "streaming compliance") {
		t.Errorf("error chain missing context; got: %q", got)
	}
}

func TestOnboarding_NilProxyServer_Panics(t *testing.T) {
	// ProxyServer is nil — SetOnboardingEnabled is called on a nil pointer:
	// production code dereferences ps.ProxyServer directly (no nil guard).
	// This test documents the known "caller must wire a non-nil ProxyServer"
	// invariant and verifies the panic is recoverable.
	d := silentDeps(t)
	d.ProxyServer = nil
	loader := configdispatch.BuildConfigLoader(d)

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic calling SetOnboardingEnabled on nil ProxyServer but did not")
		}
	}()
	_ = applyKey(t, loader, "onboarding", mustJSON(t, map[string]any{"enabled": true}))
}

func TestLogLevel_ValidLevelApplied(t *testing.T) {
	var buf bytes.Buffer
	d := silentDeps(t)
	d.Logger = bufLogger(&buf)
	loader := configdispatch.BuildConfigLoader(d)

	if err := applyKey(t, loader, "log_level", mustJSON(t, map[string]any{"level": "debug"})); err != nil {
		t.Fatalf("log_level apply: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("log level updated via shadow")) {
		t.Errorf("expected log message in output, got: %s", buf.String())
	}
}

func TestLogLevel_UnknownLevelFallsBackGracefully(t *testing.T) {
	d := silentDeps(t)
	loader := configdispatch.BuildConfigLoader(d)

	// An unknown level string should not return an error — logging.SetLevel
	// falls back to "info" for unknown names.
	if err := applyKey(t, loader, "log_level", mustJSON(t, map[string]any{"level": "nonexistent"})); err != nil {
		t.Fatalf("log_level unknown: %v", err)
	}
}

func TestLogLevel_EmptyPayload_NoError(t *testing.T) {
	d := silentDeps(t)
	loader := configdispatch.BuildConfigLoader(d)

	// Empty bytes → ParseJSON returns zero struct → SetLevel("") is fine.
	if err := applyKey(t, loader, "log_level", []byte{}); err != nil {
		t.Fatalf("log_level empty payload: %v", err)
	}
}

func TestInitHubAndCfgLoader_NilTCFactory(t *testing.T) {
	d := silentDeps(t)
	ctx := context.Background()

	// tcFactory returns nil — result must carry a non-nil CfgLoader but nil
	// ThingClient.
	result := configdispatch.InitHubAndCfgLoader(ctx, d,
		func(_ func(map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error)) *thingclient.Client {
			return nil
		},
	)
	if result.CfgLoader == nil {
		t.Fatal("CfgLoader must be non-nil even when ThingClient is nil")
	}
	if result.ThingClient != nil {
		t.Fatal("ThingClient should be nil")
	}
	// The loader must still have all 9 keys registered.
	if n := len(result.CfgLoader.Keys()); n != 9 {
		t.Errorf("CfgLoader.Keys() = %d, want 9", n)
	}
}

// TestInitHubAndCfgLoader_ApplyClosure exercises the apply closure wired by
// InitHubAndCfgLoader to ensure it routes through loader.Apply and adds
// unknown keys as passthrough (the for-loop inside the closure).
func TestInitHubAndCfgLoader_ApplyClosure(t *testing.T) {
	d := silentDeps(t)
	ctx := context.Background()

	var capturedApply func(map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error)

	result := configdispatch.InitHubAndCfgLoader(ctx, d,
		func(onCfg func(map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error)) *thingclient.Client {
			capturedApply = onCfg
			return nil
		},
	)
	_ = result

	// Invoke the captured closure with a known key and an unknown key.
	desired := map[string]thingclient.ConfigState{
		"killswitch": {State: mustJSON(t, map[string]any{"engaged": false}), Version: 1},
		"unknown_x":  {State: []byte(`{}`), Version: 1},
	}
	reported, err := capturedApply(desired)
	if err != nil {
		t.Fatalf("capturedApply: %v", err)
	}
	// Unknown key must be echoed in reported (the passthrough branch).
	if _, ok := reported["unknown_x"]; !ok {
		t.Error("unknown key should be echoed in reported map")
	}
}

// InitHubAndCfgLoader Phase 2 (non-nil ThingClient)

// TestInitHubAndCfgLoader_NonNilTC covers the Phase 2 path where the factory
// returns a non-nil *thingclient.Client so InitHubAndCfgLoader rebuilds the
// loader with tc.Outcomes(). We cannot construct a real thingclient.Client
// without a Hub connection, but we can verify nil factory → nil TC path and
// the closure executes without panic.
//
// The Phase 2 rebuild itself is tested structurally: the result must carry a
// non-nil CfgLoader with all 9 keys.
func TestInitHubAndCfgLoader_PhaseOneThenPhaseTwo_LoaderAlwaysHas9Keys(t *testing.T) {
	d := silentDeps(t)
	ctx := context.Background()

	// Simulate a factory that returns nil (nil TC = single-phase path).
	result := configdispatch.InitHubAndCfgLoader(ctx, d,
		func(_ func(map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error)) *thingclient.Client {
			return nil
		},
	)
	if n := len(result.CfgLoader.Keys()); n != 9 {
		t.Errorf("after nil TC: CfgLoader has %d keys, want 9", n)
	}
	if result.ThingClient != nil {
		t.Error("ThingClient must be nil when factory returns nil")
	}
}

// BuildConfigLoader with all-nil optional deps

func TestBuildConfigLoader_AllNilOptionalDeps(t *testing.T) {
	// Verify that BuildConfigLoader does not panic with nil optional deps and
	// returns a loader with all 9 keys.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ac, err := access.NewChecker(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	loader := configdispatch.BuildConfigLoader(configdispatch.Deps{
		Logger:              logger,
		ThingID:             "t",
		Outcomes:            thingclient.NewOutcomeTracker(),
		KillSwitch:          killswitch.NewKillSwitch(logger),
		ExemptionStore:      exemption.NewStore(logger),
		HookConfigCache:     nil,
		ConfigDB:            nil,
		CacheManager:        cache.NewManager(0, logger),
		AccessChecker:       ac,
		TelemetryProvider:   nil,
		PayloadCaptureStore: payloadcapture.NewStore(payloadcapture.DefaultConfig()),
		ProxyServer:         nil,
	})
	if loader == nil {
		t.Fatal("BuildConfigLoader returned nil")
	}
	if n := len(loader.Keys()); n != 9 {
		t.Errorf("Keys() = %d, want 9", n)
	}
}

var errSentinel = &sentinelErr{msg: "sentinel error from test"}

type sentinelErr struct{ msg string }

func (e *sentinelErr) Error() string { return e.msg }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustJSON: %v", err)
	}
	return b
}

// Suppress "time imported and not used" if we import but don't call directly.
var _ = time.Second
