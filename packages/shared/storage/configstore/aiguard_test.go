// packages/shared/storage/configstore/aiguard_test.go — pgxmock-driven unit
// tests for the singleton ai_guard_config SQL paths.
//
// These tests use pgxmock so they (a) never touch rows the test did not
// seed and (b) run in CI without TEST_DATABASE_URL. A destructive
// integration variant that mutates the shared dev DB's ai_guard_config
// singleton row stays gated behind NEXUS_DESTRUCTIVE_TESTS=1.
//
// All pure decision-tree branches (ErrNoRows defaults, JSON parse,
// marshal error paths, Save's auto-fill-ID + marshal-before-Exec
// guards) are already covered by aiguard_internal_test.go. This file
// covers the SQL surface: Load's QueryRow shape and Save's Exec call.
package configstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// aiGuardColumns mirrors the SELECT column list in (s *AIGuardStore).Load.
// Keeping this in lockstep with the SQL is intentional — if Load adds a
// column, the tests fail loudly until the columns and the row data here
// are updated too.
var aiGuardColumns = []string{
	"id", "backend_mode", "provider_id", "model_id", "external_url",
	"custom_headers", "prompt_template",
	"timeout_ms", "cache_ttl_seconds", "backend_fingerprint",
	"input_strategy", "model_context_limit",
}

func newMockStore(t *testing.T) (pgxmock.PgxPoolIface, *configstore.AIGuardStore) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, configstore.NewAIGuardStoreWithPgxPool(mock)
}

func strPtr(s string) *string { return &s }

