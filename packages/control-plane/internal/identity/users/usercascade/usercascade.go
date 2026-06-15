// Package usercascade owns the single, FK-correct ordering used to remove a
// NexusUser account together with the auth/authz artifacts that reference it.
//
// It is the ONE source of the deletion ordering shared by both account-removal
// callers:
//
//   - admin hard delete (internal/identity/users/userstore.DeleteNexusUser)
//     — a genuine account removal; and
//   - GDPR Art.17 erasure (internal/governance/dsar/dsarstore.FulfillDSARErasure)
//     — which removes the same account record + owned auth
//     artifacts as its final stage (it additionally anonymises the subject's
//     traffic footprint, which is NOT this helper's concern).
//
// The ordering matters because the NexusUser row has referrers with mixed FK
// actions; a naive `DELETE FROM "NexusUser"` fails or orphans data:
//
//   - ScimToken.createdBy is ON DELETE RESTRICT — these rows BLOCK the delete
//     and MUST be removed first.
//   - VirtualKey.ownerId / AdminApiKey.ownerUserId are ON DELETE SET NULL, so
//     deleting the user alone would orphan-null them; a removed account's keys
//     are not retained, so they are deleted outright.
//   - UserFederatedIdentity.userId / RefreshToken.userId are ON DELETE CASCADE;
//     they are deleted explicitly so the count is reported and the order is
//     independent of the cascade configuration.
//   - IamGroupMembership / IamPolicyAttachment for the admin_user principal have
//     no FK to NexusUser (they key on a string principalId), so they do not
//     block the delete — but a full account removal must not leave orphaned
//     authz grants behind, so they are cleared too.
//   - The NexusUser row is deleted LAST. Its remaining referrers
//     (DeviceAssignment.userId, ThingDiagModeWindow.setBy,
//     MetricOpsRetentionConfig.updatedBy) are ON DELETE SET NULL and resolve
//     automatically.
//
// AdminAuditLog (the tamper-evident hash chain) is DELIBERATELY NOT touched:
// breaking the chain would destroy the tamper-evidence the gateway relies on as
// a compliance control, and those rows carry no subject PII beyond an opaque
// actor id. Audit rows are never deleted by either caller.
package usercascade

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Tx is the minimal transaction surface DeleteUserAccount needs. A *pgx.Tx
// (production) and pgxmock's transaction both satisfy it. The helper does not
// own the transaction lifecycle — the caller begins, commits, and rolls back —
// so the whole sequence composes atomically with any surrounding work (DSAR
// erasure runs the traffic-anonymisation stages in the SAME tx).
type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Counts reports how many rows each stage removed. The caller maps these onto
// its own result type (DSARErasureResult for erasure; discarded for admin
// delete, which only needs success/failure).
type Counts struct {
	VKOwnedDeleted              int
	AdminApiKeysDeleted         int
	FederatedIdentitiesDeleted  int
	RefreshTokensDeleted        int
	ScimTokensDeleted           int
	IamGroupMembershipsDeleted  int
	IamPolicyAttachmentsDeleted int
	// AccountDeleted is true when the NexusUser row itself was removed. It is
	// false when no such row existed (the id was already gone); callers that
	// treat "user not found" as an error inspect this field.
	AccountDeleted bool
}

// DeleteUserAccount removes the NexusUser identified by userID together with the
// auth/authz artifacts that reference it, in FK-correct order, using tx. It does
// NOT begin/commit — the caller owns the transaction so the deletes are atomic
// with any surrounding work. On any failure it returns the partial counts
// gathered so far plus the error; the caller's deferred Rollback undoes the
// whole sequence.
//
// AdminAuditLog is intentionally never touched (see the package doc).
func DeleteUserAccount(ctx context.Context, tx Tx, userID string) (Counts, error) {
	var c Counts
	for _, stage := range []struct {
		name string
		sql  string
		dst  *int
	}{
		{"owned virtual keys", `DELETE FROM "VirtualKey" WHERE "ownerId" = $1`, &c.VKOwnedDeleted},
		{"owned admin api keys", `DELETE FROM "AdminApiKey" WHERE "ownerUserId" = $1`, &c.AdminApiKeysDeleted},
		{"federated identities", `DELETE FROM "UserFederatedIdentity" WHERE "userId" = $1`, &c.FederatedIdentitiesDeleted},
		{"refresh tokens", `DELETE FROM "RefreshToken" WHERE "userId" = $1`, &c.RefreshTokensDeleted},
		// ScimToken.createdBy is ON DELETE RESTRICT — cleared before the NexusUser
		// delete so it cannot block it.
		{"created scim tokens", `DELETE FROM "ScimToken" WHERE "createdBy" = $1`, &c.ScimTokensDeleted},
		{"iam group memberships", `DELETE FROM "IamGroupMembership" WHERE "principalType" = 'admin_user' AND "principalId" = $1`, &c.IamGroupMembershipsDeleted},
		{"iam policy attachments", `DELETE FROM "IamPolicyAttachment" WHERE "principalType" = 'admin_user' AND "principalId" = $1`, &c.IamPolicyAttachmentsDeleted},
	} {
		tag, err := tx.Exec(ctx, stage.sql, userID)
		if err != nil {
			return c, fmt.Errorf("delete %s: %w", stage.name, err)
		}
		*stage.dst = int(tag.RowsAffected())
	}

	tag, err := tx.Exec(ctx, `DELETE FROM "NexusUser" WHERE id = $1`, userID)
	if err != nil {
		return c, fmt.Errorf("delete account record: %w", err)
	}
	c.AccountDeleted = tag.RowsAffected() > 0
	return c, nil
}
