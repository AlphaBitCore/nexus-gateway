package vkstore

import (
	"context"
	"encoding/json"
	"errors"
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

var vkCols = []string{
	"id", "name", "keyHash", "keyPrefix", "projectId", "sourceApp", "enabled",
	"expiresAt", "rateLimitRpm", "compareEndpointRateLimitRpm",
	"allowedModels", "ownerId", "createdBy", "createdAt", "updatedAt",
	"vkType", "vkStatus", "approvedBy", "approvedAt", "rejectedBy", "rejectedAt", "rejectReason",
}

func vkRow(id, name string) []any {
	sp := func(s string) *string { return &s }
	ip := func(i int) *int { return &i }
	tp := (*time.Time)(nil)
	return []any{
		id, name, sp("hash"), sp("vk_abc"), sp("proj1"), sp("app"), true,
		tp, ip(60), ip(30),
		json.RawMessage(`["gpt-4o"]`), sp("owner1"), sp("creator"), tNow, tNow,
		sp("personal"), sp("active"), (*string)(nil), tp, (*string)(nil), tp, (*string)(nil),
	}
}

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

func TestListVirtualKeys(t *testing.T) {
	s, m := newMock(t)
	enabled := true
	p := VirtualKeyListParams{ProjectID: "proj1", Enabled: &enabled, OwnerID: "owner1", VKType: "personal", VKStatus: "active", Q: "x", Limit: 10}
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "VirtualKey" v`).
		WithArgs("proj1", true, "owner1", "personal", "active", "%x%").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`SELECT v\..* FROM "VirtualKey" v`).
		WithArgs("proj1", true, "owner1", "personal", "active", "%x%", 10, 0).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(vkRow("vk1", "k")...))
	keys, total, err := s.ListVirtualKeys(context.Background(), p)
	if err != nil || total != 1 || len(keys) != 1 || keys[0].ID != "vk1" {
		t.Fatalf("ListVirtualKeys: keys=%+v total=%d err=%v", keys, total, err)
	}
}

func TestListVirtualKeys_Errors(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, _, err := s.ListVirtualKeys(context.Background(), VirtualKeyListParams{}); err == nil {
		t.Fatal("count error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`FROM "VirtualKey" v`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("q"))
	if _, _, err := s2.ListVirtualKeys(context.Background(), VirtualKeyListParams{Limit: 5}); err == nil {
		t.Fatal("data query error should surface")
	}
	s3, m3 := newMock(t)
	bad := vkRow("vk1", "k")
	bad[6] = "not-a-bool"
	m3.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`FROM "VirtualKey" v`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(vkCols).AddRow(bad...))
	if _, _, err := s3.ListVirtualKeys(context.Background(), VirtualKeyListParams{Limit: 5}); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestGetVirtualKey(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "VirtualKey" WHERE id = \$1`).WithArgs("vk1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(vkRow("vk1", "k")...))
	v, err := s.GetVirtualKey(context.Background(), "vk1")
	if err != nil || v == nil || v.ID != "vk1" {
		t.Fatalf("GetVirtualKey: %+v %v", v, err)
	}
	m.ExpectQuery(`FROM "VirtualKey" WHERE id`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if v, err := s.GetVirtualKey(context.Background(), "missing"); err != nil || v != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", v, err)
	}
	m.ExpectQuery(`FROM "VirtualKey" WHERE id`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetVirtualKey(context.Background(), "e"); err == nil {
		t.Fatal("db error should surface")
	}
}

func TestCreateVirtualKey_Defaults(t *testing.T) {
	s, m := newMock(t)
	// SEC-W2-01 Layer A: key_version is arg 3 (KeyVersion unset → ""); vkType
	// ""→"personal" (arg 13), vkStatus ""→"active" (arg 14).
	m.ExpectQuery(`INSERT INTO "VirtualKey"`).
		WithArgs("k", "hash", "", "vk_abc", (*string)(nil), (*string)(nil), true,
			(*int)(nil), (*int)(nil), pgxmock.AnyArg(), (*string)(nil), (*time.Time)(nil), "personal", "active").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(vkRow("vk1", "k")...))
	v, err := s.CreateVirtualKey(context.Background(), CreateVirtualKeyParams{Name: "k", KeyHash: "hash", KeyPrefix: "vk_abc", Enabled: true})
	if err != nil || v == nil || v.ID != "vk1" {
		t.Fatalf("CreateVirtualKey: %+v %v", v, err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet (defaults not applied?): %v", err)
	}
	m.ExpectQuery(`INSERT INTO "VirtualKey"`).WithArgs(anyArgs(14)...).WillReturnError(errors.New("dup"))
	if _, err := s.CreateVirtualKey(context.Background(), CreateVirtualKeyParams{VKType: "application", VKStatus: "pending"}); err == nil {
		t.Fatal("insert error should surface")
	}
}

func TestUpdateVirtualKey(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`UPDATE "VirtualKey" SET`).WithArgs(anyArgs(10)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(vkRow("vk1", "k")...))
	en := false
	if _, err := s.UpdateVirtualKey(context.Background(), "vk1", UpdateVirtualKeyParams{Enabled: &en, UpdateExpiresAt: true}); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	m.ExpectQuery(`UPDATE "VirtualKey"`).WithArgs(anyArgs(10)...).WillReturnError(errors.New("boom"))
	if _, err := s.UpdateVirtualKey(context.Background(), "vk1", UpdateVirtualKeyParams{}); err == nil {
		t.Fatal("update error should surface")
	}
}

func TestRegenerateVirtualKeyHash(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`UPDATE "VirtualKey" SET "keyHash"`).WithArgs("vk1", "h2", "v1", "vk_xyz").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := s.RegenerateVirtualKeyHash(context.Background(), "vk1", "h2", "v1", "vk_xyz"); err != nil {
		t.Fatalf("RegenerateVirtualKeyHash: %v", err)
	}
	m.ExpectExec(`UPDATE "VirtualKey"`).WithArgs("vk1", "h2", "v1", "vk_xyz").WillReturnError(errors.New("boom"))
	if err := s.RegenerateVirtualKeyHash(context.Background(), "vk1", "h2", "v1", "vk_xyz"); err == nil {
		t.Fatal("exec error should surface")
	}
}

