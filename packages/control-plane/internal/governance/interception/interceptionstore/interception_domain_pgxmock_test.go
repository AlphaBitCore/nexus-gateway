package interceptionstore

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

func sp(s string) *string { return &s }

var domCols = []string{
	"id", "name", "description", "host_pattern", "host_match_type", "adapter_id", "adapter_config",
	"enabled", "priority", "default_path_action", "on_adapter_error", "network_zone", "source",
	"created_at", "updated_at", "created_by",
}
var pathCols = []string{"id", "domain_id", "path_pattern", "match_type", "action", "priority", "description", "enabled", "created_at", "updated_at"}

func domRow(id, name string) []any {
	return []any{
		id, name, sp("d"), "*.openai.com", "SUFFIX", "openai", json.RawMessage(`{}`),
		true, 10, "PROCESS", "FAIL_OPEN", "PUBLIC", "admin",
		tNow, tNow, sp("admin"),
	}
}
func pathRow(id, domainID string) []any {
	return []any{id, domainID, []string{"/v1"}, "PREFIX", "PROCESS", 5, sp("p"), true, tNow, tNow}
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

func TestListEnabledInterceptionDomains(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "interception_domain"\s+WHERE enabled = true`).
		WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "OpenAI")...))
	m.ExpectQuery(`FROM interception_path\s+WHERE domain_id = ANY\(\$1\) AND enabled = true`).WithArgs([]string{"d1"}).
		WillReturnRows(pgxmock.NewRows(pathCols).AddRow(pathRow("p1", "d1")...))
	doms, err := s.ListEnabledInterceptionDomains(context.Background())
	if err != nil || len(doms) != 1 || len(doms[0].Paths) != 1 || doms[0].Paths[0].ID != "p1" {
		t.Fatalf("ListEnabledInterceptionDomains: %+v %v", doms, err)
	}
	// empty → returns early, no attachPaths.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`WHERE enabled = true`).WillReturnRows(pgxmock.NewRows(domCols))
	if doms, err := s2.ListEnabledInterceptionDomains(context.Background()); err != nil || len(doms) != 0 {
		t.Fatalf("empty: %+v %v", doms, err)
	}
	// domain query error
	s3, m3 := newMock(t)
	m3.ExpectQuery(`WHERE enabled = true`).WillReturnError(errors.New("boom"))
	if _, err := s3.ListEnabledInterceptionDomains(context.Background()); err == nil {
		t.Fatal("domain query error should surface")
	}
	// domain scan error
	s4, m4 := newMock(t)
	bad := domRow("d1", "x")
	bad[7] = "not-a-bool"
	m4.ExpectQuery(`WHERE enabled = true`).WillReturnRows(pgxmock.NewRows(domCols).AddRow(bad...))
	if _, err := s4.ListEnabledInterceptionDomains(context.Background()); err == nil {
		t.Fatal("domain scan error should surface")
	}
	// attachPaths error
	s5, m5 := newMock(t)
	m5.ExpectQuery(`WHERE enabled = true`).WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "x")...))
	m5.ExpectQuery(`FROM interception_path`).WithArgs([]string{"d1"}).WillReturnError(errors.New("paths"))
	if _, err := s5.ListEnabledInterceptionDomains(context.Background()); err == nil {
		t.Fatal("attachPaths error should surface")
	}
	// attachPaths scan error
	s6, m6 := newMock(t)
	m6.ExpectQuery(`WHERE enabled = true`).WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "x")...))
	badPath := pathRow("p1", "d1")
	badPath[7] = "not-a-bool"
	m6.ExpectQuery(`FROM interception_path`).WithArgs([]string{"d1"}).WillReturnRows(pgxmock.NewRows(pathCols).AddRow(badPath...))
	if _, err := s6.ListEnabledInterceptionDomains(context.Background()); err == nil {
		t.Fatal("attachPaths scan error should surface")
	}
}

func TestListInterceptionDomains(t *testing.T) {
	s, m := newMock(t)
	enabled := true
	p := InterceptionDomainListParams{Enabled: &enabled, Search: "openai", Limit: 10}
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "interception_domain"`).WithArgs(true, "%openai%").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM "interception_domain"`).WithArgs(true, "%openai%", 10, 0).
		WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "OpenAI")...))
	m.ExpectQuery(`FROM interception_path\s+WHERE domain_id = ANY\(\$1\)`).WithArgs([]string{"d1"}).
		WillReturnRows(pgxmock.NewRows(pathCols).AddRow(pathRow("p1", "d1")...))
	res, err := s.ListInterceptionDomains(context.Background(), p)
	if err != nil || res.Total != 1 || len(res.Domains) != 1 || len(res.Domains[0].Paths) != 1 {
		t.Fatalf("ListInterceptionDomains: %+v %v", res, err)
	}
	// count error
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, err := s2.ListInterceptionDomains(context.Background(), InterceptionDomainListParams{}); err == nil {
		t.Fatal("count error should surface")
	}
	// data query error (limit≤0 → 50 default)
	s3, m3 := newMock(t)
	m3.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`FROM "interception_domain"`).WithArgs(50, 0).WillReturnError(errors.New("q"))
	if _, err := s3.ListInterceptionDomains(context.Background(), InterceptionDomainListParams{}); err == nil {
		t.Fatal("data query error should surface")
	}
	// scan error
	s4, m4 := newMock(t)
	bad := domRow("d1", "x")
	bad[7] = "not-a-bool"
	m4.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m4.ExpectQuery(`FROM "interception_domain"`).WithArgs(50, 0).WillReturnRows(pgxmock.NewRows(domCols).AddRow(bad...))
	if _, err := s4.ListInterceptionDomains(context.Background(), InterceptionDomainListParams{}); err == nil {
		t.Fatal("scan error should surface")
	}
	// attachPaths error
	s5, m5 := newMock(t)
	m5.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m5.ExpectQuery(`FROM "interception_domain"`).WithArgs(50, 0).WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "x")...))
	m5.ExpectQuery(`FROM interception_path`).WithArgs([]string{"d1"}).WillReturnError(errors.New("paths"))
	if _, err := s5.ListInterceptionDomains(context.Background(), InterceptionDomainListParams{}); err == nil {
		t.Fatal("attachPaths error should surface")
	}
}

func TestGetInterceptionDomain(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "interception_domain" WHERE id = \$1`).WithArgs("d1").
		WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "OpenAI")...))
	m.ExpectQuery(`FROM interception_path\s+WHERE domain_id = \$1`).WithArgs("d1").
		WillReturnRows(pgxmock.NewRows(pathCols).AddRow(pathRow("p1", "d1")...))
	if d, err := s.GetInterceptionDomain(context.Background(), "d1"); err != nil || d == nil || len(d.Paths) != 1 {
		t.Fatalf("GetInterceptionDomain: %+v %v", d, err)
	}
	m.ExpectQuery(`FROM "interception_domain" WHERE id`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if d, err := s.GetInterceptionDomain(context.Background(), "missing"); err != nil || d != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", d, err)
	}
	m.ExpectQuery(`FROM "interception_domain" WHERE id`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetInterceptionDomain(context.Background(), "e"); err == nil {
		t.Fatal("db error should surface")
	}
	// paths query error after domain found
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "interception_domain" WHERE id`).WithArgs("d1").
		WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "x")...))
	m2.ExpectQuery(`FROM interception_path`).WithArgs("d1").WillReturnError(errors.New("paths"))
	if _, err := s2.GetInterceptionDomain(context.Background(), "d1"); err == nil {
		t.Fatal("paths query error should surface")
	}
	// path scan error (domain ok, bad path row)
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM "interception_domain" WHERE id`).WithArgs("d1").
		WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "x")...))
	badP := pathRow("p1", "d1")
	badP[7] = "not-a-bool"
	m3.ExpectQuery(`FROM interception_path`).WithArgs("d1").WillReturnRows(pgxmock.NewRows(pathCols).AddRow(badP...))
	if _, err := s3.GetInterceptionDomain(context.Background(), "d1"); err == nil {
		t.Fatal("path scan error should surface")
	}
}

