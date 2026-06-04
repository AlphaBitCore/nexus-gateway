package modelstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// These tests drive the modelstore against pgxmock (the package's PgxPool
// interface is satisfied by pgxmock.PgxPoolIface directly), so the SQL build,
// scan, filter, and error paths are exercised with no live database — the
// pattern the coverage program uses to retire the CP store-layer exemptions.

func strptr(s string) *string   { return &s }
func f64ptr(f float64) *float64 { return &f }
func intptr(i int) *int         { return &i }

// anyArgs builds n AnyArg matchers. pgxmock treats a missing WithArgs as
// "expect zero arguments", so queries whose exact args aren't the point of the
// assertion (the 20/22-placeholder INSERT/UPDATE) still need an explicit
// matcher — otherwise the expectation silently fails to match and the injected
// rows/error never run, making the test pass for the wrong reason.
func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

var modelCols = []string{
	"id", "code", "name", "description", "providerId", "providerModelId",
	"type", "features", "inputPricePerMillion", "outputPricePerMillion",
	"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
	"maxContextTokens", "maxOutputTokens", "status", "deprecationDate",
	"replacedBy", "aliases", "inputModalities", "outputModalities",
	"lifecycle", "capabilityJson", "enabled", "createdAt", "updatedAt",
}

// modelRowValues returns one row's 25 column values in scanModel order, for a
// fully-populated model so every nullable column exercises its non-nil scan.
func modelRowValues(id, code string) []any {
	now := time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)
	cap := json.RawMessage(`{"vision":true}`)
	return []any{
		id, code, "GPT-4o", strptr("desc"), "prov1", "gpt-4o",
		"chat", []string{"chat"}, f64ptr(2.5), f64ptr(10),
		f64ptr(1.25), f64ptr(5),
		intptr(128000), intptr(4096), "active", (*time.Time)(nil),
		(*string)(nil), []string{"alias1"}, []string{"text"}, []string{"text"},
		"ga", &cap, true, now, now,
	}
}

// modelRowNullSlices returns a row whose array columns are SQL NULL (Go nil
// slices), so the store's "NULL array → empty slice" normalisation is exercised.
func modelRowNullSlices(id, code string) []any {
	v := modelRowValues(id, code)
	v[7] = []string(nil)  // features
	v[17] = []string(nil) // aliases
	v[18] = []string(nil) // inputModalities
	v[19] = []string(nil) // outputModalities
	return v
}

func newMockStore(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	return New(mock), mock
}

func TestListModelsFlat_FiltersAndResults(t *testing.T) {
	store, mock := newMockStore(t)
	enabled := true
	p := ModelListParams{Q: "gpt", Type: "chat", Status: "active", Enabled: &enabled, ProviderID: "prov1", Limit: 20, Offset: 0}

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Model"`).
		WithArgs("%gpt%", "chat", "active", true, "prov1").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT m\..* FROM "Model" m`).
		WithArgs("%gpt%", "chat", "active", true, "prov1", 20, 0).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(modelRowValues("m1", "gpt-4o")...))

	models, total, err := store.ListModelsFlat(context.Background(), p)
	if err != nil {
		t.Fatalf("ListModelsFlat: %v", err)
	}
	if total != 1 || len(models) != 1 {
		t.Fatalf("want 1 model/total, got %d models total=%d", len(models), total)
	}
	if models[0].Code != "gpt-4o" || models[0].ProviderID != "prov1" {
		t.Fatalf("unexpected model: %+v", models[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestListModelsFlat_CountError(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, _, err := store.ListModelsFlat(context.Background(), ModelListParams{}); err == nil {
		t.Fatal("count error should surface")
	}
}

func TestListModelsFlat_DataQueryError(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT m\.`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("query boom"))
	if _, _, err := store.ListModelsFlat(context.Background(), ModelListParams{Limit: 10}); err == nil {
		t.Fatal("data query error should surface")
	}
}