// TestLoad_HappyPath_SeededRow drives a fully-populated singleton row
// through Scan + the JSON header-decode branch. Exercises every
// column-mapping in the Load SQL so that a future column added without
// updating the Scan call is caught here.
func TestLoad_HappyPath_SeededRow(t *testing.T) {
	mock, store := newMockStore(t)
	headers := []byte(`{"X-Tenant":"nexus"}`)
	mock.ExpectQuery(`FROM ai_guard_config WHERE id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows(aiGuardColumns).AddRow(
			"singleton", "external_url",
			strPtr("prov-1"), strPtr("model-1"),
			strPtr("https://judge.example.com/v1"),
			headers, "tmpl", 3000, 120, "fp-abc",
			"system_plus_last_user", 0,
		))

	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ID != "singleton" || got.BackendMode != "external_url" {
		t.Errorf("id/backend: %+v", got)
	}
	if got.ExternalURL == nil || *got.ExternalURL != "https://judge.example.com/v1" {
		t.Errorf("ExternalURL: %+v", got.ExternalURL)
	}
	if got.TimeoutMs != 3000 || got.CacheTTLSeconds != 120 {
		t.Errorf("timeouts: %+v", got)
	}
	if got.CustomHeaders["X-Tenant"] != "nexus" {
		t.Errorf("CustomHeaders: %+v", got.CustomHeaders)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestLoad_NoRows_ReturnsDefaults drives the pgx.ErrNoRows branch
// through the real *AIGuardStore.Load (the internal test covers
// finalizeAIGuardLoad directly; this covers the QueryRow integration).
func TestLoad_NoRows_ReturnsDefaults(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`FROM ai_guard_config`).WillReturnError(pgx.ErrNoRows)

	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load on missing row must not error: %v", err)
	}
	if got.ID != "singleton" || got.BackendMode != "configured_provider" {
		t.Errorf("expected schema defaults; got %+v", got)
	}
	if got.TimeoutMs != 5000 || got.CacheTTLSeconds != 600 {
		t.Errorf("default timeouts: %+v", got)
	}
}

// TestLoad_QueryError_Wraps drives the generic error branch through
// the public Load API.
func TestLoad_QueryError_Wraps(t *testing.T) {
	mock, store := newMockStore(t)
	want := errors.New("simulated planner error")
	mock.ExpectQuery(`FROM ai_guard_config`).WillReturnError(want)

	_, err := store.Load(context.Background())
	if err == nil {
		t.Fatal("expected wrapped error")
	}
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "configstore: load ai_guard_config") {
		t.Errorf("missing prefix: %v", err)
	}
}

// TestSave_PersistsAllFields_AndCallsExec drives the full Save SQL
// path. The pgxmock Exec assertion validates that every column on the
// AIGuardConfig is forwarded as a parameter — protecting against the
// silent-drop class of bug where a new field is added to the struct
// but forgotten in the INSERT/UPDATE pair.
func TestSave_PersistsAllFields_AndCallsExec(t *testing.T) {
	mock, store := newMockStore(t)
	cfg := &configstore.AIGuardConfig{
		ID:                 "singleton",
		BackendMode:        "external_url",
		ProviderID:         strPtr("prov-1"),
		ModelID:            strPtr("model-1"),
		ExternalURL:        strPtr("https://judge.example.com/v1"),
		CustomHeaders:      map[string]any{"X-Tenant": "nexus"},
		PromptTemplate:     "custom template",
		TimeoutMs:          3000,
		CacheTTLSeconds:    120,
		BackendFingerprint: "fp-abc",
	}
	// pgxmock matches arguments by deep-equality; for the JSONB
	// headers column we accept any bytes the production marshaler
	// produces.
	mock.ExpectExec(`INSERT INTO ai_guard_config`).
		WithArgs(
			cfg.ID, cfg.BackendMode, cfg.ProviderID, cfg.ModelID,
			cfg.ExternalURL,
			pgxmock.AnyArg(), // custom_headers JSONB bytes
			cfg.PromptTemplate, cfg.TimeoutMs, cfg.CacheTTLSeconds, cfg.BackendFingerprint,
			cfg.InputStrategy, cfg.ModelContextLimit,
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := store.Save(context.Background(), cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestSave_NilHeaders_PassesNilBytes pins the marshalAIGuardHeaders
// nil-versus-empty contract through the public Save API: a nil headers
// map MUST reach Exec as a nil bytes payload so the JSONB column
// stores SQL NULL (not the JSON literal `null`).
func TestSave_NilHeaders_PassesNilBytes(t *testing.T) {
	mock, store := newMockStore(t)
	cfg := &configstore.AIGuardConfig{
		ID:                 "singleton",
		BackendMode:        "configured_provider",
		PromptTemplate:     "",
		TimeoutMs:          5000,
		CacheTTLSeconds:    600,
		BackendFingerprint: "fp-default",
	}
	mock.ExpectExec(`INSERT INTO ai_guard_config`).
		WithArgs(
			cfg.ID, cfg.BackendMode,
			(*string)(nil), (*string)(nil), (*string)(nil),
			[]byte(nil), // nil-headers must round-trip as a nil byte slice, not `null`
			cfg.PromptTemplate, cfg.TimeoutMs, cfg.CacheTTLSeconds, cfg.BackendFingerprint,
			cfg.InputStrategy, cfg.ModelContextLimit,
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := store.Save(context.Background(), cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// TestSave_ExecError_Wraps surfaces the post-Exec error-wrap branch:
// any DB-side failure must surface a "configstore: save
// ai_guard_config:" prefix so admin logs can attribute the failure.
func TestSave_ExecError_Wraps(t *testing.T) {
	mock, store := newMockStore(t)
	want := errors.New("constraint violation")
	mock.ExpectExec(`INSERT INTO ai_guard_config`).
		WithArgs(
			"singleton", "configured_provider",
			(*string)(nil), (*string)(nil), (*string)(nil),
			[]byte(nil),
			"", 0, 0, "",
			"", 0, // input_strategy, model_context_limit
		).
		WillReturnError(want)

	err := store.Save(context.Background(), &configstore.AIGuardConfig{
		ID: "singleton", BackendMode: "configured_provider",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "configstore: save ai_guard_config") {
		t.Errorf("missing prefix: %v", err)
	}
}

// TestNewAIGuardStore_AcceptsProductionPool exercises the production
// constructor (NewAIGuardStore takes *pgxpool.Pool concretely). We
// can't open a real pool in a unit test, but constructing with a nil
// pool is enough to cover the constructor's two statements — the
// store value is opaque so we just assert it is non-nil.
func TestNewAIGuardStore_AcceptsProductionPool(t *testing.T) {
	store := configstore.NewAIGuardStore(nil)
	if store == nil {
		t.Fatal("NewAIGuardStore returned nil")
	}
}
