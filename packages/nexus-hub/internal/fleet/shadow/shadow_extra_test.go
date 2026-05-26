// Extra tests for fleet/shadow package covering all previously-uncovered
// functions: InsertConfigChangeEvent, ListConfigHistory (all filter branches),
// GetConfigTemplates, GetConfigTemplate, ListConfigTemplateCatalog,
// UpsertConfigTemplate, UpsertConfigTemplateAt, notifyConfigChanged, decodeJSONB.

package shadow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func TestDecodeJSONB_Empty(t *testing.T) {
	var target map[string]any
	if err := decodeJSONB(nil, &target, "col"); err != nil {
		t.Fatalf("empty raw should be no-op; got %v", err)
	}
	if target != nil {
		t.Errorf("target should stay nil; got %v", target)
	}
}

func TestDecodeJSONB_ValidJSON(t *testing.T) {
	var target map[string]any
	if err := decodeJSONB([]byte(`{"key":"val"}`), &target, "col"); err != nil {
		t.Fatalf("valid JSON: %v", err)
	}
	if target["key"] != "val" {
		t.Errorf("decoded value: %v", target)
	}
}

func TestDecodeJSONB_InvalidJSON(t *testing.T) {
	var target map[string]any
	err := decodeJSONB([]byte(`not-json`), &target, "my_col")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "decode my_col jsonb") {
		t.Errorf("missing prefix in error: %v", err)
	}
}

func TestNotifyConfigChanged_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_notify`).
		WithArgs(ConfigChangedChannel, "thing-abc").
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectCommit()

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := notifyConfigChanged(context.Background(), tx, "thing-abc"); err != nil {
		t.Fatalf("notifyConfigChanged: %v", err)
	}
	_ = tx.Commit(context.Background())
}

func TestNotifyConfigChanged_ExecError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	sentinel := errors.New("pg_notify failed")
	mock.ExpectExec(`SELECT pg_notify`).
		WithArgs(ConfigChangedChannel, "thing-xyz").
		WillReturnError(sentinel)
	mock.ExpectRollback()

	tx, _ := mock.Begin(context.Background())
	err := notifyConfigChanged(context.Background(), tx, "thing-xyz")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "pg_notify") {
		t.Errorf("missing pg_notify prefix: %v", err)
	}
	_ = tx.Rollback(context.Background())
}

func TestInsertConfigChangeEvent_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO config_change_event`).
		WithArgs(
			"agent", "hook_config", "update",
			"actor-1", "Alice", pgxmock.AnyArg(),
			int64(3), "", false,
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	tx, _ := mock.Begin(context.Background())
	s := New(mock)
	err := s.InsertConfigChangeEvent(context.Background(), tx, ConfigChangeEvent{
		ThingType:         "agent",
		ConfigKey:         "hook_config",
		Action:            "update",
		ActorID:           "actor-1",
		ActorName:         "Alice",
		NewState:          map[string]any{"enabled": true},
		NewVersion:        3,
		SourceIP:          "",
		EmergencyOverride: false,
	})
	if err != nil {
		t.Fatalf("InsertConfigChangeEvent: %v", err)
	}
	_ = tx.Commit(context.Background())
}

// TestInsertConfigChangeEvent_MarshalError pins the `json.Marshal(e.NewState)`
// error path. NewState is `any`, so a channel (which json.Marshal refuses to
// encode) is the simplest sentinel that hits this defensive guard.
func TestInsertConfigChangeEvent_MarshalError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	tx, _ := mock.Begin(context.Background())
	s := New(mock)
	err := s.InsertConfigChangeEvent(context.Background(), tx, ConfigChangeEvent{
		ThingType: "agent", ConfigKey: "k", Action: "set",
		NewState: make(chan int), // json.Marshal returns UnsupportedTypeError
	})
	if err == nil {
		t.Fatal("expected marshal error; got nil")
	}
	if !strings.Contains(err.Error(), "marshal state") {
		t.Errorf("missing prefix; got %q", err.Error())
	}
	_ = tx.Rollback(context.Background())
}

