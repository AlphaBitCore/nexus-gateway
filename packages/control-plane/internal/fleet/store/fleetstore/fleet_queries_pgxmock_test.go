package fleetstore

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

var tNow = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

var fleetDeviceCols = []string{"id", "hostname", "os", "osVersion", "agentVersion", "status", "lastHeartbeat", "assignedAt", "source"}

func fleetDeviceRow(id string) []any {
	return []any{id, "host1", "darwin", "14.0", "1.2.3", "online", (*time.Time)(nil), tNow, "auto"}
}

func TestListDevicesByUserID(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("u1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("u1", 10, 0).
		WillReturnRows(pgxmock.NewRows(fleetDeviceCols).AddRow(fleetDeviceRow("d1")...))
	devs, total, err := s.ListDevicesByUserID(context.Background(), "u1", 10, 0)
	if err != nil || total != 1 || len(devs) != 1 || devs[0].ID != "d1" || devs[0].Source != "auto" {
		t.Fatalf("ListDevicesByUserID: %+v total=%d err=%v", devs, total, err)
	}
	m.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnError(errors.New("boom"))
	if _, _, err := s.ListDevicesByUserID(context.Background(), "u1", 10, 0); err == nil {
		t.Fatal("count error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`JOIN thing t`).WithArgs(anyArgs(3)...).WillReturnError(errors.New("q"))
	if _, _, err := s2.ListDevicesByUserID(context.Background(), "u1", 10, 0); err == nil {
		t.Fatal("data query error should surface")
	}
	s3, m3 := newMock(t)
	bad := fleetDeviceRow("d1")
	bad[7] = "not-a-time"
	m3.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`JOIN thing t`).WithArgs(anyArgs(3)...).WillReturnRows(pgxmock.NewRows(fleetDeviceCols).AddRow(bad...))
	if _, _, err := s3.ListDevicesByUserID(context.Background(), "u1", 10, 0); err == nil {
		t.Fatal("scan error should surface")
	}
}

var auditCols = []string{"id", "source", "timestamp", "targetHost", "latencyMs", "entityId", "entityType", "hookDecision", "details"}

func auditRow(id string) []any {
	return []any{id, "agent", tNow, sp("api.x"), ip(120), sp("subj1"), sp("user"), sp("pass"), json.RawMessage(`{}`)}
}

func TestListAuditEventsBySubjectID(t *testing.T) {
	s, m := newMock(t)
	start := tNow.Add(-time.Hour)
	end := tNow
	p := AuditEventListParams{SubjectID: "subj1", StartTime: &start, EndTime: &end, Limit: 10}
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event`).WithArgs("subj1", start, end).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM traffic_event`).WithArgs("subj1", start, end, 10, 0).
		WillReturnRows(pgxmock.NewRows(auditCols).AddRow(auditRow("a1")...))
	evs, total, err := s.ListAuditEventsBySubjectID(context.Background(), p)
	if err != nil || total != 1 || len(evs) != 1 || evs[0].ID != "a1" {
		t.Fatalf("ListAuditEventsBySubjectID: %+v total=%d err=%v", evs, total, err)
	}
	m.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnError(errors.New("boom"))
	if _, _, err := s.ListAuditEventsBySubjectID(context.Background(), AuditEventListParams{SubjectID: "x"}); err == nil {
		t.Fatal("count error should surface")
	}
	// queryAuditEventRows query error
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`FROM traffic_event`).WithArgs(anyArgs(3)...).WillReturnError(errors.New("q"))
	if _, _, err := s2.ListAuditEventsBySubjectID(context.Background(), AuditEventListParams{SubjectID: "x", Limit: 5}); err == nil {
		t.Fatal("query error should surface")
	}
	// scan error
	s3, m3 := newMock(t)
	bad := auditRow("a1")
	bad[2] = "not-a-time"
	m3.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`FROM traffic_event`).WithArgs(anyArgs(3)...).WillReturnRows(pgxmock.NewRows(auditCols).AddRow(bad...))
	if _, _, err := s3.ListAuditEventsBySubjectID(context.Background(), AuditEventListParams{SubjectID: "x", Limit: 5}); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestListAuditEventsByDeviceID(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event`).WithArgs("dev1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM traffic_event`).WithArgs("dev1", 10, 0).
		WillReturnRows(pgxmock.NewRows(auditCols).AddRow(auditRow("a1")...))
	evs, total, err := s.ListAuditEventsByDeviceID(context.Background(), AuditEventListParams{DeviceID: "dev1", Limit: 10})
	if err != nil || total != 1 || len(evs) != 1 {
		t.Fatalf("ListAuditEventsByDeviceID: %+v total=%d err=%v", evs, total, err)
	}
	m.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnError(errors.New("boom"))
	if _, _, err := s.ListAuditEventsByDeviceID(context.Background(), AuditEventListParams{DeviceID: "x"}); err == nil {
		t.Fatal("count error should surface")
	}
}

