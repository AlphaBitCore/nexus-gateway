// packages/shared/storage/configstore/semantic_cache_test.go — pgxmock-driven
// unit tests for the SemanticCacheStore SQL paths.
//
// Structure mirrors aiguard_test.go: pgxmock replaces the live Postgres pool
// so tests run in CI without TEST_DATABASE_URL. Pure-decision-tree branches
// (ErrNoRows defaults, fingerprint computation, index version bump) live in
// semantic_cache_internal_test.go; this file covers the SQL surface.
//
// Nexus is single-tenant by design: the semantic_cache_config row is the
// fleet-wide singleton (id='singleton'); there is no org_id column.
package configstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// scColumns mirrors the SELECT column list in (s *SemanticCacheStore).Get.
// Keeping this in lockstep with the SQL is intentional — if Get adds a
// column, the tests fail loudly until columns and row data here are updated.
var scColumns = []string{
	"id", "embedding_provider_id", "embedding_model_id", "embedding_dimension",
	"embedding_fingerprint", "redis_index_name", "enabled",
	"threshold", "vary_by", "embed_strategy", "allow_cross_model",
	"updated_at", "updated_by", "time_sensitive_overrides",
	// Joined columns (provider.baseUrl, model.providerModelId, model.inputPricePerMillion)
	// — added when Get/Save started JOINing Provider + Model so the gateway
	// snapshot carries them directly. Tests append the three values to AddRow.
	"provider_base_url", "provider_model_id", "provider_input_price_per_m",
	"model_capability_json",
}

func newSemanticMockStore(t *testing.T) (pgxmock.PgxPoolIface, *configstore.SemanticCacheStore) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	// Save now issues the Provider/Model join BEFORE the upsert (it needs the
	// model capability to validate/derive the dimension). Match by SQL pattern
	// rather than call order so adding the join expectation doesn't require
	// reordering every existing Save test.
	mock.MatchExpectationsInOrder(false)
	return mock, configstore.NewSemanticCacheStoreWithPgxPool(mock)
}

// emptyOverridesJSON is the default value for time_sensitive_overrides.
func emptyOverridesJSON() []byte {
	b, _ := json.Marshal(configstore.TimeSensitiveOverridesBlob{})
	return b
}

// TestSemanticGet_HappyPath_SeededRow drives a fully-populated singleton
// row through Scan. Exercises every column mapping so a future column added
// without updating Scan is caught here.
func TestSemanticGet_HappyPath_SeededRow(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	prov := "prov-uuid-1"
	model := "model-uuid-1"
	dim := 1536
	by := "admin@nexus.ai"
	now := time.Now().UTC().Truncate(time.Second)

	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows(scColumns).AddRow(
			"singleton", &prov, &model, &dim,
			"abc123fingerprint", "nexus:semantic-cache:v1", true, 0.96, "vk", "system_plus_last_user", false, now, &by,
			emptyOverridesJSON(),
			"https://api.openai.com", "text-embedding-3-small", 0.02, "",
		))

	got, err := store.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "singleton" {
		t.Errorf("ID: %q", got.ID)
	}
	if got.EmbeddingProviderID == nil || *got.EmbeddingProviderID != prov {
		t.Errorf("EmbeddingProviderID: %v", got.EmbeddingProviderID)
	}
	if got.EmbeddingModelID == nil || *got.EmbeddingModelID != model {
		t.Errorf("EmbeddingModelID: %v", got.EmbeddingModelID)
	}
	if got.EmbeddingDimension == nil || *got.EmbeddingDimension != dim {
		t.Errorf("EmbeddingDimension: %v", got.EmbeddingDimension)
	}
	if got.EmbeddingFingerprint != "abc123fingerprint" {
		t.Errorf("EmbeddingFingerprint: %q", got.EmbeddingFingerprint)
	}
	if got.RedisIndexName != "nexus:semantic-cache:v1" {
		t.Errorf("RedisIndexName: %q", got.RedisIndexName)
	}
	if !got.Enabled {
		t.Error("Enabled: want true")
	}
	if got.UpdatedBy == nil || *got.UpdatedBy != by {
		t.Errorf("UpdatedBy: %v", got.UpdatedBy)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestSemanticGet_NoRows_ReturnsDefaults drives the pgx.ErrNoRows branch
// through the real *SemanticCacheStore.Get (the internal test covers
// finalizeSemanticCacheGet directly; this covers the QueryRow integration).
func TestSemanticGet_NoRows_ReturnsDefaults(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	mock.ExpectQuery(`FROM semantic_cache_config`).WillReturnError(pgx.ErrNoRows)

	got, err := store.Get(context.Background())
	if err != nil {
		t.Fatalf("Get on missing row must not error: %v", err)
	}
	if got.ID != "singleton" {
		t.Errorf("ID: %q (want singleton)", got.ID)
	}
	if got.RedisIndexName != "nexus:semantic-cache:v1" {
		t.Errorf("RedisIndexName default: %q", got.RedisIndexName)
	}
	if got.Enabled {
		t.Error("Enabled default: want false")
	}
	if got.EmbeddingFingerprint != "" {
		t.Errorf("EmbeddingFingerprint default: %q (want empty)", got.EmbeddingFingerprint)
	}
}

// TestSemanticGet_QueryError_Wraps drives the generic error branch through
// the public Get API.
func TestSemanticGet_QueryError_Wraps(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	want := errors.New("simulated planner error")
	mock.ExpectQuery(`FROM semantic_cache_config`).WillReturnError(want)

	_, err := store.Get(context.Background())
	if err == nil {
		t.Fatal("expected wrapped error")
	}
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "configstore: load semantic_cache_config") {
		t.Errorf("missing prefix: %v", err)
	}
}