func TestInsertConfigChangeEvent_ExecError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	sentinel := errors.New("constraint violation")
	mock.ExpectExec(`INSERT INTO config_change_event`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	tx, _ := mock.Begin(context.Background())
	s := New(mock)
	err := s.InsertConfigChangeEvent(context.Background(), tx, ConfigChangeEvent{
		ThingType: "agent", ConfigKey: "k", Action: "set",
		NewState: map[string]any{},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "insert change event") {
		t.Errorf("missing prefix: %v", err)
	}
	_ = tx.Rollback(context.Background())
}

// ListConfigHistory (covers normalizeConfigHistoryParams + all filter branches)

func TestListConfigHistory_NoFilters(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM config_change_event`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT id, timestamp, thing_type, config_key`).
		WithArgs(50, 0). // LIMIT=50, OFFSET=0 (defaults)
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "timestamp", "thing_type", "config_key", "action",
			"actor_id", "actor_name", "new_state", "new_version", "source_ip", "emergency_override",
		}).AddRow("evt-1", ts, "agent", "hook_config", "update", "actor-1", "Alice", []byte(`{"k":"v"}`), int64(2), "1.2.3.4", false))

	s := New(mock)
	result, err := s.ListConfigHistory(context.Background(), ListConfigHistoryParams{})
	if err != nil {
		t.Fatalf("ListConfigHistory: %v", err)
	}
	if result.Total != 1 || len(result.Events) != 1 {
		t.Errorf("got total=%d events=%d, want 1/1", result.Total, len(result.Events))
	}
	if result.Events[0].ID != "evt-1" {
		t.Errorf("event ID = %q, want evt-1", result.Events[0].ID)
	}
}

func TestListConfigHistory_AllFilters(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	from := time.Now().UTC().Add(-24 * time.Hour)
	to := time.Now().UTC()

	// Count query gets: ThingType, ConfigKey, ActorID, From, To.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM config_change_event`).
		WithArgs("agent", "hook_config", "actor-1", from, to).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	// Row query gets same 5 filters + LIMIT + OFFSET.
	mock.ExpectQuery(`SELECT id, timestamp, thing_type, config_key`).
		WithArgs("agent", "hook_config", "actor-1", from, to, 10, 0).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "timestamp", "thing_type", "config_key", "action",
			"actor_id", "actor_name", "new_state", "new_version", "source_ip", "emergency_override",
		}))

	s := New(mock)
	result, err := s.ListConfigHistory(context.Background(), ListConfigHistoryParams{
		ThingType: "agent",
		ConfigKey: "hook_config",
		ActorID:   "actor-1",
		From:      &from,
		To:        &to,
		Page:      1,
		PageSize:  10,
	})
	if err != nil {
		t.Fatalf("ListConfigHistory all filters: %v", err)
	}
	if result.Total != 0 || len(result.Events) != 0 {
		t.Errorf("expected empty result")
	}
}

func TestListConfigHistory_DefaultPagination(t *testing.T) {
	// Page=0, PageSize=0 → default to Page=1, PageSize=50; PageSize>200 → 50.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM config_change_event`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	// The row query appends LIMIT + OFFSET as last two args.
	mock.ExpectQuery(`SELECT id, timestamp`).
		WithArgs(50, 0). // pageSize=50, offset=0
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "timestamp", "thing_type", "config_key", "action",
			"actor_id", "actor_name", "new_state", "new_version", "source_ip", "emergency_override",
		}))

	s := New(mock)
	// PageSize=0 → clamp to 50; Page=0 → clamp to 1.
	_, err := s.ListConfigHistory(context.Background(), ListConfigHistoryParams{Page: 0, PageSize: 0})
	if err != nil {
		t.Fatalf("ListConfigHistory defaults: %v", err)
	}
}

func TestListConfigHistory_CountQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("count err")
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM config_change_event`).
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.ListConfigHistory(context.Background(), ListConfigHistoryParams{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "count history") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestListConfigHistory_RowQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM config_change_event`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(5))
	sentinel := errors.New("row query err")
	mock.ExpectQuery(`SELECT id, timestamp`).
		WithArgs(50, 0). // pageSize=50, offset=0
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.ListConfigHistory(context.Background(), ListConfigHistoryParams{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "list history") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestListConfigHistory_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM config_change_event`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT id, timestamp`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "timestamp", "thing_type", "config_key", "action",
			"actor_id", "actor_name", "new_state", "new_version", "source_ip", "emergency_override",
		}).AddRow("evt-1", "WRONG_TYPE_NOT_TIME", "agent", "k", "set", "a", "b", []byte(`{}`), int64(1), "", false))

	s := New(mock)
	_, err := s.ListConfigHistory(context.Background(), ListConfigHistoryParams{})
	if err == nil {
		t.Fatal("expected scan error")
	}
}