func TestCreateInterceptionDomain(t *testing.T) {
	s, m := newMock(t)
	m.ExpectBegin()
	m.ExpectQuery(`INSERT INTO "interception_domain"`).WithArgs(anyArgs(13)...).
		WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "OpenAI")...))
	m.ExpectQuery(`INSERT INTO "interception_path"`).WithArgs(anyArgs(7)...).
		WillReturnRows(pgxmock.NewRows(pathCols).AddRow(pathRow("p1", "d1")...))
	m.ExpectCommit()
	en := true
	d, err := s.CreateInterceptionDomain(context.Background(), CreateInterceptionDomainInput{
		Name: "OpenAI", HostPattern: "*.openai.com", AdapterID: "openai", Enabled: &en,
		HostMatchType: "EXACT", DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN",
		NetworkZone: "PUBLIC", Source: "admin",
		Paths: []CreateInterceptionPathInput{{PathPattern: []string{"/v1"}, MatchType: "PREFIX", Action: "PROCESS", Enabled: &en}},
	})
	if err != nil || d == nil || len(d.Paths) != 1 {
		t.Fatalf("CreateInterceptionDomain: %+v %v", d, err)
	}
	// path action required → error (rolls back)
	s2, m2 := newMock(t)
	m2.ExpectBegin()
	m2.ExpectQuery(`INSERT INTO "interception_domain"`).WithArgs(anyArgs(13)...).
		WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "x")...))
	m2.ExpectRollback()
	if _, err := s2.CreateInterceptionDomain(context.Background(), CreateInterceptionDomainInput{
		Name: "x", Paths: []CreateInterceptionPathInput{{PathPattern: []string{"/v1"}}}, // Action empty
	}); err == nil {
		t.Fatal("path with empty action should error")
	}
	// error arms
	for _, tc := range []struct {
		name  string
		setup func(m pgxmock.PgxPoolIface)
	}{
		{"begin", func(m pgxmock.PgxPoolIface) { m.ExpectBegin().WillReturnError(errors.New("x")) }},
		{"domain insert", func(m pgxmock.PgxPoolIface) {
			m.ExpectBegin()
			m.ExpectQuery(`INSERT INTO "interception_domain"`).WithArgs(anyArgs(13)...).WillReturnError(errors.New("x"))
			m.ExpectRollback()
		}},
		{"commit", func(m pgxmock.PgxPoolIface) {
			m.ExpectBegin()
			m.ExpectQuery(`INSERT INTO "interception_domain"`).WithArgs(anyArgs(13)...).
				WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "x")...))
			m.ExpectCommit().WillReturnError(errors.New("x"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := newMock(t)
			tc.setup(m)
			if _, err := s.CreateInterceptionDomain(context.Background(), CreateInterceptionDomainInput{Name: "x"}); err == nil {
				t.Fatalf("%s error should surface", tc.name)
			}
		})
	}
}

