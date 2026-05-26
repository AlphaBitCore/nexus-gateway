// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package identity

import (
	"time"
)

// RefreshTokenTableName is the PostgreSQL table name for this model.
const RefreshTokenTableName = "RefreshToken"

// RefreshToken -- generated from schema.prisma model.
type RefreshToken struct {
	Jti       string     `db:"jti"`
	SessionId string     `db:"session_id"`
	ParentJti *string    `db:"parent_jti"`
	UserId    string     `db:"user_id"`
	ClientId  string     `db:"client_id"`
	DeviceId  *string    `db:"device_id"`
	TokenHash []byte     `db:"token_hash"`
	UsedAt    *time.Time `db:"used_at"`
	ExpiresAt time.Time  `db:"expires_at"`
	CreatedAt time.Time  `db:"created_at"`
}
