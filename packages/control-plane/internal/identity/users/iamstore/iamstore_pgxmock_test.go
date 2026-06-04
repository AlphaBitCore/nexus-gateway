package iamstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

var tNow = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

func sp(s string) *string { return &s }
func bp(b bool) *bool     { return &b }

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

// badRowN builds a row with n string columns — fed where the scanned method
// expects a different number of Scan destinations, it forces a deterministic
// "wrong destination count" scan error so the error arm is exercised even when
// every destination is a permissive string/any type.
func badRowN(n int) *pgxmock.Rows {
	cols := make([]string, n)
	vals := make([]any, n)
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d", i)
		vals[i] = "x"
	}
	return pgxmock.NewRows(cols).AddRow(vals...)
}

func policyRow() *pgxmock.Rows {
	return pgxmock.NewRows([]string{"id", "name", "description", "type", "document", "enabled", "createdBy", "createdAt", "updatedAt"}).
		AddRow("p1", "pol", sp("d"), "custom", []byte(`{}`), true, sp("admin"), tNow, tNow)
}

func groupRow() *pgxmock.Rows {
	return pgxmock.NewRows([]string{"id", "name", "description", "createdBy", "createdAt", "updatedAt"}).
		AddRow("g1", "grp", sp("d"), sp("admin"), tNow, tNow)
}

// ---- IAM Policies ----

func TestListIamPolicies(t *testing.T) {
	// All filters set: q + type + enabled.
	s, m := newMock(t)
	en := true
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "IamPolicy"`).WithArgs("%foo%", "custom", true).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`SELECT .* FROM "IamPolicy"`).WithArgs("%foo%", "custom", true, 10, 0).
		WillReturnRows(policyRow())
	ps, total, err := s.ListIamPolicies(context.Background(), "foo", "custom", &en, 10, 0)
	if err != nil || total != 1 || len(ps) != 1 || ps[0].ID != "p1" {
		t.Fatalf("filtered: %+v total=%d err=%v", ps, total, err)
	}

	// No filters — exercises the false branch of every if.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	m2.ExpectQuery(`FROM "IamPolicy"`).WithArgs(5, 0).WillReturnRows(pgxmock.NewRows([]string{"id", "name", "description", "type", "document", "enabled", "createdBy", "createdAt", "updatedAt"}))
	ps2, total2, err := s2.ListIamPolicies(context.Background(), "", "", nil, 5, 0)
	if err != nil || total2 != 0 || len(ps2) != 0 {
		t.Fatalf("unfiltered: %+v total=%d err=%v", ps2, total2, err)
	}

	// Count error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, _, err := s3.ListIamPolicies(context.Background(), "", "", nil, 5, 0); err == nil {
		t.Fatal("count error must surface")
	}

	// Data query error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m4.ExpectQuery(`FROM "IamPolicy"`).WillReturnError(errors.New("q"))
	if _, _, err := s4.ListIamPolicies(context.Background(), "", "", nil, 5, 0); err == nil {
		t.Fatal("data query error must surface")
	}

	// Scan error (non-time value into createdAt time.Time).
	s5, m5 := newMock(t)
	m5.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m5.ExpectQuery(`FROM "IamPolicy"`).WithArgs(5, 0).WillReturnRows(
		pgxmock.NewRows([]string{"id", "name", "description", "type", "document", "enabled", "createdBy", "createdAt", "updatedAt"}).
			AddRow("p1", "pol", sp("d"), "custom", json.RawMessage(`{}`), true, sp("a"), "not-a-time", tNow))
	if _, _, err := s5.ListIamPolicies(context.Background(), "", "", nil, 5, 0); err == nil {
		t.Fatal("scan error must surface")
	}
}

func TestGetIamPolicy(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IamPolicy" WHERE id = \$1`).WithArgs("p1").WillReturnRows(policyRow())
	p, err := s.GetIamPolicy(context.Background(), "p1")
	if err != nil || p == nil || p.ID != "p1" {
		t.Fatalf("GetIamPolicy: %+v %v", p, err)
	}
	// ErrNoRows → (nil, nil).
	m.ExpectQuery(`FROM "IamPolicy"`).WithArgs("gone").WillReturnError(pgx.ErrNoRows)
	if p, err := s.GetIamPolicy(context.Background(), "gone"); err != nil || p != nil {
		t.Fatalf("not-found should be (nil,nil): %+v %v", p, err)
	}
	// Other error surfaces.
	m.ExpectQuery(`FROM "IamPolicy"`).WithArgs("x").WillReturnError(errors.New("boom"))
	if _, err := s.GetIamPolicy(context.Background(), "x"); err == nil {
		t.Fatal("db error must surface")
	}
}

