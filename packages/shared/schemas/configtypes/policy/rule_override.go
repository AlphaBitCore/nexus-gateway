// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package policy

import (
	"time"
)

// RuleOverrideTableName is the PostgreSQL table name for this model.
const RuleOverrideTableName = "rule_override"

// RuleOverride -- generated from schema.prisma model.
type RuleOverride struct {
	Id               string    `db:"id"`
	InstallId        string    `db:"install_id"`
	RuleLocalId      string    `db:"rule_local_id"`
	Disabled         bool      `db:"disabled"`
	SeverityOverride *string   `db:"severity_override"`
	UpdatedAt        time.Time `db:"updated_at"`
}
