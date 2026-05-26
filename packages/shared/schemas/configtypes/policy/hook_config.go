// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package policy

import (
	"encoding/json"
	"time"
)

// HookConfigTableName is the PostgreSQL table name for this model.
const HookConfigTableName = "HookConfig"

// HookConfig -- generated from schema.prisma model.
type HookConfig struct {
	Id                string          `db:"id"`
	Name              string          `db:"name"`
	Type              string          `db:"type"`
	ImplementationId  string          `db:"implementation_id"`
	Stage             string          `db:"stage"`
	Category          *string         `db:"category"`
	Endpoint          *string         `db:"endpoint"`
	Script            *string         `db:"script"`
	Config            json.RawMessage `db:"config"`
	Priority          int32           `db:"priority"`
	TimeoutMs         int32           `db:"timeout_ms"`
	FailBehavior      string          `db:"fail_behavior"`
	Enabled           bool            `db:"enabled"`
	ApplicableIngress []string        `db:"applicable_ingress"`
	CreatedAt         time.Time       `db:"created_at"`
	UpdatedAt         time.Time       `db:"updated_at"`
}