func TestGetModel_FoundAndNotFound(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT .* FROM "Model" WHERE id = \$1`).
		WithArgs("m1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(modelRowValues("m1", "gpt-4o")...))
	got, err := store.GetModel(context.Background(), "m1")
	if err != nil || got == nil || got.ID != "m1" {
		t.Fatalf("GetModel found: got=%v err=%v", got, err)
	}

	// ErrNoRows → (nil, nil): a missing model is not an error to the caller.
	mock.ExpectQuery(`SELECT .* FROM "Model" WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)
	got, err = store.GetModel(context.Background(), "missing")
	if err != nil || got != nil {
		t.Fatalf("GetModel missing should be (nil,nil), got=%v err=%v", got, err)
	}
}

func TestGetModel_ScanError(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("m1").WillReturnError(errors.New("db down"))
	if _, err := store.GetModel(context.Background(), "m1"); err == nil {
		t.Fatal("scan/db error should surface")
	}
}

func TestListModelsByProvider(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`FROM "Model" WHERE "providerId" = \$1`).
		WithArgs("prov1").
		WillReturnRows(pgxmock.NewRows(modelCols).
			AddRow(modelRowValues("m1", "a")...).
			AddRow(modelRowValues("m2", "b")...))
	models, err := store.ListModelsByProvider(context.Background(), "prov1")
	if err != nil || len(models) != 2 {
		t.Fatalf("ListModelsByProvider: got %d err=%v", len(models), err)
	}

	mock.ExpectQuery(`FROM "Model" WHERE "providerId"`).WithArgs(anyArgs(1)...).WillReturnError(errors.New("boom"))
	if _, err := store.ListModelsByProvider(context.Background(), "p"); err == nil {
		t.Fatal("query error should surface")
	}
}

func TestCreateModel_AppliesDefaults(t *testing.T) {
	store, mock := newMockStore(t)
	// Caller omits Features/modalities/lifecycle; the store must default
	// Features=[], modalities=["text"], lifecycle="ga". Assert via WithArgs.
	mock.ExpectQuery(`INSERT INTO "Model"`).
		WithArgs(
			pgxmock.AnyArg(), "new-model", "New", (*string)(nil), "prov1", "pm", "chat",
			[]string{}, // Features defaulted
			(*float64)(nil), (*float64)(nil), (*float64)(nil), (*float64)(nil),
			(*int)(nil), (*int)(nil),
			[]string{},       // Aliases defaulted
			[]string{"text"}, // InputModalities defaulted
			[]string{"text"}, // OutputModalities defaulted
			"ga",             // Lifecycle defaulted
			(*json.RawMessage)(nil),
			false,
		).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(modelRowValues("new-id", "new-model")...))
	got, err := store.CreateModel(context.Background(), CreateModelParams{
		Code: "new-model", Name: "New", ProviderID: "prov1", ProviderModelID: "pm", Type: "chat",
	})
	if err != nil || got == nil || got.Code != "new-model" {
		t.Fatalf("CreateModel: got=%v err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestCreateModel_Error(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`INSERT INTO "Model"`).WithArgs(anyArgs(20)...).WillReturnError(errors.New("unique violation"))
	if _, err := store.CreateModel(context.Background(), CreateModelParams{Code: "x"}); err == nil {
		t.Fatal("insert error should surface")
	}
}

func TestUpdateModel_WithModalities(t *testing.T) {
	store, mock := newMockStore(t)
	in := []string{"text", "image"}
	out := []string{"text"}
	mock.ExpectQuery(`UPDATE "Model" SET`).
		WithArgs(anyArgs(22)...).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(modelRowValues("m1", "gpt-4o")...))
	got, err := store.UpdateModel(context.Background(), "m1", UpdateModelParams{
		Name: strptr("Renamed"), InputModalities: &in, OutputModalities: &out,
	})
	if err != nil || got == nil {
		t.Fatalf("UpdateModel: got=%v err=%v", got, err)
	}

	mock.ExpectQuery(`UPDATE "Model"`).WithArgs(anyArgs(22)...).WillReturnError(errors.New("boom"))
	if _, err := store.UpdateModel(context.Background(), "m1", UpdateModelParams{}); err == nil {
		t.Fatal("update error should surface")
	}
}

