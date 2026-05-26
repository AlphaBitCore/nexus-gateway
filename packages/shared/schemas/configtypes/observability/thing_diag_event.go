// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package observability

import (
	"encoding/json"
	"time"
)

// ThingDiagEventTableName is the PostgreSQL table name for this model.
const ThingDiagEventTableName = "thing_diag_event"

// ThingDiagEvent -- generated from schema.prisma model.
type ThingDiagEvent struct {
	Id           string          `db:"id"`
	ThingId      string          `db:"thing_id"`
	ThingType    string          `db:"thing_type"`
	OccurredAt   time.Time       `db:"occurred_at"`
	ReceivedAt   time.Time       `db:"received_at"`
	Level        string          `db:"level"`
	EventType    string          `db:"event_type"`
	Source       string          `db:"source"`
	Message      string          `db:"message"`
	MessageHash  string          `db:"message_hash"`
	Attrs        json.RawMessage `db:"attrs"`
	StackTrace   *string         `db:"stack_trace"`
	RepeatCount  int32           `db:"repeat_count"`
	AgentVersion *string         `db:"agent_version"`
	OsInfo       json.RawMessage `db:"os_info"`
}