func TestGetConfigTemplates_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	ts := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hook_config", []byte(`{"enabled":true}`), int64(2), ts, "actor-1").
			AddRow("agent", "quota", []byte(`{"limit":100}`), int64(1), ts, "actor-2"))

	s := New(mock)
	tpls, err := s.GetConfigTemplates(context.Background(), "agent")
	if err != nil {
		t.Fatalf("GetConfigTemplates: %v", err)
	}
	if len(tpls) != 2 {
		t.Fatalf("got %d templates, want 2", len(tpls))
	}
	if tpls[0].ConfigKey != "hook_config" {
		t.Errorf("first template key = %q, want hook_config", tpls[0].ConfigKey)
	}
}

func TestGetConfigTemplates_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("db down")
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent").
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.GetConfigTemplates(context.Background(), "agent")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "get config templates") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestGetConfigTemplates_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "k", []byte(`{}`), "WRONG_NOT_INT64", time.Now(), "a"))

	s := New(mock)
	_, err := s.GetConfigTemplates(context.Background(), "agent")
	if err == nil {
		t.Fatal("expected scan error")
	}
}

func TestGetConfigTemplates_EmptyResult(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}))

	s := New(mock)
	tpls, err := s.GetConfigTemplates(context.Background(), "agent")
	if err != nil {
		t.Fatalf("GetConfigTemplates empty: %v", err)
	}
	// nil is acceptable for empty (slice not initialized).
	if len(tpls) != 0 {
		t.Errorf("expected empty, got %d", len(tpls))
	}
}

func TestGetConfigTemplate_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	ts := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hook_config").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hook_config", []byte(`{"enabled":true}`), int64(5), ts, "actor-1"))

	s := New(mock)
	tpl, err := s.GetConfigTemplate(context.Background(), "agent", "hook_config")
	if err != nil {
		t.Fatalf("GetConfigTemplate: %v", err)
	}
	if tpl.ConfigKey != "hook_config" || tpl.Version != 5 {
		t.Errorf("unexpected tpl: %+v", tpl)
	}
}

func TestGetConfigTemplate_NotFound(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent", "missing-key").
		WillReturnError(pgx.ErrNoRows)

	s := New(mock)
	_, err := s.GetConfigTemplate(context.Background(), "agent", "missing-key")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetConfigTemplate_GenericError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("db error")
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent", "k").
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.GetConfigTemplate(context.Background(), "agent", "k")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "get config template") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestGetConfigTemplate_InvalidJSONB(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent", "k").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "k", []byte(`not-json`), int64(1), time.Now(), "actor"))

	s := New(mock)
	_, err := s.GetConfigTemplate(context.Background(), "agent", "k")
	if err == nil {
		t.Fatal("expected JSONB decode error")
	}
	if !strings.Contains(err.Error(), "decode state jsonb") {
		t.Errorf("missing decode prefix: %v", err)
	}
}

