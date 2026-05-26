// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package interception

import (
	"time"
)

// InterceptionPathTableName is the PostgreSQL table name for this model.
const InterceptionPathTableName = "interception_path"

// InterceptionPath -- generated from schema.prisma model.
type InterceptionPath struct {
	Id          string        `db:"id"`
	DomainId    string        `db:"domain_id"`
	PathPattern []string      `db:"path_pattern"`
	MatchType   PathMatchType `db:"match_type"`
	Action      PathAction    `db:"action"`
	Priority    int32         `db:"priority"`
	Description *string       `db:"description"`
	Enabled     bool          `db:"enabled"`
	CreatedAt   time.Time     `db:"created_at"`
	UpdatedAt   time.Time     `db:"updated_at"`
}