func TestListModelsGroupedByProvider(t *testing.T) {
	store, mock := newMockStore(t)
	p := GroupedModelsParams{ProviderID: "prov1", Q: "gpt", IncludeEmpty: false}

	provCols := []string{"id", "name", "displayName", "description", "adapter_type", "enabled", "model_count"}
	mock.ExpectQuery(`FROM "Provider" p WHERE p\.id = \$1`).
		WithArgs("prov1").
		WillReturnRows(pgxmock.NewRows(provCols).
			AddRow("prov1", "OpenAI", strptr("OpenAI"), strptr("d"), "openai", true, 2).
			AddRow("prov2", "Empty", (*string)(nil), (*string)(nil), "anthropic", true, 0)) // dropped: 0 models, IncludeEmpty=false
	mock.ExpectQuery(`FROM "Model" .*"providerId" = \$1`).
		WithArgs("prov1", "%gpt%").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(modelRowValues("m1", "gpt-4o")...))

	groups, err := store.ListModelsGroupedByProvider(context.Background(), p)
	if err != nil {
		t.Fatalf("grouped: %v", err)
	}
	if len(groups) != 1 || groups[0].Provider.ID != "prov1" || len(groups[0].Models) != 1 {
		t.Fatalf("want 1 provider with 1 model (prov2 dropped), got %+v", groups)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestListModelsGroupedByProvider_IncludeEmpty(t *testing.T) {
	store, mock := newMockStore(t)
	provCols := []string{"id", "name", "displayName", "description", "adapter_type", "enabled", "model_count"}
	mock.ExpectQuery(`FROM "Provider" p`).
		WillReturnRows(pgxmock.NewRows(provCols).AddRow("prov2", "Empty", (*string)(nil), (*string)(nil), "anthropic", true, 0))
	mock.ExpectQuery(`FROM "Model"`).
		WillReturnRows(pgxmock.NewRows(modelCols)) // no models
	groups, err := store.ListModelsGroupedByProvider(context.Background(), GroupedModelsParams{IncludeEmpty: true})
	if err != nil {
		t.Fatalf("grouped: %v", err)
	}
	if len(groups) != 1 || len(groups[0].Models) != 0 {
		t.Fatalf("IncludeEmpty should keep the empty provider with 0 models, got %+v", groups)
	}
}

func TestListModelsGroupedByProvider_ProviderQueryError(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider"`).WillReturnError(errors.New("boom"))
	if _, err := store.ListModelsGroupedByProvider(context.Background(), GroupedModelsParams{}); err == nil {
		t.Fatal("provider query error should surface")
	}
}

func TestListModelsGroupedByProvider_ModelQueryError(t *testing.T) {
	store, mock := newMockStore(t)
	provCols := []string{"id", "name", "displayName", "description", "adapter_type", "enabled", "model_count"}
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(provCols).AddRow("p1", "P", (*string)(nil), (*string)(nil), "openai", true, 1))
	mock.ExpectQuery(`FROM "Model"`).WillReturnError(errors.New("boom"))
	if _, err := store.ListModelsGroupedByProvider(context.Background(), GroupedModelsParams{}); err == nil {
		t.Fatal("model query error should surface")
	}
}

// TestScanModel_NullArraysDefaultToEmpty pins the normalisation contract: a
// model stored with NULL array columns reads back as empty (non-nil) slices,
// so JSON responses serialise `[]` not `null` and callers can range safely.
func TestScanModel_NullArraysDefaultToEmpty(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("m1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(modelRowNullSlices("m1", "c")...))
	m, err := store.GetModel(context.Background(), "m1")
	if err != nil || m == nil {
		t.Fatalf("GetModel: %v", err)
	}
	for name, got := range map[string][]string{
		"Features": m.Features, "Aliases": m.Aliases,
		"InputModalities": m.InputModalities, "OutputModalities": m.OutputModalities,
	} {
		if got == nil {
			t.Errorf("%s should normalise NULL → empty slice, got nil", name)
		}
	}
}

// TestScanModels_NullArrays covers the same normalisation on the multi-row path.
func TestScanModels_NullArrays(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`FROM "Model" WHERE "providerId"`).WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(modelRowNullSlices("m1", "c")...))
	models, err := store.ListModelsByProvider(context.Background(), "p1")
	if err != nil || len(models) != 1 {
		t.Fatalf("ListModelsByProvider: %v", err)
	}
	if models[0].Features == nil || models[0].Aliases == nil {
		t.Fatal("scanModels should normalise NULL arrays to empty slices")
	}
}

