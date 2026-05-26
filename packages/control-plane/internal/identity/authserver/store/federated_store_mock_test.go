package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func newFederatedMock(t *testing.T) (pgxmock.PgxPoolIface, *store.FederatedStore) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, store.NewFederatedStoreWithPool(mock)
}

var federatedRowCols = []string{
	"id", "userId", "idpId", "externalSubject", "externalEmail",
	"rawClaims", "linkedAt", "lastLoginAt",
}

// TestFederatedStore_FindByIdPSubject_HappyPath asserts every column
// lands on the returned FederatedIdentity including the JSONB
// rawClaims decoded back to a map.
func TestFederatedStore_FindByIdPSubject_HappyPath(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()

	email := "user@nexus.ai"
	linked := time.Unix(1_700_000_000, 0).UTC()
	last := time.Unix(1_700_001_000, 0).UTC()
	mock.ExpectQuery(`FROM "UserFederatedIdentity"`).
		WithArgs("idp_1", "sub_1").
		WillReturnRows(pgxmock.NewRows(federatedRowCols).AddRow(
			"fi_1", "u_1", "idp_1", "sub_1", &email,
			[]byte(`{"groups":["admins"]}`), linked, &last,
		))

	fi, found, err := s.FindByIdPSubject(ctx, "idp_1", "sub_1")
	if err != nil || !found {
		t.Fatalf("FindByIdPSubject: found=%v err=%v", found, err)
	}
	if fi.ID != "fi_1" || fi.UserID != "u_1" || fi.ExternalSubject != "sub_1" {
		t.Fatalf("scan mismatch: %+v", fi)
	}
	if fi.ExternalEmail == nil || *fi.ExternalEmail != email {
		t.Fatalf("externalEmail not round-tripped: %v", fi.ExternalEmail)
	}
	if fi.RawClaims["groups"] == nil {
		t.Fatalf("rawClaims not decoded: %v", fi.RawClaims)
	}
	if !fi.LinkedAt.Equal(linked) || fi.LastLoginAt == nil || !fi.LastLoginAt.Equal(last) {
		t.Fatalf("timestamps not round-tripped: %+v", fi)
	}
}

// TestFederatedStore_FindByIdPSubject_EmptyRawClaims asserts a NULL
// rawClaims column (decoded as zero-length []byte by pgx) leaves
// RawClaims at its zero value (nil map) without errors.
func TestFederatedStore_FindByIdPSubject_EmptyRawClaims(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	linked := time.Now().UTC()

	mock.ExpectQuery(`FROM "UserFederatedIdentity"`).
		WithArgs("idp_2", "sub_2").
		WillReturnRows(pgxmock.NewRows(federatedRowCols).AddRow(
			"fi_2", "u_2", "idp_2", "sub_2", (*string)(nil),
			[]byte{}, linked, (*time.Time)(nil),
		))

	fi, found, err := s.FindByIdPSubject(ctx, "idp_2", "sub_2")
	if err != nil || !found {
		t.Fatalf("FindByIdPSubject: found=%v err=%v", found, err)
	}
	if fi.RawClaims != nil {
		t.Fatalf("empty rawClaims must leave map nil; got %v", fi.RawClaims)
	}
}

// TestFederatedStore_FindByIdPSubject_NotFound asserts pgx.ErrNoRows
// is surfaced as (nil,false,nil) — caller distinguishes "missing"
// from "db error".
func TestFederatedStore_FindByIdPSubject_NotFound(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`FROM "UserFederatedIdentity"`).
		WithArgs("idp_3", "sub_3").
		WillReturnError(pgx.ErrNoRows)

	fi, found, err := s.FindByIdPSubject(ctx, "idp_3", "sub_3")
	if fi != nil || found || err != nil {
		t.Fatalf("expected (nil,false,nil); got fi=%v found=%v err=%v", fi, found, err)
	}
}

// TestFederatedStore_FindByIdPSubject_GenericError asserts non-ErrNoRows
// scan failures are surfaced verbatim — JIT-provisioning logic must
// see DB outages, not silently retry.
func TestFederatedStore_FindByIdPSubject_GenericError(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	boom := errors.New("conn closed")

	mock.ExpectQuery(`FROM "UserFederatedIdentity"`).
		WithArgs("idp_4", "sub_4").
		WillReturnError(boom)

	fi, found, err := s.FindByIdPSubject(ctx, "idp_4", "sub_4")
	if fi != nil || found {
		t.Fatalf("expected nil/false on err; got fi=%v found=%v", fi, found)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected generic err passthrough; got %v", err)
	}
}

