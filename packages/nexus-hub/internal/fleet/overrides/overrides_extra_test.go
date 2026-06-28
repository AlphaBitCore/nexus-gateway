// Extra tests for fleet/overrides package covering previously uncovered functions:
// MarshalJSON, UnmarshalJSON (OverrideState), GetOverride (not-found path),
// ListOverridesByThing (happy + scan + rows.Err), ListAllOverrides (all filter
// branches + happy path + row errors), ListExpiredOverrides (happy path + rows.Err),
// decodeJSONB.

package overrides

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
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

// OverrideState MarshalJSON / UnmarshalJSON

func TestOverrideState_MarshalJSON_ZeroValue(t *testing.T) {
	var s OverrideState
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(b) != "null" {
		t.Errorf("zero value marshaled = %q, want null", string(b))
	}
}

func TestOverrideState_MarshalJSON_WithValue(t *testing.T) {
	s, err := NewOverrideState([]byte(`{"enabled":true}`))
	if err != nil {
		t.Fatalf("NewOverrideState: %v", err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if !strings.Contains(string(b), "enabled") {
		t.Errorf("marshaled = %q, want to contain 'enabled'", string(b))
	}
}

func TestOverrideState_UnmarshalJSON_ValidObject(t *testing.T) {
	var s OverrideState
	if err := json.Unmarshal([]byte(`{"enabled":true}`), &s); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if len(s.Bytes()) == 0 {
		t.Error("expected non-empty bytes after unmarshal")
	}
}

func TestOverrideState_UnmarshalJSON_Empty(t *testing.T) {
	var s OverrideState
	err := json.Unmarshal([]byte(`""`), &s)
	if err == nil {
		t.Fatal("expected error for non-object JSON")
	}
}

func TestOverrideState_UnmarshalJSON_NullRejected(t *testing.T) {
	var s OverrideState
	err := json.Unmarshal([]byte(`null`), &s)
	// null is well-formed JSON but top-level type is null, not object.
	if err == nil {
		t.Fatal("expected error for null JSON (not an object)")
	}
}

func TestOverrideState_RoundTrip(t *testing.T) {
	original := `{"k":"v","n":42}`
	s, err := NewOverrideState([]byte(original))
	if err != nil {
		t.Fatalf("NewOverrideState: %v", err)
	}
	marshaled, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back OverrideState
	if err := json.Unmarshal(marshaled, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// Bytes should round-trip without corruption.
	if string(back.Bytes()) != original {
		t.Errorf("round-trip: got %q, want %q", string(back.Bytes()), original)
	}
}

// GetOverride — not-found path (ErrNoRows → ErrNotFound)

func TestGetOverride_NotFound(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM thing_config_override`).
		WithArgs("thing-1", "missing-key").
		WillReturnError(pgx.ErrNoRows)

	s := New(mock)
	_, err := s.GetOverride(context.Background(), "thing-1", "missing-key")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ListOverridesByThing — happy path, scan error, rows.Err

func TestListOverridesByThing_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	reason := "test reason"
	mock.ExpectQuery(`FROM thing_config_override tco`).
		WithArgs("thing-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
			"current_template_ver", "stale",
		}).AddRow(
			"thing-1", "hook_config", []byte(`{"enabled":true}`), int64(2),
			"actor-1", ts, &reason, (*time.Time)(nil), false,
			int64(2), false,
		))

	store := New(mock)
	overrides, err := store.ListOverridesByThing(context.Background(), "thing-1")
	if err != nil {
		t.Fatalf("ListOverridesByThing: %v", err)
	}
	if len(overrides) != 1 {
		t.Fatalf("got %d overrides, want 1", len(overrides))
	}
	if overrides[0].ConfigKey != "hook_config" {
		t.Errorf("ConfigKey = %q, want hook_config", overrides[0].ConfigKey)
	}
	if overrides[0].Stale {
		t.Error("expected not stale (ver == ver_at_set)")
	}
}

func TestListOverridesByThing_RowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("rows iter err")
	mock.ExpectQuery(`FROM thing_config_override tco`).
		WithArgs("thing-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
			"current_template_ver", "stale",
		}).CloseError(sentinel))

	store := New(mock)
	_, err := store.ListOverridesByThing(context.Background(), "thing-1")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// ListAllOverrides — all filter branches + happy path + row errors

func TestListAllOverrides_NoFilters_EmptyResult(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Row query: LIMIT=50, OFFSET=0.
	mock.ExpectQuery(`FROM thing_config_override tco`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
			"current_template_ver", "stale", "thing_name", "thing_type",
		}))
	// Summary query: no filter args.
	mock.ExpectQuery(`COUNT\(\*\) AS total_overrides`).
		WillReturnRows(pgxmock.NewRows([]string{
			"total_overrides", "total_nodes", "stale_count", "expiring_soon",
		}).AddRow(int64(0), int64(0), int64(0), int64(0)))

	store := New(mock)
	rows, total, summary, err := store.ListAllOverrides(context.Background(), ListOverridesFilter{})
	if err != nil {
		t.Fatalf("ListAllOverrides: %v", err)
	}
	if total != 0 || len(rows) != 0 {
		t.Errorf("expected empty, got total=%d rows=%d", total, len(rows))
	}
	if summary.TotalOverrides != 0 {
		t.Errorf("summary.TotalOverrides = %d, want 0", summary.TotalOverrides)
	}
}

func TestListAllOverrides_WithAllFilters(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	thingType := "agent"
	actor := "actor-1"
	hasTTL := true
	staleFilter := true

	// Row query with thingType, actor, LIMIT=50, OFFSET=0.
	// HasTTL and Stale add SQL conditions but no args.
	mock.ExpectQuery(`FROM thing_config_override tco`).
		WithArgs(thingType, actor, 50, 0).
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
			"current_template_ver", "stale", "thing_name", "thing_type",
		}))
	// Summary query with same filter args but no LIMIT/OFFSET.
	mock.ExpectQuery(`COUNT\(\*\) AS total_overrides`).
		WithArgs(thingType, actor).
		WillReturnRows(pgxmock.NewRows([]string{
			"total_overrides", "total_nodes", "stale_count", "expiring_soon",
		}).AddRow(int64(0), int64(0), int64(0), int64(0)))

	store := New(mock)
	_, _, _, err := store.ListAllOverrides(context.Background(), ListOverridesFilter{
		ThingType: &thingType,
		Actor:     &actor,
		HasTTL:    &hasTTL,
		Stale:     &staleFilter,
	})
	if err != nil {
		t.Fatalf("ListAllOverrides all filters: %v", err)
	}
}

func TestListAllOverrides_HasTTLFalse_StaleFalse(t *testing.T) {
	// Covers the `!*HasTTL` and `!*Stale` branches.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	hasTTL := false
	staleFilter := false

	mock.ExpectQuery(`FROM thing_config_override tco`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
			"current_template_ver", "stale", "thing_name", "thing_type",
		}))
	mock.ExpectQuery(`COUNT\(\*\) AS total_overrides`).
		WillReturnRows(pgxmock.NewRows([]string{
			"total_overrides", "total_nodes", "stale_count", "expiring_soon",
		}).AddRow(int64(0), int64(0), int64(0), int64(0)))

	store := New(mock)
	_, _, _, err := store.ListAllOverrides(context.Background(), ListOverridesFilter{
		HasTTL: &hasTTL,
		Stale:  &staleFilter,
	})
	if err != nil {
		t.Fatalf("ListAllOverrides HasTTL=false Stale=false: %v", err)
	}
}

func TestListAllOverrides_RowQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("row query err")
	mock.ExpectQuery(`FROM thing_config_override tco`).
		WithArgs(50, 0).
		WillReturnError(sentinel)

	store := New(mock)
	_, _, _, err := store.ListAllOverrides(context.Background(), ListOverridesFilter{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "list all overrides") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestListAllOverrides_RowsIterErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("iter err")
	mock.ExpectQuery(`FROM thing_config_override tco`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
			"current_template_ver", "stale", "thing_name", "thing_type",
		}).CloseError(sentinel))

	store := New(mock)
	_, _, _, err := store.ListAllOverrides(context.Background(), ListOverridesFilter{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "iterate override list rows") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestListAllOverrides_SummaryQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM thing_config_override tco`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
			"current_template_ver", "stale", "thing_name", "thing_type",
		}))
	sentinel := errors.New("summary err")
	mock.ExpectQuery(`COUNT\(\*\) AS total_overrides`).
		WillReturnError(sentinel)

	store := New(mock)
	_, _, _, err := store.ListAllOverrides(context.Background(), ListOverridesFilter{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "summary overrides") {
		t.Errorf("missing prefix: %v", err)
	}
}

// ListExpiredOverrides — happy path + rows.Err

func TestListExpiredOverrides_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	expiredAt := ts.Add(-1 * time.Hour)
	before := ts

	mock.ExpectQuery(`FROM thing_config_override\s+WHERE expires_at IS NOT NULL`).
		WithArgs(before).
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow(
			"thing-1", "hook_config", []byte(`{"enabled":false}`), int64(1),
			"actor-1", ts, (*string)(nil), &expiredAt, false,
		))

	store := New(mock)
	expired, err := store.ListExpiredOverrides(context.Background(), before)
	if err != nil {
		t.Fatalf("ListExpiredOverrides: %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("got %d expired, want 1", len(expired))
	}
	if expired[0].ThingID != "thing-1" {
		t.Errorf("ThingID = %q, want thing-1", expired[0].ThingID)
	}
}