func TestCreateIamPolicy(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`INSERT INTO "IamPolicy"`).WithArgs("pol", sp("d"), json.RawMessage(`{}`), "admin").WillReturnRows(policyRow())
	p, err := s.CreateIamPolicy(context.Background(), "pol", sp("d"), json.RawMessage(`{}`), "admin")
	if err != nil || p.ID != "p1" {
		t.Fatalf("CreateIamPolicy: %+v %v", p, err)
	}
	m.ExpectQuery(`INSERT INTO "IamPolicy"`).WithArgs("pol", pgxmock.AnyArg(), pgxmock.AnyArg(), "admin").WillReturnError(errors.New("boom"))
	if _, err := s.CreateIamPolicy(context.Background(), "pol", nil, json.RawMessage(`{}`), "admin"); err == nil {
		t.Fatal("insert error must surface")
	}
}

func TestUpdateIamPolicy(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`UPDATE "IamPolicy" SET`).WithArgs("p1", sp("n"), sp("d"), json.RawMessage(`{}`), bp(true)).WillReturnRows(policyRow())
	p, err := s.UpdateIamPolicy(context.Background(), "p1", UpdateIamPolicyParams{Name: sp("n"), Description: sp("d"), Document: json.RawMessage(`{}`), Enabled: bp(true)})
	if err != nil || p.ID != "p1" {
		t.Fatalf("UpdateIamPolicy: %+v %v", p, err)
	}
	m.ExpectQuery(`UPDATE "IamPolicy"`).WithArgs("p1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errors.New("boom"))
	if _, err := s.UpdateIamPolicy(context.Background(), "p1", UpdateIamPolicyParams{}); err == nil {
		t.Fatal("update error must surface")
	}
}