// TestFederatedStore_FindByIdPSubject_InvalidRawClaims asserts a
// malformed JSONB blob in rawClaims is returned as a json error so
// caller surfaces the data-corruption rather than silently using a
// nil claims map.
func TestFederatedStore_FindByIdPSubject_InvalidRawClaims(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	linked := time.Now().UTC()

	mock.ExpectQuery(`FROM "UserFederatedIdentity"`).
		WithArgs("idp_5", "sub_5").
		WillReturnRows(pgxmock.NewRows(federatedRowCols).AddRow(
			"fi_5", "u_5", "idp_5", "sub_5", (*string)(nil),
			[]byte(`{not-json`), linked, (*time.Time)(nil),
		))

	fi, found, err := s.FindByIdPSubject(ctx, "idp_5", "sub_5")
	if fi != nil || found {
		t.Fatalf("expected nil/false on json err; got fi=%v found=%v", fi, found)
	}
	if err == nil {
		t.Fatal("expected json decode err; got nil")
	}
}

// TestFederatedStore_UpsertLocalIdentity_Success asserts the upsert
// fires with the three positional args in the documented order.
func TestFederatedStore_UpsertLocalIdentity_Success(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()

	mock.ExpectExec(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("u_1", "idp_1", "sub_1").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := s.UpsertLocalIdentity(ctx, "u_1", "idp_1", "sub_1"); err != nil {
		t.Fatalf("UpsertLocalIdentity: %v", err)
	}
}

// TestFederatedStore_UpsertLocalIdentity_DBError asserts errors
// surface — silent failure here would leave the user unable to
// federate on next login.
func TestFederatedStore_UpsertLocalIdentity_DBError(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	boom := errors.New("fk violation")

	mock.ExpectExec(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("u_x", "idp_x", "sub_x").
		WillReturnError(boom)

	if err := s.UpsertLocalIdentity(ctx, "u_x", "idp_x", "sub_x"); !errors.Is(err, boom) {
		t.Fatalf("expected DB err; got %v", err)
	}
}

// TestFederatedStore_TouchLastLogin_Success asserts UPDATE fires with
// the id arg only.
func TestFederatedStore_TouchLastLogin_Success(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()

	mock.ExpectExec(`UPDATE "UserFederatedIdentity" SET "lastLoginAt" = NOW\(\) WHERE id = \$1`).
		WithArgs("fi_touch").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := s.TouchLastLogin(ctx, "fi_touch"); err != nil {
		t.Fatalf("TouchLastLogin: %v", err)
	}
}

// TestFederatedStore_TouchLastLogin_DBError asserts errors propagate.
func TestFederatedStore_TouchLastLogin_DBError(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	boom := errors.New("lock")

	mock.ExpectExec(`UPDATE "UserFederatedIdentity" SET "lastLoginAt"`).
		WithArgs("fi_err").
		WillReturnError(boom)

	if err := s.TouchLastLogin(ctx, "fi_err"); !errors.Is(err, boom) {
		t.Fatalf("expected DB err; got %v", err)
	}
}

// TestFederatedStore_UpdateRawClaims_Success asserts the UPDATE
// fires with (id, marshaled-bytes) in that arg order.
func TestFederatedStore_UpdateRawClaims_Success(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()

	// json.Marshal of a sorted single-key map is deterministic.
	claims := map[string]any{"k": "v"}
	wantBytes := []byte(`{"k":"v"}`)
	mock.ExpectExec(`UPDATE "UserFederatedIdentity" SET "rawClaims" = \$2, "lastLoginAt" = NOW\(\) WHERE id = \$1`).
		WithArgs("fi_1", wantBytes).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := s.UpdateRawClaims(ctx, "fi_1", claims); err != nil {
		t.Fatalf("UpdateRawClaims: %v", err)
	}
}

// TestFederatedStore_UpdateRawClaims_MarshalError asserts that an
// unmarshalable claims value (channel type) returns a marshal error
// without issuing a DB call.
func TestFederatedStore_UpdateRawClaims_MarshalError(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	// No ExpectExec — UPDATE must not fire because json.Marshal fails first.

	bad := map[string]any{"ch": make(chan int)}
	if err := s.UpdateRawClaims(ctx, "fi_1", bad); err == nil {
		t.Fatal("expected json marshal err; got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("DB must not be called on marshal err: %v", err)
	}
}

// TestFederatedStore_UpdateRawClaims_DBError asserts a DB failure
// after marshal surfaces to the caller.
func TestFederatedStore_UpdateRawClaims_DBError(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	boom := errors.New("io")

	mock.ExpectExec(`UPDATE "UserFederatedIdentity" SET "rawClaims"`).
		WithArgs("fi_err", []byte(`{}`)).
		WillReturnError(boom)

	if err := s.UpdateRawClaims(ctx, "fi_err", map[string]any{}); !errors.Is(err, boom) {
		t.Fatalf("expected DB err; got %v", err)
	}
}

// TestFederatedStore_JITProvisionUser_HappyPath asserts the two-stage
// tx (NexusUser INSERT then UserFederatedIdentity INSERT) commits and
// returns the new user + federated-identity id. DisplayName falls back
// to Email when DisplayName is empty.
func TestFederatedStore_JITProvisionUser_HappyPath(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()

	emailStr := "jit@nexus.ai"
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs(emailStr, &emailStr, "okta").
		WillReturnRows(pgxmock.NewRows([]string{"id", "displayName", "email", "status", "source"}).
			AddRow("u_jit", emailStr, &emailStr, "active", "oidc"))
	mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("u_jit", "idp_okta", "sub_external").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi_jit"))
	mock.ExpectCommit()

	u, fiID, err := s.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:           "idp_okta",
		ExternalSubject: "sub_external",
		Email:           emailStr,
		DisplayName:     "", // empty → falls back to Email
		CreatedBy:       "okta",
	})
	if err != nil {
		t.Fatalf("JITProvisionUser: %v", err)
	}
	if u.ID != "u_jit" || u.Source != "oidc" || u.Status != "active" {
		t.Fatalf("user mismatch: %+v", u)
	}
	if fiID != "fi_jit" {
		t.Fatalf("federated id mismatch: %q", fiID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestFederatedStore_JITProvisionUser_DisplayNameUsedWhenPresent asserts
// the supplied DisplayName wins over the email-fallback.
func TestFederatedStore_JITProvisionUser_DisplayNameUsedWhenPresent(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()

	emailStr := "u@nexus.ai"
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs("Alice Cooper", &emailStr, "okta").
		WillReturnRows(pgxmock.NewRows([]string{"id", "displayName", "email", "status", "source"}).
			AddRow("u_dn", "Alice Cooper", &emailStr, "active", "oidc"))
	mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("u_dn", "idp_okta", "sub").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi_dn"))
	mock.ExpectCommit()

	_, _, err := s.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:           "idp_okta",
		ExternalSubject: "sub",
		Email:           emailStr,
		DisplayName:     "Alice Cooper",
		CreatedBy:       "okta",
	})
	if err != nil {
		t.Fatalf("JITProvisionUser: %v", err)
	}
}

// TestFederatedStore_JITProvisionUser_EmptyEmail asserts an empty
// Email is bound as nil (SQL NULL) — IdPs that omit email must not
// insert empty-string rows that violate UNIQUE(email).
func TestFederatedStore_JITProvisionUser_EmptyEmail(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs("Bob", (*string)(nil), "saml").
		WillReturnRows(pgxmock.NewRows([]string{"id", "displayName", "email", "status", "source"}).
			AddRow("u_noemail", "Bob", (*string)(nil), "active", "oidc"))
	mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("u_noemail", "idp_saml", "sub").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi_noemail"))
	mock.ExpectCommit()

	u, _, err := s.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:           "idp_saml",
		ExternalSubject: "sub",
		Email:           "",
		DisplayName:     "Bob",
		CreatedBy:       "saml",
	})
	if err != nil {
		t.Fatalf("JITProvisionUser: %v", err)
	}
	if u.Email != nil {
		t.Fatalf("empty email must remain nil; got %v", u.Email)
	}
}