// NewOverrideState — missing branches

func TestNewOverrideState_InvalidJSONDecoderToken(t *testing.T) {
	// json.Valid passes for `[]` (an array), but top-level type is array not object.
	_, err := NewOverrideState([]byte(`[]`))
	if !errors.Is(err, ErrNonObjectState) {
		t.Fatalf("err = %v, want ErrNonObjectState", err)
	}
}

func TestNewOverrideState_ScalarRejected(t *testing.T) {
	// A number is valid JSON but not an object.
	_, err := NewOverrideState([]byte(`42`))
	if !errors.Is(err, ErrNonObjectState) {
		t.Fatalf("err = %v, want ErrNonObjectState", err)
	}
}

// GetOverride — happy path (scan succeeds)

func TestGetOverride_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	ts := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_override`).
		WithArgs("thing-1", "hook_config").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow(
			"thing-1", "hook_config", []byte(`{"enabled":true}`), int64(3),
			"actor-1", ts, (*string)(nil), (*time.Time)(nil), false,
		))

	store := New(mock)
	o, err := store.GetOverride(context.Background(), "thing-1", "hook_config")
	if err != nil {
		t.Fatalf("GetOverride: %v", err)
	}
	if o.ThingID != "thing-1" || o.ConfigKey != "hook_config" {
		t.Errorf("got %+v", o)
	}
}

// UpsertOverride — happy path (exec succeeds)

func TestUpsertOverride_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs("thing-1", "hook_config", pgxmock.AnyArg(), int64(2),
			"actor-1", pgxmock.AnyArg(), pgxmock.AnyArg(), false).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	state, _ := NewOverrideState([]byte(`{"enabled":true}`))
	tx, _ := mock.Begin(context.Background())
	store := New(mock)
	err := store.UpsertOverride(context.Background(), tx, ThingConfigOverride{
		ThingID: "thing-1", ConfigKey: "hook_config", State: state,
		TemplateVerAtSet: 2, SetBy: "actor-1",
	})
	if err != nil {
		t.Fatalf("UpsertOverride: %v", err)
	}
	_ = tx.Commit(context.Background())
}

// DeleteOverride — happy path (row deleted = existed)

func TestDeleteOverride_RowDeleted(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM thing_config_override`).
		WithArgs("thing-1", "hook_config").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectCommit()

	tx, _ := mock.Begin(context.Background())
	store := New(mock)
	existed, err := store.DeleteOverride(context.Background(), tx, "thing-1", "hook_config")
	if err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}
	if !existed {
		t.Error("expected existed=true when a row was deleted")
	}
	_ = tx.Commit(context.Background())
}