// TestScanModels_ScanError covers the row-scan failure path: a row whose value
// types don't match the destinations makes scanModels return a wrapped error.
func TestScanModels_ScanError(t *testing.T) {
	store, mock := newMockStore(t)
	bad := modelRowValues("m1", "c")
	bad[22] = "not-a-bool" // enabled is bool; a string fails the scan
	mock.ExpectQuery(`FROM "Model" WHERE "providerId"`).WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(bad...))
	if _, err := store.ListModelsByProvider(context.Background(), "p1"); err == nil {
		t.Fatal("a row that fails to scan should surface an error")
	}
}

// TestListModelsFlat_ScanError covers the scanModels failure arm of the flat
// listing (count succeeds, the data row fails to scan).
func TestListModelsFlat_ScanError(t *testing.T) {
	store, mock := newMockStore(t)
	bad := modelRowValues("m1", "c")
	bad[22] = "not-a-bool"
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT m\.`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(bad...))
	if _, _, err := store.ListModelsFlat(context.Background(), ModelListParams{Limit: 10}); err == nil {
		t.Fatal("a bad data row should surface a scan error")
	}
}

var provCols = []string{"id", "name", "displayName", "description", "adapter_type", "enabled", "model_count"}

// TestListModelsGroupedByProvider_ProviderScanError covers the provider-row
// scan failure (model_count returned as a non-int).
func TestListModelsGroupedByProvider_ProviderScanError(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(provCols).AddRow("p1", "P", (*string)(nil), (*string)(nil), "openai", true, "NaN"))
	if _, err := store.ListModelsGroupedByProvider(context.Background(), GroupedModelsParams{}); err == nil {
		t.Fatal("bad provider row should surface a scan error")
	}
}

// TestListModelsGroupedByProvider_ProviderRowsErr covers the pRows.Err() arm.
func TestListModelsGroupedByProvider_ProviderRowsErr(t *testing.T) {
	store, mock := newMockStore(t)
	rows := pgxmock.NewRows(provCols).
		AddRow("p1", "P", (*string)(nil), (*string)(nil), "openai", true, 1).
		RowError(0, errors.New("iteration boom"))
	mock.ExpectQuery(`FROM "Provider"`).WillReturnRows(rows)
	if _, err := store.ListModelsGroupedByProvider(context.Background(), GroupedModelsParams{}); err == nil {
		t.Fatal("provider rows iteration error should surface")
	}
}

// TestListModelsGroupedByProvider_ModelScanError covers the scanModels failure
// arm of the grouped path (providers OK, a model row fails to scan).
func TestListModelsGroupedByProvider_ModelScanError(t *testing.T) {
	store, mock := newMockStore(t)
	bad := modelRowValues("m1", "c")
	bad[22] = "not-a-bool"
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(provCols).AddRow("p1", "P", (*string)(nil), (*string)(nil), "openai", true, 1))
	mock.ExpectQuery(`FROM "Model"`).WillReturnRows(pgxmock.NewRows(modelCols).AddRow(bad...))
	if _, err := store.ListModelsGroupedByProvider(context.Background(), GroupedModelsParams{}); err == nil {
		t.Fatal("bad model row should surface a scan error")
	}
}

func TestDeleteModel(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "Model" WHERE id = \$1`).WithArgs("m1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := store.DeleteModel(context.Background(), "m1"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}

	// 0 rows affected → ErrNoRows (delete of a non-existent model).
	mock.ExpectExec(`DELETE FROM "Model"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := store.DeleteModel(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("delete of missing model should return ErrNoRows, got %v", err)
	}

	mock.ExpectExec(`DELETE FROM "Model"`).WillReturnError(errors.New("fk violation"))
	if err := store.DeleteModel(context.Background(), "m1"); err == nil {
		t.Fatal("exec error should surface")
	}
}
