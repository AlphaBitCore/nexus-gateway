package agentstore

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

// ── ThingNode (agent device) ──────────────────────────────────────────

var tnCols = []string{
	"id", "hostname", "os", "osVersion", "agentVersion", "status", "lastHeartbeat",
	"enrolledAt", "enrolledBy", "certSerial", "certExpiresAt", "metadata", "sysinfo",
	"primaryIp", "physicalId", "boundUserId", "boundUserDisplayName", "boundUserEmail", "tags",
}
var tnListCols = append(append([]string{}, tnCols...), "eventCount")

func tnVals(id string) []any {
	return []any{
		id, "host1", "darwin", "14.0", "1.2.3", "online", (*time.Time)(nil),
		tNow, "admin", sp("serial1"), (*time.Time)(nil), json.RawMessage(`{}`), json.RawMessage(`{}`),
		"10.0.0.1", "phys1", "u1", "Alice", "alice@x.com", []string{"vip"},
	}
}

func TestListThingNodes(t *testing.T) {
	s, m := newMock(t)
	p := ThingNodeListParams{Q: "host", Status: "online", OS: "darwin", Limit: 10}
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM`).WithArgs("%host%", "online", "darwin").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`ORDER BY t.enrolled_at DESC`).WithArgs("%host%", "online", "darwin", 10, 0).
		WillReturnRows(pgxmock.NewRows(tnListCols).AddRow(append(tnVals("d1"), 5)...))
	nodes, total, err := s.ListThingNodes(context.Background(), p)
	if err != nil || total != 1 || len(nodes) != 1 || nodes[0].ID != "d1" || nodes[0].EventCount == nil || *nodes[0].EventCount != 5 {
		t.Fatalf("ListThingNodes: %+v total=%d err=%v", nodes, total, err)
	}
}

func TestListThingNodes_Errors(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, _, err := s.ListThingNodes(context.Background(), ThingNodeListParams{}); err == nil {
		t.Fatal("count error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`ORDER BY t.enrolled_at`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("q"))
	if _, _, err := s2.ListThingNodes(context.Background(), ThingNodeListParams{Limit: 5}); err == nil {
		t.Fatal("data query error should surface")
	}
	s3, m3 := newMock(t)
	bad := append(tnVals("d1"), 5)
	bad[6] = "not-a-time" // lastHeartbeat is *time.Time; a string fails the scan
	m3.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`ORDER BY t.enrolled_at`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(tnListCols).AddRow(bad...))
	if _, _, err := s3.ListThingNodes(context.Background(), ThingNodeListParams{Limit: 5}); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestGetThingNode(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`WHERE t.id = \$1`).WithArgs("d1").
		WillReturnRows(pgxmock.NewRows(tnCols).AddRow(tnVals("d1")...))
	if n, err := s.GetThingNode(context.Background(), "d1"); err != nil || n == nil || n.ID != "d1" {
		t.Fatalf("GetThingNode found: %+v %v", n, err)
	}
	m.ExpectQuery(`WHERE t.id`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if n, err := s.GetThingNode(context.Background(), "missing"); err != nil || n != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", n, err)
	}
	m.ExpectQuery(`WHERE t.id`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetThingNode(context.Background(), "e"); err == nil {
		t.Fatal("db error should surface")
	}
}

func TestUpdateThingNodeStatus(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`UPDATE thing SET status`).WithArgs("d1", "revoked").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	m.ExpectQuery(`WHERE t.id = \$1`).WithArgs("d1").WillReturnRows(pgxmock.NewRows(tnCols).AddRow(tnVals("d1")...))
	if n, err := s.UpdateThingNodeStatus(context.Background(), "d1", "revoked"); err != nil || n == nil {
		t.Fatalf("UpdateThingNodeStatus: %+v %v", n, err)
	}
	m.ExpectExec(`UPDATE thing SET status`).WithArgs("d1", "revoked").WillReturnError(errors.New("boom"))
	if _, err := s.UpdateThingNodeStatus(context.Background(), "d1", "revoked"); err == nil {
		t.Fatal("exec error should surface (and skip the re-fetch)")
	}
}

func TestGetAgentFleetHealth(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM thing\s+WHERE type = 'agent'`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows([]string{"total", "active", "stale", "critical", "revoked"}).AddRow(10, 6, 2, 1, 1))
	h, err := s.GetAgentFleetHealth(context.Background())
	if err != nil || h.Total != 10 || h.StalePct != 20 || h.CriticalPct != 10 {
		t.Fatalf("GetAgentFleetHealth: %+v %v (expected StalePct 20, CriticalPct 10)", h, err)
	}
	m.ExpectQuery(`FROM thing`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("boom"))
	if _, err := s.GetAgentFleetHealth(context.Background()); err == nil {
		t.Fatal("query error should surface")
	}
}