func TestDeleteOverride_RowNotFound(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM thing_config_override`).
		WithArgs("thing-1", "missing").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectRollback()

	tx, _ := mock.Begin(context.Background())
	store := New(mock)
	existed, err := store.DeleteOverride(context.Background(), tx, "thing-1", "missing")
	if err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}
	if existed {
		t.Error("expected existed=false when no row deleted")
	}
	_ = tx.Rollback(context.Background())
}

// ListAllOverrides — happy path with rows returned

func TestListAllOverrides_WithRows(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_override tco`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
			"current_template_ver", "stale", "thing_name", "thing_type",
		}).AddRow(
			"thing-1", "hook_config", []byte(`{"enabled":true}`), int64(3),
			"actor-1", ts, (*string)(nil), (*time.Time)(nil), false,
			int64(3), false, "My Agent", "agent",
		))
	mock.ExpectQuery(`COUNT\(\*\) AS total_overrides`).
		WillReturnRows(pgxmock.NewRows([]string{
			"total_overrides", "total_nodes", "stale_count", "expiring_soon",
		}).AddRow(int64(1), int64(1), int64(0), int64(0)))

	store := New(mock)
	rows, total, summary, err := store.ListAllOverrides(context.Background(), ListOverridesFilter{})
	if err != nil {
		t.Fatalf("ListAllOverrides with rows: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Errorf("expected 1 row, got total=%d rows=%d", total, len(rows))
	}
	if rows[0].ThingName != "My Agent" {
		t.Errorf("ThingName = %q, want 'My Agent'", rows[0].ThingName)
	}
	if summary.TotalNodes != 1 {
		t.Errorf("TotalNodes = %d, want 1", summary.TotalNodes)
	}
}

func TestListExpiredOverrides_RowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("iter err")
	before := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE expires_at IS NOT NULL`).
		WithArgs(before).
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).CloseError(sentinel))

	store := New(mock)
	_, err := store.ListExpiredOverrides(context.Background(), before)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !strings.Contains(err.Error(), "iterate expired overrides") {
		t.Errorf("missing prefix: %v", err)
	}
}
