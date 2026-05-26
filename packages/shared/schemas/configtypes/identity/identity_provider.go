// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package identity

import (
	"encoding/json"
	"time"
)

// IdentityProviderTableName is the PostgreSQL table name for this model.
const IdentityProviderTableName = "IdentityProvider"

// IdentityProvider -- generated from schema.prisma model.
type IdentityProvider struct {
	Id          string          `db:"id"`
	Type        string          `db:"type"`
	Name        string          `db:"name"`
	Enabled     bool            `db:"enabled"`
	Config      json.RawMessage `db:"config"`
	RoleMapping json.RawMessage `db:"role_mapping"`
	DefaultRole string          `db:"default_role"`
	JitEnabled  bool            `db:"jit_enabled"`
	CreatedAt   time.Time       `db:"created_at"`
	UpdatedAt   time.Time       `db:"updated_at"`
}
