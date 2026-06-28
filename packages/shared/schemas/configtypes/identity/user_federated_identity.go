// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package identity

import (
	"github.com/goccy/go-json"
	"time"
)

// UserFederatedIdentityTableName is the PostgreSQL table name for this model.
const UserFederatedIdentityTableName = "UserFederatedIdentity"

// UserFederatedIdentity -- generated from schema.prisma model.
type UserFederatedIdentity struct {
	Id              string          `db:"id"`
	UserId          string          `db:"user_id"`
	IdpId           string          `db:"idp_id"`
	ExternalSubject string          `db:"external_subject"`
	ExternalEmail   *string         `db:"external_email"`
	RawClaims       json.RawMessage `db:"raw_claims"`
	LinkedAt        time.Time       `db:"linked_at"`
	LastLoginAt     *time.Time      `db:"last_login_at"`
}
