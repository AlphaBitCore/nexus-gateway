// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package policy

import (
	"time"
)

// RulePackTableName is the PostgreSQL table name for this model.
const RulePackTableName = "rule_pack"

// RulePack -- generated from schema.prisma model.
type RulePack struct {
	Id          string    `db:"id"`
	Name        string    `db:"name"`
	Version     string    `db:"version"`
	Maintainer  string    `db:"maintainer"`
	Description *string   `db:"description"`
	CreatedAt   time.Time `db:"created_at"`
}