func TestUpdateInterceptionDomain(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`UPDATE "interception_domain" SET`).WithArgs(anyArgs(13)...).
		WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "New")...))
	m.ExpectQuery(`FROM interception_path\s+WHERE domain_id = \$1`).WithArgs("d1").
		WillReturnRows(pgxmock.NewRows(pathCols))
	if d, err := s.UpdateInterceptionDomain(context.Background(), "d1", UpdateInterceptionDomainInput{Name: sp("New")}); err != nil || d == nil {
		t.Fatalf("UpdateInterceptionDomain: %+v %v", d, err)
	}
	m.ExpectQuery(`UPDATE "interception_domain"`).WithArgs(anyArgs(13)...).WillReturnError(pgx.ErrNoRows)
	if d, err := s.UpdateInterceptionDomain(context.Background(), "missing", UpdateInterceptionDomainInput{}); err != nil || d != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", d, err)
	}
	m.ExpectQuery(`UPDATE "interception_domain"`).WithArgs(anyArgs(13)...).WillReturnError(errors.New("db"))
	if _, err := s.UpdateInterceptionDomain(context.Background(), "d1", UpdateInterceptionDomainInput{}); err == nil {
		t.Fatal("db error should surface")
	}
	// paths query error after a successful update
	s2, m2 := newMock(t)
	m2.ExpectQuery(`UPDATE "interception_domain" SET`).WithArgs(anyArgs(13)...).
		WillReturnRows(pgxmock.NewRows(domCols).AddRow(domRow("d1", "New")...))
	m2.ExpectQuery(`FROM interception_path`).WithArgs("d1").WillReturnError(errors.New("paths"))
	if _, err := s2.UpdateInterceptionDomain(context.Background(), "d1", UpdateInterceptionDomainInput{Name: sp("New")}); err == nil {
		t.Fatal("paths query error after update should surface")
	}
}