// TestSemanticSave_NoFingerprintChange_IndexStable verifies that an
// enabled-only change (same provider, model, dim) does NOT bump the
// redis_index_name. This is the fleet kill-switch stability invariant.
func TestSemanticSave_NoFingerprintChange_IndexStable(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	prov := "prov-1"
	model := "model-1"
	dim := 1536
	by := "admin@nexus.ai"
	now := time.Now().UTC()

	// First: Get returns current row (fingerprint already set for prov+model+dim).
	existingFP := "053e187b0a8cda20152ba26c26cc29f85e7df751fa362465ac3995b45d6d91f2"
	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows(scColumns).AddRow(
			"singleton", &prov, &model, &dim,
			existingFP, "nexus:semantic-cache:v1", false, 0.96, "vk", "system_plus_last_user", false, now, &by,
			emptyOverridesJSON(),
			"https://api.openai.com", "text-embedding-3-small", 0.02, "",
		))

	// Save with same (prov, model, dim) but enabled=true — fingerprint
	// matches, so index name must stay at v1.
	mock.ExpectExec(`INSERT INTO semantic_cache_config`).
		WithArgs(
			&prov, &model, &dim,
			pgxmock.AnyArg(),          // fingerprint (same as existing)
			"nexus:semantic-cache:v1", // index name must NOT be bumped
			true,
			pgxmock.AnyArg(), // threshold
			pgxmock.AnyArg(), // vary_by
			pgxmock.AnyArg(), // embed_strategy
			pgxmock.AnyArg(), // allow_cross_model
			pgxmock.AnyArg(), // updated_by
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	saved, err := store.Save(context.Background(), configstore.SaveInput{
		EmbeddingProviderID: &prov,
		EmbeddingModelID:    &model,
		EmbeddingDimension:  &dim,
		Enabled:             true,
		UpdatedBy:           by,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if saved.RedisIndexName != "nexus:semantic-cache:v1" {
		t.Errorf("index name bumped unexpectedly: %q", saved.RedisIndexName)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestSemanticSave_FingerprintChange_IndexBumped verifies that swapping the
// embedding model (different fingerprint) bumps redis_index_name from v1→v2.
func TestSemanticSave_FingerprintChange_IndexBumped(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	oldProv := "prov-1"
	oldModel := "model-old"
	oldDim := 1536
	oldBy := "admin@nexus.ai"
	now := time.Now().UTC()

	// Get returns row with old fingerprint.
	oldFP := "old-fingerprint-value"
	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows(scColumns).AddRow(
			"singleton", &oldProv, &oldModel, &oldDim,
			oldFP, "nexus:semantic-cache:v1", true, 0.96, "vk", "system_plus_last_user", false, now, &oldBy,
			emptyOverridesJSON(),
			"https://api.openai.com", "text-embedding-3-small", 0.02, "",
		))

	// Save with a new model (different fingerprint) — index name MUST be bumped.
	newProv := "prov-1"
	newModel := "model-new"
	newDim := 3072
	mock.ExpectExec(`INSERT INTO semantic_cache_config`).
		WithArgs(
			&newProv, &newModel, &newDim,
			pgxmock.AnyArg(),          // new fingerprint
			"nexus:semantic-cache:v2", // MUST be bumped from v1 → v2
			true,
			pgxmock.AnyArg(), // threshold
			pgxmock.AnyArg(), // vary_by
			pgxmock.AnyArg(), // embed_strategy
			pgxmock.AnyArg(), // allow_cross_model
			pgxmock.AnyArg(),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	saved, err := store.Save(context.Background(), configstore.SaveInput{
		EmbeddingProviderID: &newProv,
		EmbeddingModelID:    &newModel,
		EmbeddingDimension:  &newDim,
		Enabled:             true,
		UpdatedBy:           "admin@nexus.ai",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if saved.RedisIndexName != "nexus:semantic-cache:v2" {
		t.Errorf("index name not bumped: %q (want nexus:semantic-cache:v2)", saved.RedisIndexName)
	}
	if saved.EmbeddingFingerprint == "" || saved.EmbeddingFingerprint == oldFP {
		t.Errorf("fingerprint not updated: %q", saved.EmbeddingFingerprint)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestSemanticSave_NilProvider_EmptyFingerprint_IndexUnchanged verifies that
// sending a nil providerID (disabled config) produces an empty fingerprint
// and does NOT bump the index name.
func TestSemanticSave_NilProvider_EmptyFingerprint_IndexUnchanged(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	now := time.Now().UTC()
	existingFP := "some-old-fp"
	oldIdx := "nexus:semantic-cache:v3"

	// Current row has some fp + v3 name.
	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows(scColumns).AddRow(
			"singleton", (*string)(nil), (*string)(nil), (*int)(nil),
			existingFP, oldIdx, true, 0.96, "vk", "system_plus_last_user", false, now, (*string)(nil),
			emptyOverridesJSON(),
			"https://api.openai.com", "text-embedding-3-small", 0.02, "",
		))

	// Save with nil provider — fingerprint = "" → index stays at v3.
	mock.ExpectExec(`INSERT INTO semantic_cache_config`).
		WithArgs(
			(*string)(nil), (*string)(nil), (*int)(nil),
			"",     // empty fingerprint
			oldIdx, // index unchanged
			false,
			pgxmock.AnyArg(), // threshold
			pgxmock.AnyArg(), // vary_by
			pgxmock.AnyArg(), // embed_strategy
			pgxmock.AnyArg(), // allow_cross_model
			(*string)(nil),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	saved, err := store.Save(context.Background(), configstore.SaveInput{
		Enabled: false,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if saved.EmbeddingFingerprint != "" {
		t.Errorf("fingerprint should be empty: %q", saved.EmbeddingFingerprint)
	}
	if saved.RedisIndexName != oldIdx {
		t.Errorf("index name changed unexpectedly: %q", saved.RedisIndexName)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestSemanticSave_ExecError_Wraps surfaces the post-Exec error-wrap branch.
func TestSemanticSave_ExecError_Wraps(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	want := errors.New("constraint violation")

	// Get succeeds (default row — fresh DB).
	mock.ExpectQuery(`FROM semantic_cache_config`).WillReturnError(pgx.ErrNoRows)

	mock.ExpectExec(`INSERT INTO semantic_cache_config`).
		WithArgs(
			(*string)(nil), (*string)(nil), (*int)(nil),
			"", "nexus:semantic-cache:v1", false,
			pgxmock.AnyArg(), // threshold
			pgxmock.AnyArg(), // vary_by
			pgxmock.AnyArg(), // embed_strategy
			pgxmock.AnyArg(), // allow_cross_model
			(*string)(nil),
		).
		WillReturnError(want)

	_, err := store.Save(context.Background(), configstore.SaveInput{Enabled: false})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "configstore: save semantic_cache_config") {
		t.Errorf("missing prefix: %v", err)
	}
}

// TestSemanticSave_GetError_PropagatesBeforeExec verifies that when the
// pre-save Get call fails, Save returns early without calling Exec.
func TestSemanticSave_GetError_PropagatesBeforeExec(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	getErr := errors.New("DB connection lost")
	mock.ExpectQuery(`FROM semantic_cache_config`).WillReturnError(getErr)

	_, err := store.Save(context.Background(), configstore.SaveInput{Enabled: true})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, getErr) {
		t.Errorf("must wrap original; got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestNewSemanticCacheStore_AcceptsProductionPool exercises the production
// constructor.
func TestNewSemanticCacheStore_AcceptsProductionPool(t *testing.T) {
	store := configstore.NewSemanticCacheStore(nil)
	if store == nil {
		t.Fatal("NewSemanticCacheStore returned nil")
	}
}

// TestSemanticSave_ReturnsPostSaveRow verifies the post-save row contains
// the computed fingerprint and the (possibly bumped) index name.
func TestSemanticSave_ReturnsPostSaveRow(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	now := time.Now().UTC()

	// Current row: empty fingerprint (first-time config).
	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows(scColumns).AddRow(
			"singleton", (*string)(nil), (*string)(nil), (*int)(nil),
			"", "nexus:semantic-cache:v1", false, 0.96, "vk", "system_plus_last_user", false, now, (*string)(nil),
			emptyOverridesJSON(),
			"https://api.openai.com", "text-embedding-3-small", 0.02, "",
		))

	prov := "prov-a"
	model := "model-b"
	dim := 768
	mock.ExpectExec(`INSERT INTO semantic_cache_config`).
		WithArgs(
			&prov, &model, &dim,
			pgxmock.AnyArg(),          // fingerprint
			"nexus:semantic-cache:v2", // bumped (was "", now non-empty → bump)
			true,
			pgxmock.AnyArg(), // threshold
			pgxmock.AnyArg(), // vary_by
			pgxmock.AnyArg(), // embed_strategy
			pgxmock.AnyArg(), // allow_cross_model
			pgxmock.AnyArg(),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	saved, err := store.Save(context.Background(), configstore.SaveInput{
		EmbeddingProviderID: &prov,
		EmbeddingModelID:    &model,
		EmbeddingDimension:  &dim,
		Enabled:             true,
		UpdatedBy:           "admin",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if saved.ID != "singleton" {
		t.Errorf("ID: %q", saved.ID)
	}
	if saved.EmbeddingFingerprint == "" {
		t.Error("EmbeddingFingerprint must be set after save")
	}
	if saved.RedisIndexName != "nexus:semantic-cache:v2" {
		t.Errorf("RedisIndexName: %q", saved.RedisIndexName)
	}
	if saved.UpdatedBy == nil || *saved.UpdatedBy != "admin" {
		t.Errorf("UpdatedBy: %v", saved.UpdatedBy)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestSemanticGetOverrides_EmptyBlob verifies GetOverrides returns an empty
// blob when time_sensitive_overrides is the schema default.
func TestSemanticGetOverrides_EmptyBlob(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows(scColumns).AddRow(
			"singleton", (*string)(nil), (*string)(nil), (*int)(nil),
			"", "nexus:semantic-cache:v1", false, 0.96, "vk", "system_plus_last_user", false, now, (*string)(nil),
			emptyOverridesJSON(),
			"https://api.openai.com", "text-embedding-3-small", 0.02, "",
		))

	blob, err := store.GetOverrides(context.Background())
	if err != nil {
		t.Fatalf("GetOverrides: %v", err)
	}
	if len(blob.Rules) != 0 {
		t.Errorf("expected empty rules, got %d", len(blob.Rules))
	}
}

// TestSemanticGetOverrides_WithRules verifies GetOverrides correctly parses
// a non-empty overrides blob from the DB.
func TestSemanticGetOverrides_WithRules(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	now := time.Now().UTC()

	overridesBlob := configstore.TimeSensitiveOverridesBlob{
		Rules: []configstore.TimeSensitiveOverrideRule{
			{ID: "weather", Keywords: []string{"rain"}, Enabled: false},
		},
	}
	rawOverrides, _ := json.Marshal(overridesBlob)

	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows(scColumns).AddRow(
			"singleton", (*string)(nil), (*string)(nil), (*int)(nil),
			"", "nexus:semantic-cache:v1", false, 0.96, "vk", "system_plus_last_user", false, now, (*string)(nil),
			rawOverrides,
			"", "", 0.0, "",
		))

	blob, err := store.GetOverrides(context.Background())
	if err != nil {
		t.Fatalf("GetOverrides: %v", err)
	}
	if len(blob.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(blob.Rules))
	}
	if blob.Rules[0].ID != "weather" {
		t.Errorf("rule ID: %q", blob.Rules[0].ID)
	}
	if blob.Rules[0].Enabled {
		t.Error("rule Enabled: want false")
	}
}

// TestSemanticSaveOverrides_Upserts verifies SaveOverrides issues the correct
// INSERT ... ON CONFLICT upsert with the marshaled blob.
func TestSemanticSaveOverrides_Upserts(t *testing.T) {
	mock, store := newSemanticMockStore(t)

	blob := configstore.TimeSensitiveOverridesBlob{
		Rules: []configstore.TimeSensitiveOverrideRule{
			{ID: "custom-rule", Keywords: []string{"foo"}, Enabled: true},
		},
	}

	mock.ExpectExec(`INSERT INTO semantic_cache_config`).
		WithArgs(
			pgxmock.AnyArg(), // marshaled blob
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := store.SaveOverrides(context.Background(), blob); err != nil {
		t.Fatalf("SaveOverrides: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestGetOverrides_LoadError_Propagates drives the error path in GetOverrides
// when the underlying Get fails.
func TestGetOverrides_LoadError_Propagates(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	want := errors.New("simulated DB error")
	mock.ExpectQuery(`FROM semantic_cache_config`).WillReturnError(want)

	_, err := store.GetOverrides(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, want) {
		t.Errorf("must wrap: %v", err)
	}
}

// TestGetGlobal_ScanError_Wraps exercises the parseSemanticCacheScan error path
// inside Get — corrupt JSONB in time_sensitive_overrides is silently ignored
// (the function treats it as empty) so the real scan error path is a Scan failure.
func TestGetGlobal_ScanError_Wraps(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	// Returning wrong column count forces a Scan error.
	mock.ExpectQuery(`FROM semantic_cache_config`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("singleton"))

	_, err := store.Get(context.Background())
	if err == nil {
		t.Fatal("expected scan error")
	}
	if !strings.Contains(err.Error(), "configstore: load semantic_cache_config") {
		t.Errorf("missing prefix: %v", err)
	}
}

// TestSemanticSaveOverrides_ExecError surfaces the error-wrap path.
func TestSemanticSaveOverrides_ExecError(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	want := errors.New("db: disk full")

	mock.ExpectExec(`INSERT INTO semantic_cache_config`).
		WithArgs(
			pgxmock.AnyArg(),
		).
		WillReturnError(want)

	err := store.SaveOverrides(context.Background(), configstore.TimeSensitiveOverridesBlob{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "configstore: save time_sensitive_overrides") {
		t.Errorf("missing prefix: %v", err)
	}
}

// TestSemanticSave_DerivesDimensionAndMaxTokens: nil dimension + a model whose
// capability declares a default_dimension → Save derives it and populates
// EmbeddingMaxInputTokens from the same capability.
func TestSemanticSave_DerivesDimensionAndMaxTokens(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	prov := "p"
	model := "m"
	by := "admin@nexus.ai"
	now := time.Now().UTC()
	capJSON := `{"embeddings":{"max_input_tokens":8191,"default_dimension":1536,"supported_dimensions":[512,1024,1536]}}`

	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows(scColumns).AddRow(
			"singleton", (*string)(nil), (*string)(nil), (*int)(nil),
			"", "nexus:semantic-cache:v1", false, 0.96, "vk", "system_plus_last_user", false, now, (*string)(nil),
			emptyOverridesJSON(), "", "", 0.0, "",
		))
	mock.ExpectQuery(`FROM "Provider" p, "Model" m`).WithArgs(prov, model).
		WillReturnRows(pgxmock.NewRows([]string{"b", "pm", "price", "cap"}).
			AddRow("https://api.openai.com", "text-embedding-3-small", 0.02, capJSON))
	mock.ExpectExec(`INSERT INTO semantic_cache_config`).
		WithArgs(&prov, &model, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), true,
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	saved, err := store.Save(context.Background(), configstore.SaveInput{
		EmbeddingProviderID: &prov, EmbeddingModelID: &model, EmbeddingDimension: nil, Enabled: true, UpdatedBy: by,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if saved.EmbeddingDimension == nil || *saved.EmbeddingDimension != 1536 {
		t.Fatalf("dimension should derive to default 1536; got %v", saved.EmbeddingDimension)
	}
	if saved.EmbeddingMaxInputTokens != 8191 {
		t.Fatalf("EmbeddingMaxInputTokens not populated from capability: %d", saved.EmbeddingMaxInputTokens)
	}
}

// TestSemanticSave_RejectsUnsupportedDimension: a supplied dimension the model
// cannot produce → ErrUnsupportedEmbeddingDimension, NO upsert.
func TestSemanticSave_RejectsUnsupportedDimension(t *testing.T) {
	mock, store := newSemanticMockStore(t)
	prov := "p"
	model := "m"
	now := time.Now().UTC()
	capJSON := `{"embeddings":{"default_dimension":1536,"supported_dimensions":[512,1024,1536]}}`

	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows(scColumns).AddRow(
			"singleton", (*string)(nil), (*string)(nil), (*int)(nil),
			"", "nexus:semantic-cache:v1", false, 0.96, "vk", "system_plus_last_user", false, now, (*string)(nil),
			emptyOverridesJSON(), "", "", 0.0, "",
		))
	mock.ExpectQuery(`FROM "Provider" p, "Model" m`).WithArgs(prov, model).
		WillReturnRows(pgxmock.NewRows([]string{"b", "pm", "price", "cap"}).
			AddRow("https://api.openai.com", "text-embedding-3-small", 0.02, capJSON))
	// No ExpectExec — validation must fail before the upsert.

	d3072 := 3072
	_, err := store.Save(context.Background(), configstore.SaveInput{
		EmbeddingProviderID: &prov, EmbeddingModelID: &model, EmbeddingDimension: &d3072, Enabled: true,
	})
	if !errors.Is(err, configstore.ErrUnsupportedEmbeddingDimension) {
		t.Fatalf("expected ErrUnsupportedEmbeddingDimension, got %v", err)
	}
}

// WireState is the contract the admin push AND the configreconcile drift loader
// both depend on (F-0102/F-0345): it must zero ONLY the wall-clock bookkeeping
// columns (UpdatedAt/UpdatedBy, which Save stamps from the Go clock and Get from
// DB NOW(), so they never byte-match) while preserving every functional field —
// else the watch would spuriously heal on each save (bookkeeping zeroed) or
// thrash forever (a functional field dropped).
func TestSemanticCacheConfigRow_WireState_zerosBookkeepingPreservesRest(t *testing.T) {
	providerID := "openai"
	dim := 1536
	updatedBy := "admin@example.com"
	row := &configstore.SemanticCacheConfigRow{
		ID:                            "singleton",
		EmbeddingProviderID:           &providerID,
		EmbeddingDimension:            &dim,
		EmbeddingFingerprint:          "fp-123",
		RedisIndexName:                "nexus:semantic-cache:v2",
		Enabled:                       true,
		Threshold:                     0.93,
		VaryBy:                        "org",
		EmbedStrategy:                 "recent_turns",
		AllowCrossModel:               true,
		UpdatedAt:                     time.Date(2026, 6, 7, 1, 2, 3, 0, time.UTC),
		UpdatedBy:                     &updatedBy,
		EmbeddingProviderBaseURL:      "https://api.openai.com",
		EmbeddingProviderModelID:      "text-embedding-3-small",
		EmbeddingInputPricePerMillion: 0.02,
		EmbeddingMaxInputTokens:       8191,
	}

	got := row.WireState()

	// Bookkeeping columns must be zeroed (the sole save/read clock-skew source).
	if !got.UpdatedAt.IsZero() {
		t.Errorf("WireState.UpdatedAt = %v, want zero", got.UpdatedAt)
	}
	if got.UpdatedBy != nil {
		t.Errorf("WireState.UpdatedBy = %v, want nil", *got.UpdatedBy)
	}
	// Every functional field must survive unchanged.
	if got.EmbeddingFingerprint != "fp-123" || got.RedisIndexName != "nexus:semantic-cache:v2" ||
		!got.Enabled || got.Threshold != 0.93 || got.VaryBy != "org" ||
		got.EmbedStrategy != "recent_turns" || !got.AllowCrossModel ||
		got.EmbeddingProviderBaseURL != "https://api.openai.com" ||
		got.EmbeddingProviderModelID != "text-embedding-3-small" ||
		got.EmbeddingInputPricePerMillion != 0.02 || got.EmbeddingMaxInputTokens != 8191 ||
		got.EmbeddingProviderID == nil || *got.EmbeddingProviderID != "openai" ||
		got.EmbeddingDimension == nil || *got.EmbeddingDimension != 1536 {
		t.Errorf("WireState dropped/altered a functional field: %+v", got)
	}
	// The original row must not be mutated (WireState returns a copy).
	if row.UpdatedAt.IsZero() || row.UpdatedBy == nil {
		t.Error("WireState mutated the receiver; must operate on a copy")
	}
}
