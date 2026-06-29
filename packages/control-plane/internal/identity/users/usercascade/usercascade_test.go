package usercascade

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// beginTx starts a mocked transaction and returns it as the helper's Tx.
func beginTx(t *testing.T, m pgxmock.PgxPoolIface) Tx {
	t.Helper()
	tx, err := m.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tx
}

// expectCascade queues the full FK-correct delete sequence with per-stage row
// counts so the test can assert each count maps to the right struct field and
// that the statements are issued in RESTRICT-safe order (ScimToken before the
// NexusUser delete).
func expectCascade(m pgxmock.PgxPoolIface, accountRows int64) {
	m.ExpectBegin()
	m.ExpectExec(`DELETE FROM "VirtualKey" WHERE "ownerId" = \$1`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 2))
	m.ExpectExec(`DELETE FROM "AdminApiKey" WHERE "ownerUserId" = \$1`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	m.ExpectExec(`DELETE FROM "UserFederatedIdentity" WHERE "userId" = \$1`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 3))
	m.ExpectExec(`DELETE FROM "RefreshToken" WHERE "userId" = \$1`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 4))
	m.ExpectExec(`DELETE FROM "ScimToken" WHERE "createdBy" = \$1`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 5))
	m.ExpectExec(`DELETE FROM "IamGroupMembership" WHERE "principalType" = 'admin_user' AND "principalId" = \$1`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 6))
	m.ExpectExec(`DELETE FROM "IamPolicyAttachment" WHERE "principalType" = 'admin_user' AND "principalId" = \$1`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 7))
	m.ExpectExec(`DELETE FROM "NexusUser" WHERE id = \$1`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", accountRows))
}

func newMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// TestDeleteUserAccountHappyPath asserts the helper issues every FK-dependent
// delete in RESTRICT-safe order within the tx, reports the per-stage counts, and
// — critically — clears the ScimToken RESTRICT row BEFORE the
// NexusUser delete so it no longer blocks deletion. It also asserts that no
// AdminAuditLog delete is ever queued (ExpectationsWereMet would fail if one
// were issued).
func TestDeleteUserAccountHappyPath(t *testing.T) {
	m := newMock(t)
	expectCascade(m, 1)
	tx := beginTx(t, m)

	c, err := DeleteUserAccount(context.Background(), tx, "u1")
	if err != nil {
		t.Fatalf("DeleteUserAccount: %v", err)
	}
	if c.VKOwnedDeleted != 2 || c.AdminApiKeysDeleted != 1 || c.FederatedIdentitiesDeleted != 3 ||
		c.RefreshTokensDeleted != 4 || c.ScimTokensDeleted != 5 ||
		c.IamGroupMembershipsDeleted != 6 || c.IamPolicyAttachmentsDeleted != 7 {
		t.Fatalf("per-stage counts mismatch: %+v", c)
	}
	if !c.AccountDeleted {
		t.Fatalf("AccountDeleted = false; want true")
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("statement order/audit-chain-preserved invariant: %v", err)
	}
}

// TestDeleteUserAccountNotFound covers the no-such-user path: every dependent
// delete affects 0 rows and the final NexusUser delete affects 0, so
// AccountDeleted is false and the caller can map that to not-found.
func TestDeleteUserAccountNotFound(t *testing.T) {
	m := newMock(t)
	m.ExpectBegin()
	for _, re := range []string{
		`DELETE FROM "VirtualKey"`, `DELETE FROM "AdminApiKey"`,
		`DELETE FROM "UserFederatedIdentity"`, `DELETE FROM "RefreshToken"`,
		`DELETE FROM "ScimToken"`, `DELETE FROM "IamGroupMembership"`,
		`DELETE FROM "IamPolicyAttachment"`, `DELETE FROM "NexusUser"`,
	} {
		m.ExpectExec(re).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	}
	tx := beginTx(t, m)

	c, err := DeleteUserAccount(context.Background(), tx, "u1")
	if err != nil {
		t.Fatalf("DeleteUserAccount: %v", err)
	}
	if c.AccountDeleted {
		t.Fatalf("AccountDeleted = true; want false for missing user")
	}
}

// TestDeleteUserAccountDependentError asserts a failure in a dependent delete
// (here the ScimToken RESTRICT clear) surfaces with the stage name and stops the
// sequence before the NexusUser delete.
func TestDeleteUserAccountDependentError(t *testing.T) {
	m := newMock(t)
	m.ExpectBegin()
	m.ExpectExec(`DELETE FROM "VirtualKey"`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	m.ExpectExec(`DELETE FROM "AdminApiKey"`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	m.ExpectExec(`DELETE FROM "UserFederatedIdentity"`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	m.ExpectExec(`DELETE FROM "RefreshToken"`).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	m.ExpectExec(`DELETE FROM "ScimToken"`).WithArgs("u1").WillReturnError(errors.New("restrict"))
	tx := beginTx(t, m)

	_, err := DeleteUserAccount(context.Background(), tx, "u1")
	if err == nil || !errcontains(err, "created scim tokens") {
		t.Fatalf("want scim-token stage error, got %v", err)
	}
}

// TestDeleteUserAccountFinalError asserts a failure on the terminal NexusUser
// delete surfaces with the account-record message.
func TestDeleteUserAccountFinalError(t *testing.T) {
	m := newMock(t)
	m.ExpectBegin()
	for _, re := range []string{
		`DELETE FROM "VirtualKey"`, `DELETE FROM "AdminApiKey"`,
		`DELETE FROM "UserFederatedIdentity"`, `DELETE FROM "RefreshToken"`,
		`DELETE FROM "ScimToken"`, `DELETE FROM "IamGroupMembership"`,
		`DELETE FROM "IamPolicyAttachment"`,
	} {
		m.ExpectExec(re).WithArgs("u1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	}
	m.ExpectExec(`DELETE FROM "NexusUser"`).WithArgs("u1").WillReturnError(errors.New("boom"))
	tx := beginTx(t, m)

	_, err := DeleteUserAccount(context.Background(), tx, "u1")
	if err == nil || !errcontainsAccount(err) {
		t.Fatalf("want account-record error, got %v", err)
	}
}

func errcontains(err error, sub string) bool {
	return err != nil && containsSub(err.Error(), sub)
}

func errcontainsAccount(err error) bool { return errcontains(err, "delete account record") }

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
