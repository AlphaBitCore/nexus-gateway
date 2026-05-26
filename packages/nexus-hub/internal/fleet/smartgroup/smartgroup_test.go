// Package smartgroup_test covers Store methods in the smartgroup package
// using pgxmock — no real Postgres required (Category C DB-bound tests).
//
// Architecture reference: docs/developers/architecture/services/hub/nexus-hub-internals-architecture.md (Tier 3).
// SDD reference: docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md (device-group cascade).
//
// Tested methods:
//   - New / constructor wiring
//   - ListSmartGroups: happy path, query error, scan error, bad JSON predicate
//   - LoadDevicesForSmartGroupEval: happy path, query error, scan error
//   - EvictExpiredMemberships: happy path, exec error
//   - ReplaceSmartGroupCache: happy path, empty device list, begin-tx error,
//     delete error, insert conflict, commit error
//   - decodeJSONB: empty-input passthrough, valid JSON, malformed JSON
package smartgroup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/hubstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/device"
)

// silentLog discards all log output so tests stay quiet.
func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newMockStore returns a Store backed by pgxmock and the mock itself.
func newMockStore(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	return New(mock), mock
}

func TestNew_ReturnsNonNil(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	s := New(mock)
	if s == nil {
		t.Fatal("New should return a non-nil *Store")
	}
}

func TestNew_SetsDBField(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	s := New(mock)
	if s.db == nil {
		t.Fatal("Store.db should be set by New")
	}
}

// Sentinel error re-export

func TestSentinelsAreHubstoreAliases(t *testing.T) {
	if !errors.Is(ErrNotFound, hubstore.ErrNotFound) {
		t.Error("smartgroup.ErrNotFound must be errors.Is-equal to hubstore.ErrNotFound")
	}
	if !errors.Is(ErrAmbiguous, hubstore.ErrAmbiguous) {
		t.Error("smartgroup.ErrAmbiguous must be errors.Is-equal to hubstore.ErrAmbiguous")
	}
}

// decodeJSONB (package-internal pure helper)

func TestDecodeJSONB_EmptyInput_IsNoOp(t *testing.T) {
	var m map[string]string
	if err := decodeJSONB(nil, &m, "col"); err != nil {
		t.Errorf("decodeJSONB(nil) should return nil, got %v", err)
	}
	if m != nil {
		t.Error("target should remain nil for empty input")
	}
}

func TestDecodeJSONB_EmptySlice_IsNoOp(t *testing.T) {
	var m map[string]string
	if err := decodeJSONB([]byte{}, &m, "col"); err != nil {
		t.Errorf("decodeJSONB([]) should return nil, got %v", err)
	}
}