func TestDeleteIamPolicy(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`DELETE FROM "IamPolicy"`).WithArgs("p1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.DeleteIamPolicy(context.Background(), "p1"); err != nil {
		t.Fatalf("DeleteIamPolicy: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IamPolicy"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.DeleteIamPolicy(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("0 rows should be ErrNoRows: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IamPolicy"`).WithArgs("x").WillReturnError(errors.New("boom"))
	if err := s.DeleteIamPolicy(context.Background(), "x"); err == nil {
		t.Fatal("exec error must surface")
	}
}

// ---- IAM Groups ----

func TestListIamGroups(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IamGroup" ORDER BY`).WillReturnRows(groupRow())
	gs, err := s.ListIamGroups(context.Background())
	if err != nil || len(gs) != 1 || gs[0].ID != "g1" {
		t.Fatalf("ListIamGroups: %+v %v", gs, err)
	}
	m.ExpectQuery(`FROM "IamGroup"`).WillReturnError(errors.New("boom"))
	if _, err := s.ListIamGroups(context.Background()); err == nil {
		t.Fatal("query error must surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "IamGroup"`).WillReturnRows(
		pgxmock.NewRows([]string{"id", "name", "description", "createdBy", "createdAt", "updatedAt"}).
			AddRow("g1", "grp", sp("d"), sp("a"), "not-a-time", tNow))
	if _, err := s2.ListIamGroups(context.Background()); err == nil {
		t.Fatal("scan error must surface")
	}
}

func TestGetIamGroup(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IamGroup" WHERE id = \$1`).WithArgs("g1").WillReturnRows(groupRow())
	g, err := s.GetIamGroup(context.Background(), "g1")
	if err != nil || g == nil || g.ID != "g1" {
		t.Fatalf("GetIamGroup: %+v %v", g, err)
	}
	m.ExpectQuery(`FROM "IamGroup"`).WithArgs("gone").WillReturnError(pgx.ErrNoRows)
	if g, err := s.GetIamGroup(context.Background(), "gone"); err != nil || g != nil {
		t.Fatalf("not-found should be (nil,nil): %+v %v", g, err)
	}
	m.ExpectQuery(`FROM "IamGroup"`).WithArgs("x").WillReturnError(errors.New("boom"))
	if _, err := s.GetIamGroup(context.Background(), "x"); err == nil {
		t.Fatal("db error must surface")
	}
}

func TestCreateUpdateIamGroup(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`INSERT INTO "IamGroup"`).WithArgs("grp", sp("d"), "admin").WillReturnRows(groupRow())
	if g, err := s.CreateIamGroup(context.Background(), "grp", sp("d"), "admin"); err != nil || g.ID != "g1" {
		t.Fatalf("CreateIamGroup: %+v %v", g, err)
	}
	m.ExpectQuery(`INSERT INTO "IamGroup"`).WithArgs("grp", pgxmock.AnyArg(), "admin").WillReturnError(errors.New("boom"))
	if _, err := s.CreateIamGroup(context.Background(), "grp", nil, "admin"); err == nil {
		t.Fatal("create error must surface")
	}
	m.ExpectQuery(`UPDATE "IamGroup" SET`).WithArgs("g1", sp("n"), sp("d")).WillReturnRows(groupRow())
	if g, err := s.UpdateIamGroup(context.Background(), "g1", UpdateIamGroupParams{Name: sp("n"), Description: sp("d")}); err != nil || g.ID != "g1" {
		t.Fatalf("UpdateIamGroup: %+v %v", g, err)
	}
	m.ExpectQuery(`UPDATE "IamGroup"`).WillReturnError(errors.New("boom"))
	if _, err := s.UpdateIamGroup(context.Background(), "g1", UpdateIamGroupParams{}); err == nil {
		t.Fatal("update error must surface")
	}
}

func TestDeleteIamGroup(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`DELETE FROM "IamGroup"`).WithArgs("g1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.DeleteIamGroup(context.Background(), "g1"); err != nil {
		t.Fatalf("DeleteIamGroup: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IamGroup"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.DeleteIamGroup(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("0 rows should be ErrNoRows: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IamGroup"`).WithArgs("x").WillReturnError(errors.New("boom"))
	if err := s.DeleteIamGroup(context.Background(), "x"); err == nil {
		t.Fatal("exec error must surface")
	}
}

// ---- Group membership ----

func TestAddRemoveGroupMember(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`INSERT INTO "IamGroupMembership"`).WithArgs("g1", "nexus_user", "u1").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("mem1"))
	if id, err := s.AddGroupMember(context.Background(), "g1", "nexus_user", "u1"); err != nil || id != "mem1" {
		t.Fatalf("AddGroupMember: %q %v", id, err)
	}
	m.ExpectQuery(`INSERT INTO "IamGroupMembership"`).WithArgs("g1", "nexus_user", "u2").WillReturnError(errors.New("boom"))
	if _, err := s.AddGroupMember(context.Background(), "g1", "nexus_user", "u2"); err == nil {
		t.Fatal("add error must surface")
	}

	m.ExpectExec(`DELETE FROM "IamGroupMembership" WHERE id = \$1`).WithArgs("mem1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.RemoveGroupMember(context.Background(), "mem1"); err != nil {
		t.Fatalf("RemoveGroupMember: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IamGroupMembership" WHERE id`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.RemoveGroupMember(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("0 rows should be ErrNoRows: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IamGroupMembership" WHERE id`).WithArgs("x").WillReturnError(errors.New("boom"))
	if err := s.RemoveGroupMember(context.Background(), "x"); err == nil {
		t.Fatal("exec error must surface")
	}

	m.ExpectExec(`DELETE FROM "IamGroupMembership"\s+WHERE "groupId"`).WithArgs("g1", "nexus_user", "u1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.RemoveGroupMemberByPrincipal(context.Background(), "g1", "nexus_user", "u1"); err != nil {
		t.Fatalf("RemoveGroupMemberByPrincipal: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IamGroupMembership"`).WillReturnError(errors.New("boom"))
	if err := s.RemoveGroupMemberByPrincipal(context.Background(), "g1", "nexus_user", "u9"); err == nil {
		t.Fatal("exec error must surface")
	}
}

func TestListGroupMembersRaw(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IamGroupMembership" m\s+LEFT JOIN "NexusUser"`).WithArgs("g1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "principalId", "displayName"}).AddRow("mem1", "u1", "Alice"))
	out, err := s.ListGroupMembersRaw(context.Background(), "g1")
	if err != nil || len(out) != 1 || out[0]["userId"] != "u1" || out[0]["displayName"] != "Alice" {
		t.Fatalf("ListGroupMembersRaw: %+v %v", out, err)
	}
	m.ExpectQuery(`LEFT JOIN "NexusUser"`).WillReturnError(errors.New("boom"))
	if _, err := s.ListGroupMembersRaw(context.Background(), "g1"); err == nil {
		t.Fatal("query error must surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`LEFT JOIN "NexusUser"`).WithArgs("g1").WillReturnRows(badRowN(1)) // 1 col vs 3 dests
	if _, err := s2.ListGroupMembersRaw(context.Background(), "g1"); err == nil {
		t.Fatal("scan error must surface")
	}
}

func TestListGroupMembers(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IamGroupMembership"\s+WHERE "groupId"`).WithArgs("g1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "principalType", "principalId", "createdAt"}).AddRow("mem1", "nexus_user", "u1", tNow))
	ms, err := s.ListGroupMembers(context.Background(), "g1")
	if err != nil || len(ms) != 1 || ms[0].PrincipalID != "u1" {
		t.Fatalf("ListGroupMembers: %+v %v", ms, err)
	}
	// Empty → non-nil empty slice (nil-guard branch).
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("g1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "principalType", "principalId", "createdAt"}))
	if ms, err := s2.ListGroupMembers(context.Background(), "g1"); err != nil || ms == nil || len(ms) != 0 {
		t.Fatalf("empty must be non-nil: %+v %v", ms, err)
	}
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM "IamGroupMembership"`).WillReturnError(errors.New("boom"))
	if _, err := s3.ListGroupMembers(context.Background(), "g1"); err == nil {
		t.Fatal("query error must surface")
	}
	s4, m4 := newMock(t)
	m4.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("g1").WillReturnRows(badRowN(1)) // 1 col vs 4 dests
	if _, err := s4.ListGroupMembers(context.Background(), "g1"); err == nil {
		t.Fatal("scan error must surface")
	}
}

func TestListGroupMembersPaginated(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "IamGroupMembership"`).WithArgs("g1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM "IamGroupMembership" m\s+WHERE`).WithArgs("g1", 10, 0).
		WillReturnRows(pgxmock.NewRows([]string{"id", "principalType", "principalId", "createdAt"}).AddRow("mem1", "nexus_user", "u1", tNow))
	ms, total, err := s.ListGroupMembersPaginated(context.Background(), "g1", 10, 0)
	if err != nil || total != 1 || len(ms) != 1 {
		t.Fatalf("paginated: %+v total=%d err=%v", ms, total, err)
	}
	// Empty → non-nil.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WithArgs("g1").WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	m2.ExpectQuery(`FROM "IamGroupMembership" m`).WithArgs("g1", 10, 0).WillReturnRows(pgxmock.NewRows([]string{"id", "principalType", "principalId", "createdAt"}))
	if ms, _, err := s2.ListGroupMembersPaginated(context.Background(), "g1", 10, 0); err != nil || ms == nil {
		t.Fatalf("empty must be non-nil: %+v %v", ms, err)
	}
	// Count error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`SELECT COUNT`).WithArgs("g1").WillReturnError(errors.New("boom"))
	if _, _, err := s3.ListGroupMembersPaginated(context.Background(), "g1", 10, 0); err == nil {
		t.Fatal("count error must surface")
	}
	// Data query error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`SELECT COUNT`).WithArgs("g1").WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m4.ExpectQuery(`FROM "IamGroupMembership" m`).WithArgs("g1", 10, 0).WillReturnError(errors.New("q"))
	if _, _, err := s4.ListGroupMembersPaginated(context.Background(), "g1", 10, 0); err == nil {
		t.Fatal("data error must surface")
	}
	// Scan error.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`SELECT COUNT`).WithArgs("g1").WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m5.ExpectQuery(`FROM "IamGroupMembership" m`).WithArgs("g1", 10, 0).WillReturnRows(badRowN(1))
	if _, _, err := s5.ListGroupMembersPaginated(context.Background(), "g1", 10, 0); err == nil {
		t.Fatal("scan error must surface")
	}
}

func TestListGroupNamesForPrincipal(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IamGroupMembership" m\s+JOIN "IamGroup"`).WithArgs("nexus_user", "u1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("admins").AddRow("auditors"))
	names, err := s.ListGroupNamesForPrincipal(context.Background(), "nexus_user", "u1")
	if err != nil || len(names) != 2 || names[0] != "admins" {
		t.Fatalf("ListGroupNamesForPrincipal: %+v %v", names, err)
	}
	m.ExpectQuery(`JOIN "IamGroup"`).WillReturnError(errors.New("boom"))
	if _, err := s.ListGroupNamesForPrincipal(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("query error must surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`JOIN "IamGroup"`).WithArgs("nexus_user", "u1").WillReturnRows(badRowN(2)) // 2 cols vs 1 dest
	if _, err := s2.ListGroupNamesForPrincipal(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("scan error must surface")
	}
}

// ---- Group policy attachments ----

func TestListGroupPolicies(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IamGroupPolicyAttachment" gpa\s+JOIN "IamPolicy"`).WithArgs("g1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "policyId", "name", "createdAt"}).AddRow("gpa1", "p1", "pol", tNow))
	ps, err := s.ListGroupPolicies(context.Background(), "g1")
	if err != nil || len(ps) != 1 || ps[0].PolicyName != "pol" {
		t.Fatalf("ListGroupPolicies: %+v %v", ps, err)
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`JOIN "IamPolicy"`).WithArgs("g1").WillReturnRows(pgxmock.NewRows([]string{"id", "policyId", "name", "createdAt"}))
	if ps, err := s2.ListGroupPolicies(context.Background(), "g1"); err != nil || ps == nil {
		t.Fatalf("empty must be non-nil: %+v %v", ps, err)
	}
	s3, m3 := newMock(t)
	m3.ExpectQuery(`JOIN "IamPolicy"`).WillReturnError(errors.New("boom"))
	if _, err := s3.ListGroupPolicies(context.Background(), "g1"); err == nil {
		t.Fatal("query error must surface")
	}
	s4, m4 := newMock(t)
	m4.ExpectQuery(`JOIN "IamPolicy"`).WithArgs("g1").WillReturnRows(badRowN(1))
	if _, err := s4.ListGroupPolicies(context.Background(), "g1"); err == nil {
		t.Fatal("scan error must surface")
	}
}

func TestAttachDetachGroupPolicy(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`INSERT INTO "IamGroupPolicyAttachment"`).WithArgs("g1", "p1").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("gpa1"))
	if id, err := s.AttachGroupPolicy(context.Background(), "g1", "p1"); err != nil || id != "gpa1" {
		t.Fatalf("AttachGroupPolicy: %q %v", id, err)
	}
	m.ExpectQuery(`INSERT INTO "IamGroupPolicyAttachment"`).WithArgs("g1", "p2").WillReturnError(errors.New("boom"))
	if _, err := s.AttachGroupPolicy(context.Background(), "g1", "p2"); err == nil {
		t.Fatal("attach error must surface")
	}
	m.ExpectExec(`DELETE FROM "IamGroupPolicyAttachment"`).WithArgs("gpa1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.DetachGroupPolicy(context.Background(), "gpa1"); err != nil {
		t.Fatalf("DetachGroupPolicy: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IamGroupPolicyAttachment"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.DetachGroupPolicy(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("0 rows should be ErrNoRows: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IamGroupPolicyAttachment"`).WithArgs("x").WillReturnError(errors.New("boom"))
	if err := s.DetachGroupPolicy(context.Background(), "x"); err == nil {
		t.Fatal("exec error must surface")
	}
}

func TestListPrincipalPolicyAttachments(t *testing.T) {
	// Direct + group rows.
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IamPolicyAttachment" a\s+JOIN "IamPolicy"`).WithArgs("nexus_user", "u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "policyId", "name", "createdAt"}).AddRow("a1", "p1", "pol", tNow))
	m.ExpectQuery(`FROM "IamGroupMembership" m\s+JOIN "IamGroup"`).WithArgs("nexus_user", "u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "policyId", "name", "group_id", "group_name", "createdAt"}).
			AddRow("gpa1", "p2", "pol2", "g1", "grp", tNow))
	res, err := s.ListPrincipalPolicyAttachments(context.Background(), "nexus_user", "u1")
	if err != nil || len(res) != 2 || res[0].Source != "direct" || res[1].Source != "group" || *res[1].GroupName != "grp" {
		t.Fatalf("ListPrincipalPolicyAttachments: %+v %v", res, err)
	}
	// Both empty → non-nil empty.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").WillReturnRows(pgxmock.NewRows([]string{"id", "policyId", "name", "createdAt"}))
	m2.ExpectQuery(`FROM "IamGroupMembership" m`).WithArgs("nexus_user", "u1").WillReturnRows(pgxmock.NewRows([]string{"id", "policyId", "name", "group_id", "group_name", "createdAt"}))
	if res, err := s2.ListPrincipalPolicyAttachments(context.Background(), "nexus_user", "u1"); err != nil || res == nil || len(res) != 0 {
		t.Fatalf("empty must be non-nil: %+v %v", res, err)
	}
	// Direct query error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").WillReturnError(errors.New("boom"))
	if _, err := s3.ListPrincipalPolicyAttachments(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("direct query error must surface")
	}
	// Direct scan error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").WillReturnRows(badRowN(1))
	if _, err := s4.ListPrincipalPolicyAttachments(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("direct scan error must surface")
	}
	// Group query error.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").WillReturnRows(pgxmock.NewRows([]string{"id", "policyId", "name", "createdAt"}))
	m5.ExpectQuery(`FROM "IamGroupMembership" m`).WithArgs("nexus_user", "u1").WillReturnError(errors.New("g"))
	if _, err := s5.ListPrincipalPolicyAttachments(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("group query error must surface")
	}
	// Group scan error.
	s6, m6 := newMock(t)
	m6.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").WillReturnRows(pgxmock.NewRows([]string{"id", "policyId", "name", "createdAt"}))
	m6.ExpectQuery(`FROM "IamGroupMembership" m`).WithArgs("nexus_user", "u1").WillReturnRows(badRowN(1))
	if _, err := s6.ListPrincipalPolicyAttachments(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("group scan error must surface")
	}
	// Direct mid-stream iteration error.
	s7, m7 := newMock(t)
	m7.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "policyId", "name", "createdAt"}).AddRow("a1", "p1", "pol", tNow).CloseError(errors.New("conn reset")))
	if _, err := s7.ListPrincipalPolicyAttachments(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("direct iterate error must surface")
	}
	// Group mid-stream iteration error.
	s8, m8 := newMock(t)
	m8.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").WillReturnRows(pgxmock.NewRows([]string{"id", "policyId", "name", "createdAt"}))
	m8.ExpectQuery(`FROM "IamGroupMembership" m`).WithArgs("nexus_user", "u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "policyId", "name", "group_id", "group_name", "createdAt"}).AddRow("gpa1", "p2", "pol2", "g1", "grp", tNow).CloseError(errors.New("conn reset")))
	if _, err := s8.ListPrincipalPolicyAttachments(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("group iterate error must surface")
	}
}

func TestListGroupsForPolicy(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IamGroupPolicyAttachment" gpa\s+JOIN "IamGroup"`).WithArgs("p1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("g1", "grp"))
	gs, err := s.ListGroupsForPolicy(context.Background(), "p1")
	if err != nil || len(gs) != 1 || gs[0].Name != "grp" {
		t.Fatalf("ListGroupsForPolicy: %+v %v", gs, err)
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`JOIN "IamGroup"`).WithArgs("p1").WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
	if gs, err := s2.ListGroupsForPolicy(context.Background(), "p1"); err != nil || gs == nil {
		t.Fatalf("empty must be non-nil: %+v %v", gs, err)
	}
	s3, m3 := newMock(t)
	m3.ExpectQuery(`JOIN "IamGroup"`).WithArgs("p1").WillReturnError(errors.New("boom"))
	if _, err := s3.ListGroupsForPolicy(context.Background(), "p1"); err == nil {
		t.Fatal("query error must surface")
	}
	s4, m4 := newMock(t)
	m4.ExpectQuery(`JOIN "IamGroup"`).WithArgs("p1").WillReturnRows(badRowN(1))
	if _, err := s4.ListGroupsForPolicy(context.Background(), "p1"); err == nil {
		t.Fatal("scan error must surface")
	}
}

