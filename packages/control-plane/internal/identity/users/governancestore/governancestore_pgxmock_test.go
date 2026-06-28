package governancestore

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

func TestGetUserAuditEvents(t *testing.T) {
	s, m := newMock(t)
	auditCols := []string{"id", "source", "timestamp", "targetHost", "latencyMs", "entityId", "entityType", "hookDecision", "details"}
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event`).WithArgs("u1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM traffic_event`).WithArgs("u1", 10, 0).
		WillReturnRows(pgxmock.NewRows(auditCols).AddRow("a1", "agent", tNow, sp("api.x"), ip(50), sp("e1"), sp("user"), sp("pass"), json.RawMessage(`{}`)))
	evs, total, err := s.GetUserAuditEvents(context.Background(), "u1", 10, 0)
	if err != nil || total != 1 || len(evs) != 1 || evs[0].ID != "a1" {
		t.Fatalf("GetUserAuditEvents: %+v total=%d err=%v", evs, total, err)
	}
	m.ExpectQuery(`SELECT COUNT`).WithArgs("u1").WillReturnError(errors.New("boom"))
	if _, _, err := s.GetUserAuditEvents(context.Background(), "u1", 10, 0); err == nil {
		t.Fatal("count error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WithArgs("u1").WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`FROM traffic_event`).WithArgs(anyArgs(3)...).WillReturnError(errors.New("q"))
	if _, _, err := s2.GetUserAuditEvents(context.Background(), "u1", 10, 0); err == nil {
		t.Fatal("data query error should surface")
	}
	s3, m3 := newMock(t)
	m3.ExpectQuery(`SELECT COUNT`).WithArgs("u1").WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`FROM traffic_event`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(auditCols).AddRow("a1", "agent", "not-a-time", sp("x"), ip(1), sp("e"), sp("u"), sp("p"), json.RawMessage(`{}`)))
	if _, _, err := s3.GetUserAuditEvents(context.Background(), "u1", 10, 0); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestDisableVirtualKeysByOwner(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`UPDATE "VirtualKey" SET enabled = false`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("UPDATE", 3))
	if n, err := s.DisableVirtualKeysByOwner(context.Background(), "u1"); err != nil || n != 3 {
		t.Fatalf("DisableVirtualKeysByOwner: %d %v", n, err)
	}
	m.ExpectExec(`UPDATE "VirtualKey"`).WithArgs("u1").WillReturnError(errors.New("boom"))
	if _, err := s.DisableVirtualKeysByOwner(context.Background(), "u1"); err == nil {
		t.Fatal("exec error should surface")
	}
}

func TestRevokeDevicesByUser(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`UPDATE thing SET status = 'revoked'`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	if n, err := s.RevokeDevicesByUser(context.Background(), "u1"); err != nil || n != 2 {
		t.Fatalf("RevokeDevicesByUser: %d %v", n, err)
	}
	m.ExpectExec(`UPDATE thing`).WithArgs("u1").WillReturnError(errors.New("boom"))
	if _, err := s.RevokeDevicesByUser(context.Background(), "u1"); err == nil {
		t.Fatal("exec error should surface")
	}
}

func TestSuspendUser(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`UPDATE "NexusUser" SET status = 'suspended'`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := s.SuspendUser(context.Background(), "u1"); err != nil {
		t.Fatalf("SuspendUser: %v", err)
	}
	// 0 rows → "user not found" error
	m.ExpectExec(`UPDATE "NexusUser"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	if err := s.SuspendUser(context.Background(), "gone"); err == nil {
		t.Fatal("0 rows should error (user not found)")
	}
	m.ExpectExec(`UPDATE "NexusUser"`).WithArgs("u1").WillReturnError(errors.New("boom"))
	if err := s.SuspendUser(context.Background(), "u1"); err == nil {
		t.Fatal("exec error should surface")
	}
}

func TestListVirtualKeysByOwner(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "VirtualKey"\s+WHERE "ownerId" = \$1`).WithArgs("u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "enabled", "createdAt"}).AddRow("vk1", "k", true, tNow))
	if ks, err := s.ListVirtualKeysByOwner(context.Background(), "u1"); err != nil || len(ks) != 1 || ks[0].ID != "vk1" {
		t.Fatalf("ListVirtualKeysByOwner: %+v %v", ks, err)
	}
	m.ExpectQuery(`FROM "VirtualKey"`).WithArgs("u1").WillReturnError(errors.New("boom"))
	if _, err := s.ListVirtualKeysByOwner(context.Background(), "u1"); err == nil {
		t.Fatal("query error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "VirtualKey"`).WithArgs("u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "enabled", "createdAt"}).AddRow("vk1", "k", "not-a-bool", tNow))
	if _, err := s2.ListVirtualKeysByOwner(context.Background(), "u1"); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestListActiveDevicesByUser(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "hostname", "os", "status", "assignedAt"}).AddRow("d1", "host1", "darwin", "online", tNow))
	if ds, err := s.ListActiveDevicesByUser(context.Background(), "u1"); err != nil || len(ds) != 1 || ds[0].Hostname != "host1" {
		t.Fatalf("ListActiveDevicesByUser: %+v %v", ds, err)
	}
	m.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("u1").WillReturnError(errors.New("boom"))
	if _, err := s.ListActiveDevicesByUser(context.Background(), "u1"); err == nil {
		t.Fatal("query error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "hostname", "os", "status", "assignedAt"}).AddRow("d1", "h", "o", "s", "not-a-time"))
	if _, err := s2.ListActiveDevicesByUser(context.Background(), "u1"); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestGetUserAuditSummary(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM traffic_event`).WithArgs("u1").
		WillReturnRows(pgxmock.NewRows([]string{"total", "vk", "proxy", "agent", "last"}).AddRow(10, 4, 3, 3, &tNow))
	sum, err := s.GetUserAuditSummary(context.Background(), "u1")
	if err != nil || sum.TotalEvents != 10 || sum.VKEvents != 4 || sum.AgentEvents != 3 {
		t.Fatalf("GetUserAuditSummary: %+v %v", sum, err)
	}
	m.ExpectQuery(`FROM traffic_event`).WithArgs("u1").WillReturnError(errors.New("boom"))
	if _, err := s.GetUserAuditSummary(context.Background(), "u1"); err == nil {
		t.Fatal("query error should surface")
	}
}