// TestFederatedStore_JITProvisionUser_BeginError asserts a Begin
// failure short-circuits before any INSERT and returns the wrapped
// error.
func TestFederatedStore_JITProvisionUser_BeginError(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	boom := errors.New("no conn")

	mock.ExpectBegin().WillReturnError(boom)

	u, fiID, err := s.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:           "idp",
		ExternalSubject: "sub",
		Email:           "u@nexus.ai",
		CreatedBy:       "test",
	})
	if u != nil || fiID != "" {
		t.Fatalf("expected nil/empty on Begin err; got u=%v fiID=%q", u, fiID)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected Begin err; got %v", err)
	}
}

// TestFederatedStore_JITProvisionUser_UserInsertError asserts the
// NexusUser INSERT failure is returned and Commit is NOT called
// (Rollback via defer).
func TestFederatedStore_JITProvisionUser_UserInsertError(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	boom := errors.New("unique violation")

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(boom)
	mock.ExpectRollback()

	u, fiID, err := s.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:           "idp",
		ExternalSubject: "sub",
		Email:           "u@nexus.ai",
		CreatedBy:       "test",
	})
	if u != nil || fiID != "" {
		t.Fatalf("expected nil/empty on user-insert err; got u=%v fiID=%q", u, fiID)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected user-insert err; got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestFederatedStore_JITProvisionUser_FederatedInsertError asserts
// the federated INSERT failure rolls back and surfaces the error —
// half-committed (user without federated identity) would orphan a
// shadow user.
func TestFederatedStore_JITProvisionUser_FederatedInsertError(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	boom := errors.New("unique federated")

	emailStr := "u@nexus.ai"
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs(emailStr, &emailStr, "test").
		WillReturnRows(pgxmock.NewRows([]string{"id", "displayName", "email", "status", "source"}).
			AddRow("u_x", emailStr, &emailStr, "active", "oidc"))
	mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("u_x", "idp", "sub").
		WillReturnError(boom)
	mock.ExpectRollback()

	u, fiID, err := s.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:           "idp",
		ExternalSubject: "sub",
		Email:           emailStr,
		CreatedBy:       "test",
	})
	if u != nil || fiID != "" {
		t.Fatalf("expected nil/empty on federated-insert err; got u=%v fiID=%q", u, fiID)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected federated-insert err; got %v", err)
	}
}

