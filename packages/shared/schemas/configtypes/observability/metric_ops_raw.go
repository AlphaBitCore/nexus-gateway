// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package observability

import (
	"github.com/goccy/go-json"
	"time"
)

// MetricOpsRawTableName is the PostgreSQL table name for this model.
const MetricOpsRawTableName = "metric_ops_raw"

// MetricOpsRaw -- generated from schema.prisma model.
type MetricOpsRaw struct {
	Id           string          `db:"id"`
	SampledAt    time.Time       `db:"sampled_at"`
	ThingId      string          `db:"thing_id"`
	ThingType    string          `db:"thing_type"`
	MetricName   string          `db:"metric_name"`
	MetricKind   string          `db:"metric_kind"`
	DimensionKey string          `db:"dimension_key"`
	Value        *float64        `db:"value"`
	Metadata     json.RawMessage `db:"metadata"`
}