func TestListDirectPolicyAttachments(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IamPolicyAttachment" a\s+WHERE a."policyId"`).WithArgs("p1").
		WillReturnRows(pgxmock.NewRows([]string{"principalType", "principalId"}).AddRow("nexus_user", "u1"))
	rs, err := s.ListDirectPolicyAttachments(context.Background(), "p1")
	if err != nil || len(rs) != 1 || rs[0].PrincipalID != "u1" {
		t.Fatalf("ListDirectPolicyAttachments: %+v %v", rs, err)
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("p1").WillReturnRows(pgxmock.NewRows([]string{"principalType", "principalId"}))
	if rs, err := s2.ListDirectPolicyAttachments(context.Background(), "p1"); err != nil || rs == nil {
		t.Fatalf("empty must be non-nil: %+v %v", rs, err)
	}
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("p1").WillReturnError(errors.New("boom"))
	if _, err := s3.ListDirectPolicyAttachments(context.Background(), "p1"); err == nil {
		t.Fatal("query error must surface")
	}
	s4, m4 := newMock(t)
	m4.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("p1").WillReturnRows(badRowN(1))
	if _, err := s4.ListDirectPolicyAttachments(context.Background(), "p1"); err == nil {
		t.Fatal("scan error must surface")
	}
}

func TestListPolicyNamesForPrincipal(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT DISTINCT p.name FROM`).WithArgs("nexus_user", "u1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("polA").AddRow("polB"))
	names, err := s.ListPolicyNamesForPrincipal(context.Background(), "nexus_user", "u1")
	if err != nil || len(names) != 2 {
		t.Fatalf("ListPolicyNamesForPrincipal: %+v %v", names, err)
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT DISTINCT p.name`).WithArgs("nexus_user", "u1").WillReturnRows(pgxmock.NewRows([]string{"name"}))
	if names, err := s2.ListPolicyNamesForPrincipal(context.Background(), "nexus_user", "u1"); err != nil || names == nil {
		t.Fatalf("empty must be non-nil: %+v %v", names, err)
	}
	s3, m3 := newMock(t)
	m3.ExpectQuery(`SELECT DISTINCT p.name`).WithArgs("nexus_user", "u1").WillReturnError(errors.New("boom"))
	if _, err := s3.ListPolicyNamesForPrincipal(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("query error must surface")
	}
	s4, m4 := newMock(t)
	m4.ExpectQuery(`SELECT DISTINCT p.name`).WithArgs("nexus_user", "u1").WillReturnRows(badRowN(2))
	if _, err := s4.ListPolicyNamesForPrincipal(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("scan error must surface")
	}
}

