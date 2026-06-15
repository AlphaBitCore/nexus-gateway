// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package identity

import (
	"time"
)

// OAuthClientTableName is the PostgreSQL table name for this model.
const OAuthClientTableName = "OAuthClient"

// OAuthClient -- generated from schema.prisma model.
type OAuthClient struct {
	Id                string    `db:"id"`
	Name              string    `db:"name"`
	Type              string    `db:"type"`
	RedirectUris      []string  `db:"redirect_uris"`
	AllowedScopes     []string  `db:"allowed_scopes"`
	AccessTtlSeconds  int32     `db:"access_ttl_seconds"`
	RefreshTtlSeconds int32     `db:"refresh_ttl_seconds"`
	ClientSecretHash  *string   `db:"client_secret_hash"`
	CreatedAt         time.Time `db:"created_at"`
	UpdatedAt         time.Time `db:"updated_at"`
}
