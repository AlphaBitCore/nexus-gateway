package vkstore

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
	// key_version is arg 3 (KeyVersion unset → ""); vkType
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

// An expiry edit must re-derive vkStatus, and must confine that re-derivation
// to the two clock-driven states. Without the projection 'expired' is a
// dead-end: the expiry job only moves active -> expired, so a key given a
// future date would keep reading as expired in the admin list and stay
// rejected by the gateway. The paired guard is that a date edit must never
// resurrect a revoked key or approve a pending one, so the WHERE-side state
// filter is asserted here too — dropping either half re-opens the bug.
func TestUpdateVirtualKeyReprojectsStatusFromExpiry(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`"vkStatus" = CASE\s+WHEN \$9::boolean AND "vkStatus" IN \('active', 'expired'\)\s+THEN CASE\s+WHEN \$10::timestamptz IS NOT NULL AND \$10::timestamptz <= NOW\(\) THEN 'expired'\s+ELSE 'active'`).
		WithArgs(anyArgs(10)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(vkRow("vk1", "k")...))

	future := tNow.Add(720 * time.Hour)
	if _, err := s.UpdateVirtualKey(context.Background(), "vk1", UpdateVirtualKeyParams{
		UpdateExpiresAt: true,
		ExpiresAt:       &future,
	}); err != nil {
		t.Fatalf("expiry edit must re-project vkStatus: %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("projection statement not issued: %v", err)
	}
}

// The projection is gated on UpdateExpiresAt: an edit that leaves the expiry
// alone (renaming a project, flipping enabled) must carry vkStatus through
// untouched rather than recomputing it from a column it did not write.
func TestUpdateVirtualKeyLeavesStatusWhenExpiryUntouched(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`"vkStatus" = CASE\s+WHEN \$9::boolean AND`).
		WithArgs("vk1", (*string)(nil), (*string)(nil), (*bool)(nil), (*int)(nil), (*int)(nil),
			json.RawMessage(nil), (*string)(nil), false, (*time.Time)(nil)).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(vkRow("vk1", "k")...))

	if _, err := s.UpdateVirtualKey(context.Background(), "vk1", UpdateVirtualKeyParams{}); err != nil {
		t.Fatalf("non-expiry edit: %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected UpdateExpiresAt=false to reach the projection guard: %v", err)
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

// Renew exists primarily to rescue an already-expired key, so its state
// filter must admit 'expired' and the write must return the row to active.
// Restricting it to 'active' — the original behaviour — made the endpoint
// 404 on exactly the keys it was meant to serve. 'revoked' and 'rejected'
// stay out: those are administrative decisions, not clock events.
func TestRenewVirtualKeyRescuesExpiredKey(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`SET "expiresAt" = \$2, "vkStatus" = 'active'.*WHERE id = \$1 AND "vkType" = 'application' AND "vkStatus" IN \('active', 'expired'\)`).
		WithArgs("vk1", tNow).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := s.RenewVirtualKey(context.Background(), "vk1", tNow); err != nil {
		t.Fatalf("renew must accept an expired key and restore it to active: %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("renew statement shape: %v", err)
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