func TestAttachDetachPrincipalPolicy(t *testing.T) {
	s, m := newMock(t)
	exp := tNow
	m.ExpectQuery(`INSERT INTO "IamPolicyAttachment"`).WithArgs("nexus_user", "u1", "p1", &exp).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("a1"))
	if id, err := s.AttachPrincipalPolicy(context.Background(), "nexus_user", "u1", "p1", &exp); err != nil || id != "a1" {
		t.Fatalf("AttachPrincipalPolicy: %q %v", id, err)
	}
	// nil expiry (permanent) + error.
	m.ExpectQuery(`INSERT INTO "IamPolicyAttachment"`).WithArgs("nexus_user", "u2", "p1", pgxmock.AnyArg()).WillReturnError(errors.New("boom"))
	if _, err := s.AttachPrincipalPolicy(context.Background(), "nexus_user", "u2", "p1", nil); err == nil {
		t.Fatal("attach error must surface")
	}
	m.ExpectExec(`DELETE FROM "IamPolicyAttachment"`).WithArgs("a1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.DetachPrincipalPolicy(context.Background(), "a1"); err != nil {
		t.Fatalf("DetachPrincipalPolicy: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IamPolicyAttachment"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.DetachPrincipalPolicy(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("0 rows should be ErrNoRows: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IamPolicyAttachment"`).WithArgs("x").WillReturnError(errors.New("boom"))
	if err := s.DetachPrincipalPolicy(context.Background(), "x"); err == nil {
		t.Fatal("exec error must surface")
	}
}

func TestGetByIDHelpers(t *testing.T) {
	s, m := newMock(t)
	// GetPrincipalPolicyAttachmentByID
	m.ExpectQuery(`FROM "IamPolicyAttachment"\s+WHERE id`).WithArgs("a1").
		WillReturnRows(pgxmock.NewRows([]string{"principalType", "principalId", "policyId"}).AddRow("nexus_user", "u1", "p1"))
	pt, pid, polID, err := s.GetPrincipalPolicyAttachmentByID(context.Background(), "a1")
	if err != nil || pt != "nexus_user" || pid != "u1" || polID != "p1" {
		t.Fatalf("GetPrincipalPolicyAttachmentByID: %q %q %q %v", pt, pid, polID, err)
	}
	m.ExpectQuery(`FROM "IamPolicyAttachment"`).WithArgs("gone").WillReturnError(pgx.ErrNoRows)
	if _, _, _, err := s.GetPrincipalPolicyAttachmentByID(context.Background(), "gone"); err == nil {
		t.Fatal("not-found must surface as error")
	}

	// GetGroupMembershipByID
	m.ExpectQuery(`FROM "IamGroupMembership"\s+WHERE id`).WithArgs("mem1").
		WillReturnRows(pgxmock.NewRows([]string{"groupId", "principalType", "principalId"}).AddRow("g1", "nexus_user", "u1"))
	gid, pt2, pid2, err := s.GetGroupMembershipByID(context.Background(), "mem1")
	if err != nil || gid != "g1" || pt2 != "nexus_user" || pid2 != "u1" {
		t.Fatalf("GetGroupMembershipByID: %q %q %q %v", gid, pt2, pid2, err)
	}
	m.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("gone").WillReturnError(pgx.ErrNoRows)
	if _, _, _, err := s.GetGroupMembershipByID(context.Background(), "gone"); err == nil {
		t.Fatal("not-found must surface as error")
	}

	// GetGroupPolicyAttachmentByID
	m.ExpectQuery(`FROM "IamGroupPolicyAttachment"\s+WHERE id`).WithArgs("gpa1").
		WillReturnRows(pgxmock.NewRows([]string{"groupId", "policyId"}).AddRow("g1", "p1"))
	gid2, polID2, err := s.GetGroupPolicyAttachmentByID(context.Background(), "gpa1")
	if err != nil || gid2 != "g1" || polID2 != "p1" {
		t.Fatalf("GetGroupPolicyAttachmentByID: %q %q %v", gid2, polID2, err)
	}
	m.ExpectQuery(`FROM "IamGroupPolicyAttachment"`).WithArgs("gone").WillReturnError(pgx.ErrNoRows)
	if _, _, err := s.GetGroupPolicyAttachmentByID(context.Background(), "gone"); err == nil {
		t.Fatal("not-found must surface as error")
	}
}

func TestListPolicyAttachedUserIDs(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT DISTINCT "principalId" FROM`).WithArgs("p1").
		WillReturnRows(pgxmock.NewRows([]string{"principalId"}).AddRow("u1").AddRow("u2"))
	ids, err := s.ListPolicyAttachedUserIDs(context.Background(), "p1")
	if err != nil || len(ids) != 2 {
		t.Fatalf("ListPolicyAttachedUserIDs: %+v %v", ids, err)
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT DISTINCT "principalId"`).WithArgs("p1").WillReturnRows(pgxmock.NewRows([]string{"principalId"}))
	if ids, err := s2.ListPolicyAttachedUserIDs(context.Background(), "p1"); err != nil || ids == nil {
		t.Fatalf("empty must be non-nil: %+v %v", ids, err)
	}
	s3, m3 := newMock(t)
	m3.ExpectQuery(`SELECT DISTINCT "principalId"`).WithArgs("p1").WillReturnError(errors.New("boom"))
	if _, err := s3.ListPolicyAttachedUserIDs(context.Background(), "p1"); err == nil {
		t.Fatal("query error must surface")
	}
	s4, m4 := newMock(t)
	m4.ExpectQuery(`SELECT DISTINCT "principalId"`).WithArgs("p1").WillReturnRows(badRowN(2))
	if _, err := s4.ListPolicyAttachedUserIDs(context.Background(), "p1"); err == nil {
		t.Fatal("scan error must surface")
	}
}

// ---- LoadPolicies (iam.PolicyLoader) ----

func TestLoadPolicies(t *testing.T) {
	// Direct: one valid + one malformed (skipped). Group: one dup (skipped),
	// one new valid (added), one malformed (skipped).
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IamPolicyAttachment" a\s+JOIN "IamPolicy"`).WithArgs("nexus_user", "u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "document"}).
			AddRow("p1", "pol1", []byte(`{}`)).
			AddRow("pbad", "polbad", []byte(`not json`)))
	m.ExpectQuery(`FROM "IamGroupMembership" m\s+JOIN "IamGroup"`).WithArgs("nexus_user", "u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "document", "name2"}).
			AddRow("p1", "pol1", []byte(`{}`), "grp").         // dup → skipped
			AddRow("p3", "pol3", []byte(`{}`), "grp").         // new → added (group)
			AddRow("pbad2", "polbad2", []byte(`nope`), "grp")) // malformed → skipped
	pols, err := s.LoadPolicies(context.Background(), "nexus_user", "u1")
	if err != nil {
		t.Fatalf("LoadPolicies: %v", err)
	}
	if len(pols) != 2 || pols[0].ID != "p1" || pols[0].Source != "direct" || pols[1].ID != "p3" || pols[1].Source != "group" || pols[1].GroupName != "grp" {
		t.Fatalf("LoadPolicies result: %+v", pols)
	}

	// Direct query error.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").WillReturnError(errors.New("boom"))
	if _, err := s2.LoadPolicies(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("direct query error must surface")
	}

	// Direct scan error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").WillReturnRows(badRowN(1))
	if _, err := s3.LoadPolicies(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("direct scan error must surface")
	}

	// Direct mid-stream iteration error (connection drop after rows yielded).
	s3b, m3b := newMock(t)
	m3b.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "document"}).AddRow("p1", "pol1", []byte(`{}`)).CloseError(errors.New("conn reset")))
	if _, err := s3b.LoadPolicies(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("direct iterate error must surface")
	}

	// Group query error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").WillReturnRows(pgxmock.NewRows([]string{"id", "name", "document"}))
	m4.ExpectQuery(`FROM "IamGroupMembership" m`).WithArgs("nexus_user", "u1").WillReturnError(errors.New("g"))
	if _, err := s4.LoadPolicies(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("group query error must surface")
	}

	// Group scan error.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").WillReturnRows(pgxmock.NewRows([]string{"id", "name", "document"}))
	m5.ExpectQuery(`FROM "IamGroupMembership" m`).WithArgs("nexus_user", "u1").WillReturnRows(badRowN(1))
	if _, err := s5.LoadPolicies(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("group scan error must surface")
	}

	// Group mid-stream iteration error.
	s6, m6 := newMock(t)
	m6.ExpectQuery(`FROM "IamPolicyAttachment" a`).WithArgs("nexus_user", "u1").WillReturnRows(pgxmock.NewRows([]string{"id", "name", "document"}))
	m6.ExpectQuery(`FROM "IamGroupMembership" m`).WithArgs("nexus_user", "u1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "document", "name2"}).AddRow("p3", "pol3", []byte(`{}`), "grp").CloseError(errors.New("conn reset")))
	if _, err := s6.LoadPolicies(context.Background(), "nexus_user", "u1"); err == nil {
		t.Fatal("group iterate error must surface")
	}
}

func TestEscapeILIKE(t *testing.T) {
	if got := escapeILIKE(`a%b_c\d`); got != `a\%b\_c\\d` {
		t.Fatalf("escapeILIKE: %q", got)
	}
}
