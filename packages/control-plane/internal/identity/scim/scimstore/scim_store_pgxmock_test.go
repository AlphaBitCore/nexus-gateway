package scimstore

import (
	"context"
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

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

var tokCols = []string{"id", "name", "tokenHash", "tokenPrefix", "identityProviderId", "createdBy", "createdAt", "lastUsedAt", "revokedAt"}

func tokRow(id string) []any {
	return []any{id, "tok", "hash", "nxs_scim_abc", sp("idp1"), "admin", tNow, (*time.Time)(nil), (*time.Time)(nil)}
}

func TestScimToken_GenerateAndHash(t *testing.T) {
	tok, prefix, err := GenerateScimToken()
	if err != nil || len(tok) < 20 || prefix != tok[:20] {
		t.Fatalf("GenerateScimToken: tok=%q prefix=%q err=%v", tok, prefix, err)
	}
	hx1, hx2 := HashScimToken("x"), HashScimToken("x")
	if hx1 != hx2 || HashScimToken("x") == HashScimToken("y") {
		t.Fatal("HashScimToken must be deterministic + collision-distinct")
	}
}

func TestCreateListGetScimToken(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`INSERT INTO "ScimToken"`).WithArgs("tok", "hash", "nxs_scim_abc", sp("idp1"), "admin").
		WillReturnRows(pgxmock.NewRows(tokCols).AddRow(tokRow("t1")...))
	if tk, err := s.CreateScimToken(context.Background(), CreateScimTokenParams{Name: "tok", TokenHash: "hash", TokenPrefix: "nxs_scim_abc", IdentityProviderID: sp("idp1"), CreatedBy: "admin"}); err != nil || tk == nil || tk.ID != "t1" {
		t.Fatalf("CreateScimToken: %+v %v", tk, err)
	}
	m.ExpectQuery(`INSERT INTO "ScimToken"`).WithArgs(anyArgs(5)...).WillReturnError(errors.New("dup"))
	if _, err := s.CreateScimToken(context.Background(), CreateScimTokenParams{}); err == nil {
		t.Fatal("create error should surface")
	}
	// List with idp filter
	m.ExpectQuery(`FROM "ScimToken" WHERE "revokedAt" IS NULL AND "identityProviderId" = \$1`).WithArgs("idp1").
		WillReturnRows(pgxmock.NewRows(tokCols).AddRow(tokRow("t1")...))
	if ts, err := s.ListScimTokens(context.Background(), sp("idp1")); err != nil || len(ts) != 1 {
		t.Fatalf("ListScimTokens filtered: %+v %v", ts, err)
	}
	// List no filter + query error
	m.ExpectQuery(`FROM "ScimToken"`).WillReturnError(errors.New("boom"))
	if _, err := s.ListScimTokens(context.Background(), nil); err == nil {
		t.Fatal("list query error should surface")
	}
	// List scan error
	s2, m2 := newMock(t)
	bad := tokRow("t1")
	bad[6] = "not-a-time"
	m2.ExpectQuery(`FROM "ScimToken"`).WillReturnRows(pgxmock.NewRows(tokCols).AddRow(bad...))
	if _, err := s2.ListScimTokens(context.Background(), nil); err == nil {
		t.Fatal("list scan error should surface")
	}
	// GetByHash found / not found / err
	m.ExpectQuery(`WHERE "tokenHash" = \$1 AND "revokedAt" IS NULL`).WithArgs("hash").
		WillReturnRows(pgxmock.NewRows(tokCols).AddRow(tokRow("t1")...))
	if tk, err := s.GetScimTokenByHash(context.Background(), "hash"); err != nil || tk == nil {
		t.Fatalf("GetScimTokenByHash: %+v %v", tk, err)
	}
	m.ExpectQuery(`"tokenHash"`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if tk, err := s.GetScimTokenByHash(context.Background(), "missing"); err != nil || tk != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", tk, err)
	}
	m.ExpectQuery(`"tokenHash"`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetScimTokenByHash(context.Background(), "e"); err == nil {
		t.Fatal("db error should surface")
	}
}

func TestTouchAndRevokeScimToken(t *testing.T) {
	s, m := newMock(t)
	// TouchScimToken ignores errors — just ensure it issues the UPDATE.
	m.ExpectExec(`UPDATE "ScimToken" SET "lastUsedAt"`).WithArgs("t1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	s.TouchScimToken(context.Background(), "t1")
	// even on error it must not panic
	m.ExpectExec(`UPDATE "ScimToken" SET "lastUsedAt"`).WithArgs("t2").WillReturnError(errors.New("boom"))
	s.TouchScimToken(context.Background(), "t2")

	m.ExpectExec(`SET "revokedAt" = NOW\(\) WHERE id = \$1 AND "revokedAt" IS NULL`).WithArgs("t1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := s.RevokeScimToken(context.Background(), "t1"); err != nil {
		t.Fatalf("RevokeScimToken: %v", err)
	}
	m.ExpectExec(`"revokedAt" = NOW\(\)`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	if err := s.RevokeScimToken(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("already-revoked/missing → ErrNoRows, got %v", err)
	}
	m.ExpectExec(`"revokedAt" = NOW\(\)`).WithArgs("t1").WillReturnError(errors.New("boom"))
	if err := s.RevokeScimToken(context.Background(), "t1"); err == nil {
		t.Fatal("exec error should surface")
	}
}

var idpCols = []string{"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled", "createdAt", "updatedAt"}

func idpRow(id string) []any {
	return []any{id, "oidc", "Okta", true, []byte(`{}`), []byte(`[]`), "developer", true, tNow, tNow}
}

func TestIdentityProviderCRUD(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "IdentityProvider"\s+ORDER BY`).
		WillReturnRows(pgxmock.NewRows(idpCols).AddRow(idpRow("idp1")...))
	if rs, err := s.ListIdentityProviders(context.Background()); err != nil || len(rs) != 1 || rs[0].Type != "oidc" {
		t.Fatalf("ListIdentityProviders: %+v %v", rs, err)
	}
	// empty → [] (non-nil)
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "IdentityProvider"`).WillReturnRows(pgxmock.NewRows(idpCols))
	if rs, err := s2.ListIdentityProviders(context.Background()); err != nil || rs == nil || len(rs) != 0 {
		t.Fatalf("empty → non-nil []: %+v %v", rs, err)
	}
	m2.ExpectQuery(`FROM "IdentityProvider"`).WillReturnError(errors.New("boom"))
	if _, err := s2.ListIdentityProviders(context.Background()); err == nil {
		t.Fatal("list query error should surface")
	}
	// scan error
	s3, m3 := newMock(t)
	bad := idpRow("idp1")
	bad[3] = "not-a-bool"
	m3.ExpectQuery(`FROM "IdentityProvider"`).WillReturnRows(pgxmock.NewRows(idpCols).AddRow(bad...))
	if _, err := s3.ListIdentityProviders(context.Background()); err == nil {
		t.Fatal("list scan error should surface")
	}
	// Get found / error
	m.ExpectQuery(`FROM "IdentityProvider"\s+WHERE id = \$1`).WithArgs("idp1").
		WillReturnRows(pgxmock.NewRows(idpCols).AddRow(idpRow("idp1")...))
	if r, err := s.GetIdentityProvider(context.Background(), "idp1"); err != nil || r == nil || r.ID != "idp1" {
		t.Fatalf("GetIdentityProvider: %+v %v", r, err)
	}
	m.ExpectQuery(`WHERE id = \$1`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetIdentityProvider(context.Background(), "e"); err == nil {
		t.Fatal("get error should surface")
	}
	// Create with defaults (Config nil→{}, RoleMapping nil→[], DefaultRole ""→developer)
	m.ExpectQuery(`INSERT INTO "IdentityProvider"`).
		WithArgs("oidc", "Okta", true, []byte(`{}`), []byte(`[]`), "developer", false).
		WillReturnRows(pgxmock.NewRows(idpCols).AddRow(idpRow("idp1")...))
	if r, err := s.CreateIdentityProvider(context.Background(), CreateIdentityProviderParams{Type: "oidc", Name: "Okta", Enabled: true}); err != nil || r == nil {
		t.Fatalf("CreateIdentityProvider: %+v %v", r, err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("create defaults not applied: %v", err)
	}
	m.ExpectQuery(`INSERT INTO "IdentityProvider"`).WithArgs(anyArgs(7)...).WillReturnError(errors.New("dup"))
	if _, err := s.CreateIdentityProvider(context.Background(), CreateIdentityProviderParams{}); err == nil {
		t.Fatal("create error should surface")
	}
	// Update (defaults too)
	m.ExpectQuery(`UPDATE "IdentityProvider"`).WithArgs(anyArgs(8)...).
		WillReturnRows(pgxmock.NewRows(idpCols).AddRow(idpRow("idp1")...))
	if _, err := s.UpdateIdentityProvider(context.Background(), UpdateIdentityProviderParams{ID: "idp1", Type: "oidc", Name: "Okta"}); err != nil {
		t.Fatalf("UpdateIdentityProvider: %v", err)
	}
	m.ExpectQuery(`UPDATE "IdentityProvider"`).WithArgs(anyArgs(8)...).WillReturnError(errors.New("boom"))
	if _, err := s.UpdateIdentityProvider(context.Background(), UpdateIdentityProviderParams{ID: "x"}); err == nil {
		t.Fatal("update error should surface")
	}
}

func TestCountAndDeleteIdentityProvider(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "UserFederatedIdentity" WHERE "idpId" = \$1`).WithArgs("idp1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(3))
	if n, err := s.CountFederatedIdentitiesForIdP(context.Background(), "idp1"); err != nil || n != 3 {
		t.Fatalf("CountFederatedIdentitiesForIdP: %d %v", n, err)
	}
	m.ExpectQuery(`FROM "UserFederatedIdentity"`).WithArgs("idp1").WillReturnError(errors.New("boom"))
	if _, err := s.CountFederatedIdentitiesForIdP(context.Background(), "idp1"); err == nil {
		t.Fatal("count error should surface")
	}
	// Delete force=true (tx: delete federated + revoke tokens + delete idp + commit)
	s2, m2 := newMock(t)
	m2.ExpectBegin()
	m2.ExpectExec(`DELETE FROM "UserFederatedIdentity" WHERE "idpId"`).WithArgs("idp1").WillReturnResult(pgxmock.NewResult("DELETE", 2))
	m2.ExpectExec(`UPDATE "ScimToken" SET "revokedAt"`).WithArgs("idp1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	m2.ExpectExec(`DELETE FROM "IdentityProvider" WHERE id`).WithArgs("idp1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	m2.ExpectCommit()
	if err := s2.DeleteIdentityProvider(context.Background(), "idp1", true); err != nil {
		t.Fatalf("DeleteIdentityProvider force: %v", err)
	}
	// Delete force=false, not found → ErrNoRows
	s3, m3 := newMock(t)
	m3.ExpectBegin()
	m3.ExpectExec(`DELETE FROM "IdentityProvider"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	m3.ExpectRollback()
	if err := s3.DeleteIdentityProvider(context.Background(), "gone", false); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing → ErrNoRows, got %v", err)
	}
	// begin error
	s4, m4 := newMock(t)
	m4.ExpectBegin().WillReturnError(errors.New("no tx"))
	if err := s4.DeleteIdentityProvider(context.Background(), "idp1", false); err == nil {
		t.Fatal("begin error should surface")
	}
	// force: federated delete error
	s5, m5 := newMock(t)
	m5.ExpectBegin()
	m5.ExpectExec(`DELETE FROM "UserFederatedIdentity"`).WithArgs("idp1").WillReturnError(errors.New("fk"))
	m5.ExpectRollback()
	if err := s5.DeleteIdentityProvider(context.Background(), "idp1", true); err == nil {
		t.Fatal("federated delete error should surface")
	}
	// idp delete exec error
	s6, m6 := newMock(t)
	m6.ExpectBegin()
	m6.ExpectExec(`DELETE FROM "IdentityProvider"`).WithArgs("idp1").WillReturnError(errors.New("boom"))
	m6.ExpectRollback()
	if err := s6.DeleteIdentityProvider(context.Background(), "idp1", false); err == nil {
		t.Fatal("idp delete error should surface")
	}
}

var mapCols = []string{"id", "identityProviderId", "externalGroupId", "externalGroupName", "iamGroupId", "createdAt"}
var mapJoinCols = append(append([]string{}, mapCols...), "iamGroupName")

func mapRow(id string) []any {
	return []any{id, "idp1", "ext-g", sp("Engineers"), "iam-g", tNow}
}

func TestIdpGroupMapping(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`INSERT INTO "IdpGroupMapping"`).WithArgs("idp1", "ext-g", sp("Engineers"), "iam-g").
		WillReturnRows(pgxmock.NewRows(mapCols).AddRow(mapRow("m1")...))
	if mm, err := s.CreateIdpGroupMapping(context.Background(), CreateIdpGroupMappingParams{IdentityProviderID: "idp1", ExternalGroupID: "ext-g", ExternalGroupName: sp("Engineers"), IamGroupID: "iam-g"}); err != nil || mm == nil || mm.ID != "m1" {
		t.Fatalf("CreateIdpGroupMapping: %+v %v", mm, err)
	}
	m.ExpectQuery(`INSERT INTO "IdpGroupMapping"`).WithArgs(anyArgs(4)...).WillReturnError(errors.New("boom"))
	if _, err := s.CreateIdpGroupMapping(context.Background(), CreateIdpGroupMappingParams{}); err == nil {
		t.Fatal("create error should surface")
	}
	// List (JOIN, 7 cols)
	m.ExpectQuery(`FROM "IdpGroupMapping" m\s+LEFT JOIN "IamGroup"`).WithArgs("idp1").
		WillReturnRows(pgxmock.NewRows(mapJoinCols).AddRow(append(mapRow("m1"), sp("Engineers"))...))
	if ms, err := s.ListIdpGroupMappings(context.Background(), "idp1"); err != nil || len(ms) != 1 {
		t.Fatalf("ListIdpGroupMappings: %+v %v", ms, err)
	}
	m.ExpectQuery(`FROM "IdpGroupMapping"`).WithArgs("idp1").WillReturnError(errors.New("boom"))
	if _, err := s.ListIdpGroupMappings(context.Background(), "idp1"); err == nil {
		t.Fatal("list query error should surface")
	}
	// Delete
	m.ExpectExec(`DELETE FROM "IdpGroupMapping" WHERE id = \$1`).WithArgs("m1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.DeleteIdpGroupMapping(context.Background(), "m1"); err != nil {
		t.Fatalf("DeleteIdpGroupMapping: %v", err)
	}
	m.ExpectExec(`DELETE FROM "IdpGroupMapping"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.DeleteIdpGroupMapping(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing → ErrNoRows, got %v", err)
	}
	// Find found / not found / err
	m.ExpectQuery(`WHERE m."identityProviderId" = \$1 AND m."externalGroupId" = \$2`).WithArgs("idp1", "ext-g").
		WillReturnRows(pgxmock.NewRows(mapJoinCols).AddRow(append(mapRow("m1"), sp("Engineers"))...))
	if mm, err := s.FindIdpGroupMappingByExternal(context.Background(), "idp1", "ext-g"); err != nil || mm == nil {
		t.Fatalf("FindIdpGroupMappingByExternal: %+v %v", mm, err)
	}
	m.ExpectQuery(`externalGroupId`).WithArgs("idp1", "missing").WillReturnError(pgx.ErrNoRows)
	if mm, err := s.FindIdpGroupMappingByExternal(context.Background(), "idp1", "missing"); err != nil || mm != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", mm, err)
	}
	m.ExpectQuery(`externalGroupId`).WithArgs("idp1", "e").WillReturnError(errors.New("db"))
	if _, err := s.FindIdpGroupMappingByExternal(context.Background(), "idp1", "e"); err == nil {
		t.Fatal("find error should surface")
	}
}

func TestCreateScimIamGroupAndSource(t *testing.T) {
	s, m := newMock(t)
	grpCols := []string{"id", "name", "description", "createdBy", "createdAt", "updatedAt"}
	m.ExpectQuery(`INSERT INTO "IamGroup"`).WithArgs("Eng", sp("d"), "idp1", "admin").
		WillReturnRows(pgxmock.NewRows(grpCols).AddRow("g1", "Eng", sp("d"), sp("admin"), tNow, tNow))
	if g, err := s.CreateScimIamGroup(context.Background(), "Eng", sp("d"), "idp1", "admin"); err != nil || g == nil || g.ID != "g1" {
		t.Fatalf("CreateScimIamGroup: %+v %v", g, err)
	}
	m.ExpectQuery(`INSERT INTO "IamGroup"`).WithArgs(anyArgs(4)...).WillReturnError(errors.New("dup"))
	if _, err := s.CreateScimIamGroup(context.Background(), "Eng", nil, "idp1", "admin"); err == nil {
		t.Fatal("create error should surface")
	}
	// GetIamGroupSource found / not-found→("",nil,nil) / err
	m.ExpectQuery(`SELECT source, identity_provider_id FROM "IamGroup" WHERE id = \$1`).WithArgs("g1").
		WillReturnRows(pgxmock.NewRows([]string{"source", "idp"}).AddRow("scim", sp("idp1")))
	if src, idp, err := s.GetIamGroupSource(context.Background(), "g1"); err != nil || src != "scim" || idp == nil || *idp != "idp1" {
		t.Fatalf("GetIamGroupSource: %q %v %v", src, idp, err)
	}
	m.ExpectQuery(`FROM "IamGroup" WHERE id`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if src, idp, err := s.GetIamGroupSource(context.Background(), "missing"); err != nil || src != "" || idp != nil {
		t.Fatalf("missing → (\"\",nil,nil), got %q %v %v", src, idp, err)
	}
}

func TestLinkUserToIdP(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`INSERT INTO "UserFederatedIdentity"`).WithArgs("u1", "idp1", "ext-sub", sp("a@x.com")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := s.LinkUserToIdP(context.Background(), "u1", "idp1", "ext-sub", sp("a@x.com")); err != nil {
		t.Fatalf("LinkUserToIdP: %v", err)
	}
	m.ExpectExec(`INSERT INTO "UserFederatedIdentity"`).WithArgs(anyArgs(4)...).WillReturnError(errors.New("boom"))
	if err := s.LinkUserToIdP(context.Background(), "u1", "idp1", "ext-sub", nil); err == nil {
		t.Fatal("exec error should surface")
	}
}
