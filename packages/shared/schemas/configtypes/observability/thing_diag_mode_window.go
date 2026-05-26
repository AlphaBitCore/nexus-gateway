// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package observability

import (
	"time"
)

// ThingDiagModeWindowTableName is the PostgreSQL table name for this model.
const ThingDiagModeWindowTableName = "thing_diag_mode_window"

// ThingDiagModeWindow -- generated from schema.prisma model.
type ThingDiagModeWindow struct {
	Id        string    `db:"id"`
	ThingId   string    `db:"thing_id"`
	StartedAt time.Time `db:"started_at"`
	EndedAt   time.Time `db:"ended_at"`
	SetBy     *string   `db:"set_by"`
	Reason    *string   `db:"reason"`
	CreatedAt time.Time `db:"created_at"`
}
