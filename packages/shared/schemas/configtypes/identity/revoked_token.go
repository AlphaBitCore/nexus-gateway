// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package identity

import (
	"time"
)

// RevokedTokenTableName is the PostgreSQL table name for this model.
const RevokedTokenTableName = "RevokedToken"

// RevokedToken -- generated from schema.prisma model.
type RevokedToken struct {
	Id              int64     `db:"id"`
	Scope           string    `db:"scope"`
	TargetJti       *string   `db:"target_jti"`
	TargetUserId    *string   `db:"target_user_id"`
	TargetDeviceId  *string   `db:"target_device_id"`
	TargetSessionId *string   `db:"target_session_id"`
	RevokedAt       time.Time `db:"revoked_at"`
	ExpiresAt       time.Time `db:"expires_at"`
	Reason          string    `db:"reason"`
	Actor           *string   `db:"actor"`
}
