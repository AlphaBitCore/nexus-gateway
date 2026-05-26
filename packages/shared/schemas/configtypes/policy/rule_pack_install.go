// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package policy

import (
	"time"
)

// RulePackInstallTableName is the PostgreSQL table name for this model.
const RulePackInstallTableName = "rule_pack_install"

// RulePackInstall -- generated from schema.prisma model.
type RulePackInstall struct {
	Id          string    `db:"id"`
	PackId      string    `db:"pack_id"`
	PinVersion  string    `db:"pin_version"`
	BoundHookId string    `db:"bound_hook_id"`
	Enabled     bool      `db:"enabled"`
	InstalledAt time.Time `db:"installed_at"`
}