// TestListAuditEventsByDeviceID_TimeFilters covers the start/end filter branches.
func TestListAuditEventsByDeviceID_TimeFilters(t *testing.T) {
	s, m := newMock(t)
	start := tNow.Add(-time.Hour)
	end := tNow
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event`).WithArgs("dev1", start, end).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM traffic_event`).WithArgs("dev1", start, end, 10, 0).
		WillReturnRows(pgxmock.NewRows(auditCols).AddRow(auditRow("a1")...))
	if _, total, err := s.ListAuditEventsByDeviceID(context.Background(), AuditEventListParams{DeviceID: "dev1", StartTime: &start, EndTime: &end, Limit: 10}); err != nil || total != 1 {
		t.Fatalf("ByDeviceID time filters: total=%d err=%v", total, err)
	}
}

var assignCols = []string{"id", "deviceId", "userId", "assignedAt", "releasedAt", "source", "loginMethod", "ipAddress", "tokenJti", "userDisplayName", "userOsUsername", "userOsDomain"}

func assignRow(id string) []any {
	return []any{id, "dev1", "u1", tNow, (*time.Time)(nil), "oauth", sp("password"), sp("1.2.3.4"), sp("jti1"), sp("Alice"), sp("alice"), sp("CORP")}
}

func TestListDeviceAssignments(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "DeviceAssignment" da\s+LEFT JOIN "NexusUser"`).WithArgs("dev1").
		WillReturnRows(pgxmock.NewRows(assignCols).AddRow(assignRow("as1")...))
	got, err := s.ListDeviceAssignments(context.Background(), "dev1")
	if err != nil || len(got) != 1 || got[0].UserDisplayName == nil || *got[0].UserDisplayName != "Alice" {
		t.Fatalf("ListDeviceAssignments: %+v %v", got, err)
	}
	m.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("dev1").WillReturnError(errors.New("boom"))
	if _, err := s.ListDeviceAssignments(context.Background(), "dev1"); err == nil {
		t.Fatal("query error should surface")
	}
	s2, m2 := newMock(t)
	bad := assignRow("as1")
	bad[3] = "not-a-time"
	m2.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("dev1").WillReturnRows(pgxmock.NewRows(assignCols).AddRow(bad...))
	if _, err := s2.ListDeviceAssignments(context.Background(), "dev1"); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestListDeviceAssignmentsByDevice(t *testing.T) {
	s, m := newMock(t)
	// limit<=0 defaults to 25 → assert via WithArgs.
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "DeviceAssignment" WHERE "deviceId"`).WithArgs("dev1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM "DeviceAssignment" da\s+LEFT JOIN`).WithArgs("dev1", 25, 0).
		WillReturnRows(pgxmock.NewRows(assignCols).AddRow(assignRow("as1")...))
	got, total, err := s.ListDeviceAssignmentsByDevice(context.Background(), "dev1", 0, 0)
	if err != nil || total != 1 || len(got) != 1 {
		t.Fatalf("ListDeviceAssignmentsByDevice: %+v total=%d err=%v", got, total, err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet (limit default 25 not applied?): %v", err)
	}
	m.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnError(errors.New("boom"))
	if _, _, err := s.ListDeviceAssignmentsByDevice(context.Background(), "dev1", 10, 0); err == nil {
		t.Fatal("count error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`LEFT JOIN`).WithArgs(anyArgs(3)...).WillReturnError(errors.New("q"))
	if _, _, err := s2.ListDeviceAssignmentsByDevice(context.Background(), "dev1", 10, 0); err == nil {
		t.Fatal("data query error should surface")
	}
	s3, m3 := newMock(t)
	bad := assignRow("as1")
	bad[3] = "not-a-time"
	m3.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`LEFT JOIN`).WithArgs(anyArgs(3)...).WillReturnRows(pgxmock.NewRows(assignCols).AddRow(bad...))
	if _, _, err := s3.ListDeviceAssignmentsByDevice(context.Background(), "dev1", 10, 0); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestListDeviceAssignmentsByUser(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "DeviceAssignment" da\s+LEFT JOIN "NexusUser".*WHERE da."userId"`).WithArgs("u1").
		WillReturnRows(pgxmock.NewRows(assignCols).AddRow(assignRow("as1")...))
	got, err := s.ListDeviceAssignmentsByUser(context.Background(), "u1")
	if err != nil || len(got) != 1 {
		t.Fatalf("ListDeviceAssignmentsByUser: %+v %v", got, err)
	}
	m.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("u1").WillReturnError(errors.New("boom"))
	if _, err := s.ListDeviceAssignmentsByUser(context.Background(), "u1"); err == nil {
		t.Fatal("query error should surface")
	}
	s2, m2 := newMock(t)
	bad := assignRow("as1")
	bad[3] = "not-a-time"
	m2.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("u1").WillReturnRows(pgxmock.NewRows(assignCols).AddRow(bad...))
	if _, err := s2.ListDeviceAssignmentsByUser(context.Background(), "u1"); err == nil {
		t.Fatal("scan error should surface")
	}
}