func TestListConfigTemplateCatalog_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT type, config_key\s+FROM thing_config_template`).
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key"}).
			AddRow("agent", "hook_config").
			AddRow("agent", "quota").
			AddRow("compliance-proxy", "active_exemptions"))

	s := New(mock)
	catalog, err := s.ListConfigTemplateCatalog(context.Background())
	if err != nil {
		t.Fatalf("ListConfigTemplateCatalog: %v", err)
	}
	if len(catalog) != 2 {
		t.Fatalf("got %d entries, want 2", len(catalog))
	}
	if catalog[0].ThingType != "agent" || len(catalog[0].ConfigKeys) != 2 {
		t.Errorf("agent entry: %+v", catalog[0])
	}
	if catalog[1].ThingType != "compliance-proxy" || len(catalog[1].ConfigKeys) != 1 {
		t.Errorf("compliance-proxy entry: %+v", catalog[1])
	}
}

func TestListConfigTemplateCatalog_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("catalog query error")
	mock.ExpectQuery(`SELECT type, config_key\s+FROM thing_config_template`).
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.ListConfigTemplateCatalog(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "list config template catalog") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestListConfigTemplateCatalog_RowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	rowErr := errors.New("iter err")
	// Use CloseError to trigger rows.Err() without any row data (clean path).
	mock.ExpectQuery(`SELECT type, config_key\s+FROM thing_config_template`).
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key"}).
			CloseError(rowErr))

	s := New(mock)
	_, err := s.ListConfigTemplateCatalog(context.Background())
	if !errors.Is(err, rowErr) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListConfigTemplateCatalog_Empty(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT type, config_key\s+FROM thing_config_template`).
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key"}))

	s := New(mock)
	catalog, err := s.ListConfigTemplateCatalog(context.Background())
	if err != nil {
		t.Fatalf("ListConfigTemplateCatalog empty: %v", err)
	}
	if len(catalog) != 0 {
		t.Errorf("expected empty catalog, got %d", len(catalog))
	}
}

func TestUpsertConfigTemplate_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "hook_config", pgxmock.AnyArg(), "actor-1").
		WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(3)))
	mock.ExpectCommit()

	tx, _ := mock.Begin(context.Background())
	s := New(mock)
	ver, err := s.UpsertConfigTemplate(context.Background(), tx, "agent", "hook_config", map[string]any{"enabled": true}, "actor-1")
	if err != nil {
		t.Fatalf("UpsertConfigTemplate: %v", err)
	}
	if ver != 3 {
		t.Errorf("ver = %d, want 3", ver)
	}
	_ = tx.Commit(context.Background())
}

func TestUpsertConfigTemplate_DBError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	sentinel := errors.New("upsert fail")
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "k", pgxmock.AnyArg(), "a").
		WillReturnError(sentinel)
	mock.ExpectRollback()

	tx, _ := mock.Begin(context.Background())
	s := New(mock)
	_, err := s.UpsertConfigTemplate(context.Background(), tx, "agent", "k", map[string]any{}, "a")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "upsert template") {
		t.Errorf("missing prefix: %v", err)
	}
	_ = tx.Rollback(context.Background())
}

func TestUpsertConfigTemplateAt_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "hook_config", pgxmock.AnyArg(), int64(7), "actor-1").
		WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(7)))
	mock.ExpectCommit()

	tx, _ := mock.Begin(context.Background())
	s := New(mock)
	ver, err := s.UpsertConfigTemplateAt(context.Background(), tx, "agent", "hook_config", map[string]any{"k": "v"}, 7, "actor-1")
	if err != nil {
		t.Fatalf("UpsertConfigTemplateAt: %v", err)
	}
	if ver != 7 {
		t.Errorf("ver = %d, want 7", ver)
	}
	_ = tx.Commit(context.Background())
}

func TestUpsertConfigTemplateAt_StaleVersionNoOp(t *testing.T) {
	// When EXCLUDED.version <= stored version the WHERE clause prevents an update.
	// QueryRow returns ErrNoRows → UpsertConfigTemplateAt returns ErrNotFound.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "k", pgxmock.AnyArg(), int64(1), "actor").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	tx, _ := mock.Begin(context.Background())
	s := New(mock)
	_, err := s.UpsertConfigTemplateAt(context.Background(), tx, "agent", "k", map[string]any{}, 1, "actor")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	_ = tx.Rollback(context.Background())
}

func TestUpsertConfigTemplateAt_DBError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	sentinel := errors.New("upsert-at fail")
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "k", pgxmock.AnyArg(), int64(5), "actor").
		WillReturnError(sentinel)
	mock.ExpectRollback()

	tx, _ := mock.Begin(context.Background())
	s := New(mock)
	_, err := s.UpsertConfigTemplateAt(context.Background(), tx, "agent", "k", map[string]any{}, 5, "actor")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "upsert template at 5") {
		t.Errorf("missing prefix: %v", err)
	}
	_ = tx.Rollback(context.Background())
}