// TestFederatedStore_JITProvisionUser_CommitError asserts a Commit
// failure surfaces and the half-completed work is reported as an
// error.
func TestFederatedStore_JITProvisionUser_CommitError(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	boom := errors.New("commit failed")

	emailStr := "u@nexus.ai"
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs(emailStr, &emailStr, "test").
		WillReturnRows(pgxmock.NewRows([]string{"id", "displayName", "email", "status", "source"}).
			AddRow("u_c", emailStr, &emailStr, "active", "oidc"))
	mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("u_c", "idp", "sub").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi_c"))
	mock.ExpectCommit().WillReturnError(boom)

	u, fiID, err := s.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:           "idp",
		ExternalSubject: "sub",
		Email:           emailStr,
		CreatedBy:       "test",
	})
	if u != nil || fiID != "" {
		t.Fatalf("expected nil/empty on commit err; got u=%v fiID=%q", u, fiID)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected commit err; got %v", err)
	}
}

// TestFederatedStore_JITProvisionUser_GroupsMapped asserts that each
// JWT group whose externalGroupId resolves via IdpGroupMapping causes a
// matching IamGroupMembership row to be inserted in the same tx.
// principalType is the SCIM convention "nexus_user" so policy
// resolution sees OIDC-JIT users the same way SCIM-provisioned ones.
func TestFederatedStore_JITProvisionUser_GroupsMapped(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()

	emailStr := "jit@nexus.ai"
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs(emailStr, &emailStr, "okta").
		WillReturnRows(pgxmock.NewRows([]string{"id", "displayName", "email", "status", "source"}).
			AddRow("u_groups", emailStr, &emailStr, "active", "oidc"))
	mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("u_groups", "idp_okta", "sub_groups").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi_groups"))
	// Group 1 — mapped.
	mock.ExpectQuery(`FROM "IdpGroupMapping"`).
		WithArgs("idp_okta", "admins").
		WillReturnRows(pgxmock.NewRows([]string{"iamGroupId"}).AddRow("iam_super_admins"))
	mock.ExpectExec(`INSERT INTO "IamGroupMembership"`).
		WithArgs("iam_super_admins", "u_groups").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Group 2 — mapped.
	mock.ExpectQuery(`FROM "IdpGroupMapping"`).
		WithArgs("idp_okta", "viewers").
		WillReturnRows(pgxmock.NewRows([]string{"iamGroupId"}).AddRow("iam_viewers"))
	mock.ExpectExec(`INSERT INTO "IamGroupMembership"`).
		WithArgs("iam_viewers", "u_groups").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	u, fiID, err := s.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:           "idp_okta",
		ExternalSubject: "sub_groups",
		Email:           emailStr,
		Groups:          []string{"admins", "viewers"},
		CreatedBy:       "okta",
	})
	if err != nil {
		t.Fatalf("JITProvisionUser: %v", err)
	}
	if u.ID != "u_groups" || fiID != "fi_groups" {
		t.Fatalf("unexpected u/fiID: %+v %q", u, fiID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestFederatedStore_JITProvisionUser_GroupsUnmappedSkipped asserts an
// external group with no IdpGroupMapping row is silently skipped (no
// IamGroupMembership insert), matching the SCIM Groups POST policy
// where mapping miss is a no-op, not an error.
func TestFederatedStore_JITProvisionUser_GroupsUnmappedSkipped(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()

	emailStr := "jit@nexus.ai"
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs(emailStr, &emailStr, "okta").
		WillReturnRows(pgxmock.NewRows([]string{"id", "displayName", "email", "status", "source"}).
			AddRow("u_skip", emailStr, &emailStr, "active", "oidc"))
	mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("u_skip", "idp_okta", "sub_skip").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi_skip"))
	// Mapping miss for "random-external" → silent skip, no INSERT into
	// IamGroupMembership.
	mock.ExpectQuery(`FROM "IdpGroupMapping"`).
		WithArgs("idp_okta", "random-external").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectCommit()

	_, _, err := s.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:           "idp_okta",
		ExternalSubject: "sub_skip",
		Email:           emailStr,
		Groups:          []string{"random-external", ""}, // empty entry also skipped
		CreatedBy:       "okta",
	})
	if err != nil {
		t.Fatalf("JITProvisionUser: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestFederatedStore_JITProvisionUser_GroupsMappingLookupError asserts a
// non-ErrNoRows DB error from the IdpGroupMapping lookup rolls back the
// whole tx — we MUST NOT commit a JIT user with a half-applied group
// set when the mapping table is unreachable.
func TestFederatedStore_JITProvisionUser_GroupsMappingLookupError(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	boom := errors.New("mapping table down")

	emailStr := "jit@nexus.ai"
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs(emailStr, &emailStr, "okta").
		WillReturnRows(pgxmock.NewRows([]string{"id", "displayName", "email", "status", "source"}).
			AddRow("u_err", emailStr, &emailStr, "active", "oidc"))
	mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("u_err", "idp_okta", "sub_err").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi_err"))
	mock.ExpectQuery(`FROM "IdpGroupMapping"`).
		WithArgs("idp_okta", "admins").
		WillReturnError(boom)
	mock.ExpectRollback()

	u, fiID, err := s.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:           "idp_okta",
		ExternalSubject: "sub_err",
		Email:           emailStr,
		Groups:          []string{"admins"},
		CreatedBy:       "okta",
	})
	if u != nil || fiID != "" {
		t.Fatalf("expected nil/empty on mapping err; got u=%v fiID=%q", u, fiID)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected mapping err; got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestFederatedStore_JITProvisionUser_GroupsMembershipInsertError asserts
// a failure on the IamGroupMembership INSERT rolls back — same atomicity
// guarantee as the mapping-lookup error case.
func TestFederatedStore_JITProvisionUser_GroupsMembershipInsertError(t *testing.T) {
	mock, s := newFederatedMock(t)
	ctx := context.Background()
	boom := errors.New("membership insert failed")

	emailStr := "jit@nexus.ai"
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs(emailStr, &emailStr, "okta").
		WillReturnRows(pgxmock.NewRows([]string{"id", "displayName", "email", "status", "source"}).
			AddRow("u_ins", emailStr, &emailStr, "active", "oidc"))
	mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("u_ins", "idp_okta", "sub_ins").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi_ins"))
	mock.ExpectQuery(`FROM "IdpGroupMapping"`).
		WithArgs("idp_okta", "admins").
		WillReturnRows(pgxmock.NewRows([]string{"iamGroupId"}).AddRow("iam_super_admins"))
	mock.ExpectExec(`INSERT INTO "IamGroupMembership"`).
		WithArgs("iam_super_admins", "u_ins").
		WillReturnError(boom)
	mock.ExpectRollback()

	u, fiID, err := s.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:           "idp_okta",
		ExternalSubject: "sub_ins",
		Email:           emailStr,
		Groups:          []string{"admins"},
		CreatedBy:       "okta",
	})
	if u != nil || fiID != "" {
		t.Fatalf("expected nil/empty on membership err; got u=%v fiID=%q", u, fiID)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected membership err; got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