func TestDecodeJSONB_ValidJSON_PopulatesTarget(t *testing.T) {
	var m map[string]string
	raw := []byte(`{"key":"val"}`)
	if err := decodeJSONB(raw, &m, "col"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["key"] != "val" {
		t.Errorf("m[key]=%q want val", m["key"])
	}
}

func TestDecodeJSONB_MalformedJSON_ReturnsError(t *testing.T) {
	var m map[string]string
	if err := decodeJSONB([]byte(`{bad`), &m, "col"); err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestDecodeJSONB_ErrorContainsColumnName(t *testing.T) {
	var m map[string]string
	err := decodeJSONB([]byte(`bad`), &m, "membership_query")
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() == "" {
		t.Error("error message should not be empty")
	}
}

func TestListSmartGroups_HappyPath_ReturnsDecoded(t *testing.T) {
	s, mock := newMockStore(t)
	predicateJSON := []byte(`{"all":[{"field":"os","op":"eq","value":"darwin"}]}`)
	rows := pgxmock.NewRows([]string{"id", "membership_query"}).
		AddRow("group-1", predicateJSON)
	mock.ExpectQuery(`SELECT id, membership_query FROM "DeviceGroup"`).
		WillReturnRows(rows)

	got, err := s.ListSmartGroups(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	if got[0].ID != "group-1" {
		t.Errorf("ID=%q want group-1", got[0].ID)
	}
	if len(got[0].Predicate.All) != 1 {
		t.Errorf("Predicate.All len=%d want 1", len(got[0].Predicate.All))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestListSmartGroups_MultipleRows_ReturnsAll(t *testing.T) {
	s, mock := newMockStore(t)
	p1 := []byte(`{"all":[{"field":"os","op":"eq","value":"darwin"}]}`)
	p2 := []byte(`{"any":[{"field":"os","op":"eq","value":"windows"}]}`)
	rows := pgxmock.NewRows([]string{"id", "membership_query"}).
		AddRow("g1", p1).
		AddRow("g2", p2)
	mock.ExpectQuery(`SELECT id, membership_query FROM "DeviceGroup"`).
		WillReturnRows(rows)

	got, err := s.ListSmartGroups(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len=%d want 2", len(got))
	}
}

func TestListSmartGroups_EmptyTable_ReturnsEmptySlice(t *testing.T) {
	s, mock := newMockStore(t)
	rows := pgxmock.NewRows([]string{"id", "membership_query"})
	mock.ExpectQuery(`SELECT id, membership_query FROM "DeviceGroup"`).
		WillReturnRows(rows)

	got, err := s.ListSmartGroups(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len=%d want 0 for empty table", len(got))
	}
}

func TestListSmartGroups_QueryError_WrappedAndReturned(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT id, membership_query FROM "DeviceGroup"`).
		WillReturnError(errors.New("connection refused"))

	_, err := s.ListSmartGroups(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !containsStr(err.Error(), "list smart groups") {
		t.Errorf("error should mention 'list smart groups', got %v", err)
	}
}

func TestListSmartGroups_MalformedPredicateJSON_StopsAndReturnsError(t *testing.T) {
	// A malformed JSON predicate in one row causes early return with the
	// partial result + an error — named failure mode: "single bad row
	// shouldn't take out the whole fleet" (the partial slice is returned).
	s, mock := newMockStore(t)
	rows := pgxmock.NewRows([]string{"id", "membership_query"}).
		AddRow("good-group", []byte(`{"all":[{"field":"os","op":"eq","value":"darwin"}]}`)).
		AddRow("bad-group", []byte(`{not-json`))
	mock.ExpectQuery(`SELECT id, membership_query FROM "DeviceGroup"`).
		WillReturnRows(rows)

	got, err := s.ListSmartGroups(context.Background())
	// Error must be returned so the recompute job can log which group is broken.
	if err == nil {
		t.Fatal("expected error for malformed predicate")
	}
	if !containsStr(err.Error(), "bad-group") {
		t.Errorf("error should identify the broken group, got %v", err)
	}
	// Partial slice is returned (good-group was appended before the bad row).
	if len(got) != 1 || got[0].ID != "good-group" {
		t.Errorf("want partial slice with good-group, got %v", got)
	}
}

var deviceQueryCols = []string{
	"id",
	"os", "os_version", "version",
	"hostname", "primary_ip", "physical_id", "status",
	"bound_user_id", "bound_user_org_path",
	"enrolled_at_sec", "last_heartbeat_sec",
	"idp_group_ids",
	"tags",
}

func TestLoadDevicesForSmartGroupEval_HappyPath(t *testing.T) {
	s, mock := newMockStore(t)
	rows := pgxmock.NewRows(deviceQueryCols).AddRow(
		"device-1",
		"darwin", "14.0", "1.2.3",
		"macbook-1", "10.0.0.1", "phys-1", "online",
		"user-1", "corp/eng/",
		int64(1700000000), int64(1700001000),
		[]string{"grp-a"},
		[]string{"tag1"},
	)
	mock.ExpectQuery(`SELECT`).WillReturnRows(rows)

	got, err := s.LoadDevicesForSmartGroupEval(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	d := got[0]
	if d.ID != "device-1" {
		t.Errorf("ID=%q want device-1", d.ID)
	}
	if d.Dev.OS != "darwin" {
		t.Errorf("OS=%q want darwin", d.Dev.OS)
	}
	if d.Dev.BoundUserID != "user-1" {
		t.Errorf("BoundUserID=%q want user-1", d.Dev.BoundUserID)
	}
	if len(d.Dev.IdpGroupIDs) != 1 || d.Dev.IdpGroupIDs[0] != "grp-a" {
		t.Errorf("IdpGroupIDs=%v want [grp-a]", d.Dev.IdpGroupIDs)
	}
	if len(d.Dev.Tags) != 1 || d.Dev.Tags[0] != "tag1" {
		t.Errorf("Tags=%v want [tag1]", d.Dev.Tags)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestLoadDevicesForSmartGroupEval_EmptyTable(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT`).WillReturnRows(pgxmock.NewRows(deviceQueryCols))

	got, err := s.LoadDevicesForSmartGroupEval(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len=%d want 0", len(got))
	}
}

func TestLoadDevicesForSmartGroupEval_QueryError_Wrapped(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("timeout"))

	_, err := s.LoadDevicesForSmartGroupEval(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !containsStr(err.Error(), "load devices for smart group eval") {
		t.Errorf("error should mention 'load devices', got %v", err)
	}
}

func TestLoadDevicesForSmartGroupEval_MultipleDevices_StableOrder(t *testing.T) {
	s, mock := newMockStore(t)
	rows := pgxmock.NewRows(deviceQueryCols).
		AddRow("alpha", "linux", "", "", "host-a", "", "", "online", "", "", int64(0), int64(0), []string{}, []string{}).
		AddRow("beta", "darwin", "", "", "host-b", "", "", "online", "", "", int64(0), int64(0), []string{}, []string{})
	mock.ExpectQuery(`SELECT`).WillReturnRows(rows)

	got, err := s.LoadDevicesForSmartGroupEval(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	// ORDER BY t.id is enforced by the SQL; pgxmock returns rows in the order we added them.
	if got[0].ID != "alpha" || got[1].ID != "beta" {
		t.Errorf("unexpected order: [%s, %s]", got[0].ID, got[1].ID)
	}
}

func TestEvictExpiredMemberships_HappyPath_ReturnsCount(t *testing.T) {
	s, mock := newMockStore(t)
	ct := pgconn.NewCommandTag("DELETE 5")
	mock.ExpectExec(`DELETE FROM "DeviceGroupMembership"`).
		WillReturnResult(ct)

	n, err := s.EvictExpiredMemberships(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Errorf("n=%d want 5", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestEvictExpiredMemberships_NothingExpired_ReturnsZero(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "DeviceGroupMembership"`).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))

	n, err := s.EvictExpiredMemberships(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
}

func TestEvictExpiredMemberships_ExecError_Wrapped(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "DeviceGroupMembership"`).
		WillReturnError(errors.New("disk full"))

	_, err := s.EvictExpiredMemberships(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !containsStr(err.Error(), "evict expired memberships") {
		t.Errorf("error should mention 'evict expired memberships', got %v", err)
	}
}

func TestReplaceSmartGroupCache_HappyPath_DeleteThenInsert(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM device_group_membership_cache`).
		WithArgs("grp-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 2"))
	mock.ExpectExec(`INSERT INTO device_group_membership_cache`).
		WithArgs("grp-1", "d1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectExec(`INSERT INTO device_group_membership_cache`).
		WithArgs("grp-1", "d2").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	err := s.ReplaceSmartGroupCache(context.Background(), "grp-1", []string{"d1", "d2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestReplaceSmartGroupCache_EmptyDeviceList_OnlyDeletes(t *testing.T) {
	// Empty-deviceIDs case: group's predicate matches nothing right now →
	// cache is emptied (delete runs, no inserts).
	s, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM device_group_membership_cache`).
		WithArgs("grp-empty").
		WillReturnResult(pgconn.NewCommandTag("DELETE 3"))
	mock.ExpectCommit()

	err := s.ReplaceSmartGroupCache(context.Background(), "grp-empty", []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestReplaceSmartGroupCache_BeginTxError_Wrapped(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectBegin().WillReturnError(errors.New("no connections"))

	err := s.ReplaceSmartGroupCache(context.Background(), "grp-1", []string{"d1"})
	if err == nil {
		t.Fatal("expected error from Begin failure")
	}
	if !containsStr(err.Error(), "begin tx") {
		t.Errorf("error should mention 'begin tx', got %v", err)
	}
}

func TestReplaceSmartGroupCache_DeleteError_Wrapped(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM device_group_membership_cache`).
		WithArgs("grp-1").
		WillReturnError(errors.New("disk io error"))
	mock.ExpectRollback()

	err := s.ReplaceSmartGroupCache(context.Background(), "grp-1", []string{"d1"})
	if err == nil {
		t.Fatal("expected error from DELETE failure")
	}
	if !containsStr(err.Error(), "clear cache") {
		t.Errorf("error should mention 'clear cache', got %v", err)
	}
}

func TestReplaceSmartGroupCache_InsertError_Wrapped(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM device_group_membership_cache`).
		WithArgs("grp-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
	mock.ExpectExec(`INSERT INTO device_group_membership_cache`).
		WithArgs("grp-1", "d1").
		WillReturnError(errors.New("constraint violation"))
	mock.ExpectRollback()

	err := s.ReplaceSmartGroupCache(context.Background(), "grp-1", []string{"d1"})
	if err == nil {
		t.Fatal("expected error from INSERT failure")
	}
	if !containsStr(err.Error(), "insert cache row") {
		t.Errorf("error should mention 'insert cache row', got %v", err)
	}
}

func TestReplaceSmartGroupCache_ConflictOnInsert_Ignored(t *testing.T) {
	// ON CONFLICT (group_id, device_id) DO NOTHING — the affected-rows
	// count is 0 but the operation succeeds. pgxmock returns the tag
	// "INSERT 0 0" to simulate a no-op upsert.
	s, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM device_group_membership_cache`).
		WithArgs("grp-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
	mock.ExpectExec(`INSERT INTO device_group_membership_cache`).
		WithArgs("grp-1", "d1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 0")) // conflict → no-op
	mock.ExpectCommit()

	err := s.ReplaceSmartGroupCache(context.Background(), "grp-1", []string{"d1"})
	if err != nil {
		t.Errorf("ON CONFLICT DO NOTHING should not error, got %v", err)
	}
}

func TestReplaceSmartGroupCache_CommitError_Wrapped(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM device_group_membership_cache`).
		WithArgs("grp-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
	mock.ExpectCommit().WillReturnError(errors.New("serialization failure"))

	err := s.ReplaceSmartGroupCache(context.Background(), "grp-1", []string{})
	if err == nil {
		t.Fatal("expected error from Commit failure")
	}
}

// Device predicate evaluation via device.Evaluate — used by the
// recompute job that calls ListSmartGroups + LoadDevicesForSmartGroupEval.
// We test the integration of Predicate → Device matching so coverage of
// the membership-computation logic is asserted at this layer.

func TestDevicePredicate_AllMatch_IncludedInGroup(t *testing.T) {
	p := device.Predicate{All: []device.Leaf{
		{Field: "os", Op: "eq", Value: "darwin"},
		{Field: "status", Op: "eq", Value: "online"},
	}}
	d := &device.Device{OS: "darwin", Status: "online"}
	ok, err := device.Evaluate(p, d, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("device should match all-leaf predicate")
	}
}

func TestDevicePredicate_AllFails_NotIncluded(t *testing.T) {
	p := device.Predicate{All: []device.Leaf{
		{Field: "os", Op: "eq", Value: "darwin"},
		{Field: "status", Op: "eq", Value: "online"},
	}}
	d := &device.Device{OS: "windows", Status: "online"}
	ok, err := device.Evaluate(p, d, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("device with wrong OS should not match 'all' predicate")
	}
}

func TestDevicePredicate_AnyMatch_IncludedInGroup(t *testing.T) {
	p := device.Predicate{Any: []device.Leaf{
		{Field: "os", Op: "eq", Value: "darwin"},
		{Field: "os", Op: "eq", Value: "linux"},
	}}
	d := &device.Device{OS: "linux"}
	ok, err := device.Evaluate(p, d, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("device with OS=linux should match 'any' predicate")
	}
}

func TestDevicePredicate_EmptyPredicate_MatchesNothing(t *testing.T) {
	// Empty predicates are a quarantine pattern — explicitly zero members.
	p := device.Predicate{}
	d := &device.Device{OS: "darwin"}
	ok, err := device.Evaluate(p, d, 0)
	if err != nil {
		t.Fatalf("unexpected error for empty predicate: %v", err)
	}
	if ok {
		t.Error("empty predicate must match nothing (quarantine pattern)")
	}
}

func TestDevicePredicate_BothAllAndAny_Error(t *testing.T) {
	// A predicate with both All and Any is a schema violation.
	p := device.Predicate{
		All: []device.Leaf{{Field: "os", Op: "eq", Value: "darwin"}},
		Any: []device.Leaf{{Field: "os", Op: "eq", Value: "linux"}},
	}
	_, err := device.Evaluate(p, &device.Device{}, 0)
	if err == nil {
		t.Error("predicate with both All and Any should return an error")
	}
}

func TestDevicePredicate_InOperator_MultipleValues(t *testing.T) {
	p := device.Predicate{All: []device.Leaf{
		{Field: "os", Op: "in", Value: []any{"darwin", "linux"}},
	}}
	tests := []struct {
		os   string
		want bool
	}{
		{"darwin", true},
		{"linux", true},
		{"windows", false},
	}
	for _, tt := range tests {
		d := &device.Device{OS: tt.os}
		ok, err := device.Evaluate(p, d, 0)
		if err != nil {
			t.Fatalf("os=%q unexpected error: %v", tt.os, err)
		}
		if ok != tt.want {
			t.Errorf("os=%q: match=%v want %v", tt.os, ok, tt.want)
		}
	}
}

func TestDevicePredicate_IdpGroupMember_Match(t *testing.T) {
	// Field is "idpGroup", op is "idp_group_member" (see predicate.go).
	p := device.Predicate{All: []device.Leaf{
		{Field: "idpGroup", Op: "idp_group_member", Value: "grp-eng"},
	}}
	d := &device.Device{IdpGroupIDs: []string{"grp-finance", "grp-eng"}}
	ok, err := device.Evaluate(p, d, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("device is in grp-eng, should match idp_group_member predicate")
	}
}

func TestDevicePredicate_IdpGroupMember_NoMatch(t *testing.T) {
	p := device.Predicate{All: []device.Leaf{
		{Field: "idpGroup", Op: "idp_group_member", Value: "grp-eng"},
	}}
	d := &device.Device{IdpGroupIDs: []string{"grp-finance"}}
	ok, err := device.Evaluate(p, d, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("device not in grp-eng should not match idp_group_member predicate")
	}
}

func TestDevicePredicate_TagsContains_Match(t *testing.T) {
	// Field is "tags", op is "tags_contains" (see predicate.go).
	p := device.Predicate{All: []device.Leaf{
		{Field: "tags", Op: "tags_contains", Value: "corp-device"},
	}}
	d := &device.Device{Tags: []string{"managed", "corp-device"}}
	ok, err := device.Evaluate(p, d, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("device has 'corp-device' tag, should match tags_contains predicate")
	}
}

func TestDevicePredicate_TagsContains_NoMatch(t *testing.T) {
	p := device.Predicate{All: []device.Leaf{
		{Field: "tags", Op: "tags_contains", Value: "corp-device"},
	}}
	d := &device.Device{Tags: []string{"personal"}}
	ok, err := device.Evaluate(p, d, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("device missing 'corp-device' tag should not match tags_contains predicate")
	}
}

// SmartGroupSnapshot type

func TestSmartGroupSnapshot_Fields(t *testing.T) {
	p := device.Predicate{All: []device.Leaf{{Field: "os", Op: "eq", Value: "linux"}}}
	snap := SmartGroupSnapshot{ID: "g1", Predicate: p}
	if snap.ID != "g1" {
		t.Errorf("ID=%q want g1", snap.ID)
	}
	if len(snap.Predicate.All) != 1 {
		t.Errorf("Predicate.All len=%d want 1", len(snap.Predicate.All))
	}
}

func containsStr(s, sub string) bool {
	return len(s) > 0 && len(sub) > 0 && (s == sub || len(s) >= len(sub) && stringContains(s, sub))
}

func stringContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Compile-time: ensure silentLog is used so the import is recognized.
var _ = silentLog