func TestDeleteInterceptionDomain(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`DELETE FROM "interception_domain" WHERE id = \$1`).WithArgs("d1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.DeleteInterceptionDomain(context.Background(), "d1"); err != nil {
		t.Fatalf("DeleteInterceptionDomain: %v", err)
	}
	m.ExpectExec(`DELETE FROM "interception_domain"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.DeleteInterceptionDomain(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing → ErrNoRows, got %v", err)
	}
	m.ExpectExec(`DELETE FROM "interception_domain"`).WithArgs("d1").WillReturnError(errors.New("fk"))
	if err := s.DeleteInterceptionDomain(context.Background(), "d1"); err == nil {
		t.Fatal("exec error should surface")
	}
}

func TestInterceptionPathCRUD(t *testing.T) {
	// GetInterceptionPath
	s, m := newMock(t)
	m.ExpectQuery(`FROM "interception_path" WHERE id = \$1`).WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(pathCols).AddRow(pathRow("p1", "d1")...))
	if p, err := s.GetInterceptionPath(context.Background(), "p1"); err != nil || p == nil || p.ID != "p1" {
		t.Fatalf("GetInterceptionPath: %+v %v", p, err)
	}
	m.ExpectQuery(`FROM "interception_path" WHERE id`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if p, err := s.GetInterceptionPath(context.Background(), "missing"); err != nil || p != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", p, err)
	}
	m.ExpectQuery(`FROM "interception_path" WHERE id`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetInterceptionPath(context.Background(), "e"); err == nil {
		t.Fatal("db error should surface")
	}

	// CreateInterceptionPath (tx)
	s2, m2 := newMock(t)
	m2.ExpectBegin()
	m2.ExpectQuery(`INSERT INTO "interception_path"`).WithArgs(anyArgs(7)...).
		WillReturnRows(pgxmock.NewRows(pathCols).AddRow(pathRow("p1", "d1")...))
	m2.ExpectCommit()
	if p, err := s2.CreateInterceptionPath(context.Background(), "d1", CreateInterceptionPathInput{PathPattern: []string{"/v1"}, Action: "PROCESS"}); err != nil || p == nil {
		t.Fatalf("CreateInterceptionPath: %+v %v", p, err)
	}
	// action required
	s3, m3 := newMock(t)
	m3.ExpectBegin()
	m3.ExpectRollback()
	if _, err := s3.CreateInterceptionPath(context.Background(), "d1", CreateInterceptionPathInput{}); err == nil {
		t.Fatal("empty action should error")
	}
	// begin error
	s4, m4 := newMock(t)
	m4.ExpectBegin().WillReturnError(errors.New("x"))
	if _, err := s4.CreateInterceptionPath(context.Background(), "d1", CreateInterceptionPathInput{Action: "PROCESS"}); err == nil {
		t.Fatal("begin error should surface")
	}
	// commit error
	s5, m5 := newMock(t)
	m5.ExpectBegin()
	m5.ExpectQuery(`INSERT INTO "interception_path"`).WithArgs(anyArgs(7)...).
		WillReturnRows(pgxmock.NewRows(pathCols).AddRow(pathRow("p1", "d1")...))
	m5.ExpectCommit().WillReturnError(errors.New("x"))
	if _, err := s5.CreateInterceptionPath(context.Background(), "d1", CreateInterceptionPathInput{Action: "PROCESS"}); err == nil {
		t.Fatal("commit error should surface")
	}

	// UpdateInterceptionPath
	s6, m6 := newMock(t)
	m6.ExpectQuery(`UPDATE "interception_path" SET`).WithArgs(anyArgs(7)...).
		WillReturnRows(pgxmock.NewRows(pathCols).AddRow(pathRow("p1", "d1")...))
	if _, err := s6.UpdateInterceptionPath(context.Background(), "p1", UpdateInterceptionPathInput{Action: sp("BLOCK")}); err != nil {
		t.Fatalf("UpdateInterceptionPath: %v", err)
	}
	m6.ExpectQuery(`UPDATE "interception_path"`).WithArgs(anyArgs(7)...).WillReturnError(errors.New("db"))
	if _, err := s6.UpdateInterceptionPath(context.Background(), "p1", UpdateInterceptionPathInput{}); err == nil {
		t.Fatal("update error should surface")
	}

	// DeleteInterceptionPath
	s7, m7 := newMock(t)
	m7.ExpectExec(`DELETE FROM "interception_path" WHERE id = \$1`).WithArgs("p1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s7.DeleteInterceptionPath(context.Background(), "p1"); err != nil {
		t.Fatalf("DeleteInterceptionPath: %v", err)
	}
	m7.ExpectExec(`DELETE FROM "interception_path"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s7.DeleteInterceptionPath(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing → ErrNoRows, got %v", err)
	}
	m7.ExpectExec(`DELETE FROM "interception_path"`).WithArgs("p1").WillReturnError(errors.New("fk"))
	if err := s7.DeleteInterceptionPath(context.Background(), "p1"); err == nil {
		t.Fatal("exec error should surface")
	}
}