func TestLookupThingNodeByCertSerial(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`WHERE ta.cert_serial = \$1 OR ta.previous_cert_serial = \$1`).WithArgs("serial1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "hostname", "status", "certSerial"}).AddRow("d1", "host1", "online", "serial1"))
	if info, err := s.LookupThingNodeByCertSerial(context.Background(), "serial1"); err != nil || info == nil || info.ID != "d1" {
		t.Fatalf("LookupThingNodeByCertSerial: %+v %v", info, err)
	}
	m.ExpectQuery(`cert_serial`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if info, err := s.LookupThingNodeByCertSerial(context.Background(), "missing"); err != nil || info != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", info, err)
	}
	m.ExpectQuery(`cert_serial`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.LookupThingNodeByCertSerial(context.Background(), "e"); err == nil {
		t.Fatal("db error should surface")
	}
}

// ── DeviceGroup ──────────────────────────────────────────────────────

var dgCols = []string{"id", "name", "description", "createdBy", "createdAt", "updatedAt"}
var dgListCols = append(append([]string{}, dgCols...), "memberCount")

func dgVals(id, name string) []any { return []any{id, name, sp("d"), sp("admin"), tNow, tNow} }

func TestListDeviceGroups(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "DeviceGroup"`).WithArgs("%x%").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM "DeviceGroup" g`).WithArgs("%x%", 10, 0).
		WillReturnRows(pgxmock.NewRows(dgListCols).AddRow(append(dgVals("g1", "grp"), 4)...))
	groups, total, err := s.ListDeviceGroups(context.Background(), DeviceGroupListParams{Q: "x", Limit: 10})
	if err != nil || total != 1 || len(groups) != 1 || groups[0].MemberCount == nil || *groups[0].MemberCount != 4 {
		t.Fatalf("ListDeviceGroups: %+v total=%d err=%v", groups, total, err)
	}
	m.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, _, err := s.ListDeviceGroups(context.Background(), DeviceGroupListParams{}); err == nil {
		t.Fatal("count error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`FROM "DeviceGroup" g`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("q"))
	if _, _, err := s2.ListDeviceGroups(context.Background(), DeviceGroupListParams{Limit: 5}); err == nil {
		t.Fatal("data query error should surface")
	}
	s3, m3 := newMock(t)
	bad := append(dgVals("g1", "grp"), 4)
	bad[4] = "not-a-time"
	m3.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`FROM "DeviceGroup" g`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(dgListCols).AddRow(bad...))
	if _, _, err := s3.ListDeviceGroups(context.Background(), DeviceGroupListParams{Limit: 5}); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestGetDeviceGroup(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "DeviceGroup" WHERE id = \$1`).WithArgs("g1").
		WillReturnRows(pgxmock.NewRows(dgCols).AddRow(dgVals("g1", "grp")...))
	if g, err := s.GetDeviceGroup(context.Background(), "g1"); err != nil || g == nil || g.ID != "g1" {
		t.Fatalf("GetDeviceGroup: %+v %v", g, err)
	}
	m.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if g, err := s.GetDeviceGroup(context.Background(), "missing"); err != nil || g != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", g, err)
	}
	m.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetDeviceGroup(context.Background(), "e"); err == nil {
		t.Fatal("db error should surface")
	}
}

func TestCreateUpdateDeviceGroup(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`INSERT INTO "DeviceGroup"`).WithArgs("grp", sp("d"), "admin").
		WillReturnRows(pgxmock.NewRows(dgCols).AddRow(dgVals("g1", "grp")...))
	if g, err := s.CreateDeviceGroup(context.Background(), "grp", sp("d"), "admin"); err != nil || g == nil {
		t.Fatalf("CreateDeviceGroup: %+v %v", g, err)
	}
	m.ExpectQuery(`INSERT INTO "DeviceGroup"`).WithArgs(anyArgs(3)...).WillReturnError(errors.New("dup"))
	if _, err := s.CreateDeviceGroup(context.Background(), "grp", nil, "admin"); err == nil {
		t.Fatal("create error should surface")
	}
	m.ExpectQuery(`UPDATE "DeviceGroup" SET`).WithArgs("g1", sp("New"), (*string)(nil)).
		WillReturnRows(pgxmock.NewRows(dgCols).AddRow(dgVals("g1", "New")...))
	if _, err := s.UpdateDeviceGroup(context.Background(), "g1", UpdateDeviceGroupParams{Name: sp("New")}); err != nil {
		t.Fatalf("UpdateDeviceGroup: %v", err)
	}
}

func TestSetSmartGroupQuery(t *testing.T) {
	// Non-nil query → smart mode, no cache wipe.
	s, m := newMock(t)
	m.ExpectQuery(`UPDATE "DeviceGroup" SET\s+membership_query`).WithArgs("g1", []byte(`{"os":"darwin"}`)).
		WillReturnRows(pgxmock.NewRows(dgCols).AddRow(dgVals("g1", "grp")...))
	if g, err := s.SetSmartGroupQuery(context.Background(), "g1", []byte(`{"os":"darwin"}`)); err != nil || g == nil {
		t.Fatalf("SetSmartGroupQuery smart: %+v %v", g, err)
	}
	// nil query → static, wipes cache first.
	s2, m2 := newMock(t)
	m2.ExpectExec(`DELETE FROM device_group_membership_cache`).WithArgs("g1").WillReturnResult(pgxmock.NewResult("DELETE", 2))
	m2.ExpectQuery(`UPDATE "DeviceGroup"`).WithArgs("g1", []byte(nil)).
		WillReturnRows(pgxmock.NewRows(dgCols).AddRow(dgVals("g1", "grp")...))
	if _, err := s2.SetSmartGroupQuery(context.Background(), "g1", nil); err != nil {
		t.Fatalf("SetSmartGroupQuery static: %v", err)
	}
	// cache-wipe error aborts before the UPDATE.
	s3, m3 := newMock(t)
	m3.ExpectExec(`DELETE FROM device_group_membership_cache`).WithArgs("g1").WillReturnError(errors.New("boom"))
	if _, err := s3.SetSmartGroupQuery(context.Background(), "g1", nil); err == nil {
		t.Fatal("cache wipe error should surface")
	}
	// group not found → (nil,nil).
	s4, m4 := newMock(t)
	m4.ExpectQuery(`UPDATE "DeviceGroup"`).WithArgs("missing", []byte(`{}`)).WillReturnError(pgx.ErrNoRows)
	if g, err := s4.SetSmartGroupQuery(context.Background(), "missing", []byte(`{}`)); err != nil || g != nil {
		t.Fatalf("not-found → (nil,nil), got %+v %v", g, err)
	}
	// update db error.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`UPDATE "DeviceGroup"`).WithArgs("g1", []byte(`{}`)).WillReturnError(errors.New("db"))
	if _, err := s5.SetSmartGroupQuery(context.Background(), "g1", []byte(`{}`)); err == nil {
		t.Fatal("update error should surface")
	}
}

func TestDeleteDeviceGroup(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`DELETE FROM "DeviceGroup" WHERE id = \$1`).WithArgs("g1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.DeleteDeviceGroup(context.Background(), "g1"); err != nil {
		t.Fatalf("DeleteDeviceGroup: %v", err)
	}
	m.ExpectExec(`DELETE FROM "DeviceGroup"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.DeleteDeviceGroup(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing → ErrNoRows, got %v", err)
	}
	m.ExpectExec(`DELETE FROM "DeviceGroup"`).WithArgs("g1").WillReturnError(errors.New("fk"))
	if err := s.DeleteDeviceGroup(context.Background(), "g1"); err == nil {
		t.Fatal("exec error should surface")
	}
}

func TestAddRemoveDeviceToGroup(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`INSERT INTO "DeviceGroupMembership"`).WithArgs("g1", "d1", (*time.Time)(nil)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("mem1"))
	if id, err := s.AddDeviceToGroup(context.Background(), "g1", "d1", nil); err != nil || id != "mem1" {
		t.Fatalf("AddDeviceToGroup: %q %v", id, err)
	}
	m.ExpectQuery(`INSERT INTO "DeviceGroupMembership"`).WithArgs(anyArgs(3)...).WillReturnError(errors.New("boom"))
	if _, err := s.AddDeviceToGroup(context.Background(), "g1", "d1", nil); err == nil {
		t.Fatal("insert error should surface")
	}
	m.ExpectExec(`DELETE FROM "DeviceGroupMembership" WHERE "groupId" = \$1 AND "deviceId" = \$2`).WithArgs("g1", "d1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.RemoveDeviceFromGroup(context.Background(), "g1", "d1"); err != nil {
		t.Fatalf("RemoveDeviceFromGroup: %v", err)
	}
	m.ExpectExec(`DELETE FROM "DeviceGroupMembership"`).WithArgs("g1", "gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.RemoveDeviceFromGroup(context.Background(), "g1", "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing → ErrNoRows, got %v", err)
	}
	m.ExpectExec(`DELETE FROM "DeviceGroupMembership"`).WithArgs("g1", "d1").WillReturnError(errors.New("boom"))
	if err := s.RemoveDeviceFromGroup(context.Background(), "g1", "d1"); err == nil {
		t.Fatal("delete exec error should surface")
	}
}

func TestGroupsOfDeviceAndMembersOfGroup(t *testing.T) {
	// GroupsOfDevice — UNION of static + cache.
	s, m := newMock(t)
	m.ExpectQuery(`FROM "DeviceGroupMembership"\s+WHERE "deviceId" = \$1`).WithArgs("d1").
		WillReturnRows(pgxmock.NewRows([]string{"groupId"}).AddRow("g1").AddRow("g2"))
	if gs, err := s.GroupsOfDevice(context.Background(), "d1"); err != nil || len(gs) != 2 {
		t.Fatalf("GroupsOfDevice: %v %v", gs, err)
	}
	m.ExpectQuery(`DeviceGroupMembership`).WithArgs("d1").WillReturnError(errors.New("boom"))
	if _, err := s.GroupsOfDevice(context.Background(), "d1"); err == nil {
		t.Fatal("query error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`DeviceGroupMembership`).WithArgs("d1").WillReturnRows(pgxmock.NewRows([]string{"groupId"}).AddRow(tNow))
	if _, err := s2.GroupsOfDevice(context.Background(), "d1"); err == nil {
		t.Fatal("scan error should surface")
	}
	// MembersOfGroup — UNION of static + cache.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM "DeviceGroupMembership"\s+WHERE "groupId" = \$1`).WithArgs("g1").
		WillReturnRows(pgxmock.NewRows([]string{"deviceId"}).AddRow("d1"))
	if ds, err := s3.MembersOfGroup(context.Background(), "g1"); err != nil || len(ds) != 1 || ds[0] != "d1" {
		t.Fatalf("MembersOfGroup: %v %v", ds, err)
	}
	m3.ExpectQuery(`DeviceGroupMembership`).WithArgs("g1").WillReturnError(errors.New("boom"))
	if _, err := s3.MembersOfGroup(context.Background(), "g1"); err == nil {
		t.Fatal("query error should surface")
	}
	s4, m4 := newMock(t)
	m4.ExpectQuery(`DeviceGroupMembership`).WithArgs("g1").WillReturnRows(pgxmock.NewRows([]string{"deviceId"}).AddRow(tNow))
	if _, err := s4.MembersOfGroup(context.Background(), "g1"); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestListDeviceGroupMemberships(t *testing.T) {
	s, m := newMock(t)
	cols := []string{"id", "groupId", "deviceId", "createdAt", "expiresAt", "deviceId2", "status", "hostname", "os"}
	m.ExpectQuery(`FROM "DeviceGroupMembership" m\s+JOIN thing t`).WithArgs("g1").
		WillReturnRows(pgxmock.NewRows(cols).AddRow("mem1", "g1", "d1", tNow, (*time.Time)(nil), "d1", "online", "host1", "darwin"))
	out, err := s.ListDeviceGroupMemberships(context.Background(), "g1")
	if err != nil || len(out) != 1 || out[0].Device.Hostname != "host1" || out[0].Device.Status != "online" {
		t.Fatalf("ListDeviceGroupMemberships: %+v %v", out, err)
	}
	m.ExpectQuery(`DeviceGroupMembership m`).WithArgs("g1").WillReturnError(errors.New("boom"))
	if _, err := s.ListDeviceGroupMemberships(context.Background(), "g1"); err == nil {
		t.Fatal("query error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`DeviceGroupMembership m`).WithArgs("g1").
		WillReturnRows(pgxmock.NewRows(cols).AddRow("mem1", "g1", "d1", "not-a-time", (*time.Time)(nil), "d1", "online", "host1", "darwin"))
	if _, err := s2.ListDeviceGroupMemberships(context.Background(), "g1"); err == nil {
		t.Fatal("scan error should surface")
	}
}