// execStatusMethod is a table helper: each approval/lifecycle method runs an
// Exec whose RowsAffected==0 maps to ErrNoRows (the row wasn't in the required
// state) and whose exec error surfaces. Asserting all three arms per method.
func TestVirtualKeyLifecycleMethods(t *testing.T) {
	cases := []struct {
		name string
		args int
		call func(s *Store) error
	}{
		{"Approve", 2, func(s *Store) error { return s.ApproveVirtualKey(context.Background(), "vk1", "admin") }},
		{"Reject", 3, func(s *Store) error { return s.RejectVirtualKey(context.Background(), "vk1", "admin", "spam") }},
		{"Renew", 2, func(s *Store) error { return s.RenewVirtualKey(context.Background(), "vk1", tNow) }},
		{"Revoke", 1, func(s *Store) error { return s.RevokeVirtualKey(context.Background(), "vk1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/ok", func(t *testing.T) {
			s, m := newMock(t)
			m.ExpectExec(`UPDATE "VirtualKey"`).WithArgs(anyArgs(tc.args)...).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
			if err := tc.call(s); err != nil {
				t.Fatalf("%s ok: %v", tc.name, err)
			}
		})
		t.Run(tc.name+"/not-in-state", func(t *testing.T) {
			s, m := newMock(t)
			m.ExpectExec(`UPDATE "VirtualKey"`).WithArgs(anyArgs(tc.args)...).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			if err := tc.call(s); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("%s 0-rows → ErrNoRows, got %v", tc.name, err)
			}
		})
		t.Run(tc.name+"/exec-error", func(t *testing.T) {
			s, m := newMock(t)
			m.ExpectExec(`UPDATE "VirtualKey"`).WithArgs(anyArgs(tc.args)...).WillReturnError(errors.New("boom"))
			if err := tc.call(s); err == nil || errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("%s exec error should surface (non-ErrNoRows), got %v", tc.name, err)
			}
		})
	}
}

func TestExpireOverdueVirtualKeys(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`SET "vkStatus" = 'expired'`).WillReturnResult(pgxmock.NewResult("UPDATE", 3))
	n, err := s.ExpireOverdueVirtualKeys(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("ExpireOverdueVirtualKeys: %d %v", n, err)
	}
	m.ExpectExec(`'expired'`).WillReturnError(errors.New("boom"))
	if _, err := s.ExpireOverdueVirtualKeys(context.Background()); err == nil {
		t.Fatal("exec error should surface")
	}
}

func TestListExpiringVirtualKeys(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "VirtualKey"`).WithArgs(7).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "expiresAt"}).AddRow("vk1", "k", tNow))
	keys, err := s.ListExpiringVirtualKeys(context.Background(), 7)
	if err != nil || len(keys) != 1 || keys[0].ID != "vk1" {
		t.Fatalf("ListExpiringVirtualKeys: %+v %v", keys, err)
	}
	m.ExpectQuery(`FROM "VirtualKey"`).WithArgs(7).WillReturnError(errors.New("boom"))
	if _, err := s.ListExpiringVirtualKeys(context.Background(), 7); err == nil {
		t.Fatal("query error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "VirtualKey"`).WithArgs(7).WillReturnRows(pgxmock.NewRows([]string{"id", "name", "expiresAt"}).AddRow("vk1", "k", "not-a-time"))
	if _, err := s2.ListExpiringVirtualKeys(context.Background(), 7); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestDeleteVirtualKey(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`DELETE FROM "VirtualKey" WHERE id = \$1`).WithArgs("vk1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.DeleteVirtualKey(context.Background(), "vk1"); err != nil {
		t.Fatalf("DeleteVirtualKey: %v", err)
	}
	m.ExpectExec(`DELETE FROM "VirtualKey"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.DeleteVirtualKey(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing → ErrNoRows, got %v", err)
	}
	m.ExpectExec(`DELETE FROM "VirtualKey"`).WithArgs("vk1").WillReturnError(errors.New("fk"))
	if err := s.DeleteVirtualKey(context.Background(), "vk1"); err == nil {
		t.Fatal("exec error should surface")
	}
}
