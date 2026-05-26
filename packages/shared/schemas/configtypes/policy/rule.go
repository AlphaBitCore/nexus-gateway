// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package policy

// RuleTableName is the PostgreSQL table name for this model.
const RuleTableName = "rule"

// Rule -- generated from schema.prisma model.
type Rule struct {
	Id          string   `db:"id"`
	PackId      string   `db:"pack_id"`
	RuleId      string   `db:"rule_id"`
	Category    string   `db:"category"`
	Severity    string   `db:"severity"`
	Pattern     string   `db:"pattern"`
	Flags       *string  `db:"flags"`
	Description *string  `db:"description"`
	Labels      []string `db:"labels"`
}
