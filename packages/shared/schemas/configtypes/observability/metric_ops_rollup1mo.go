// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package observability

import (
	"encoding/json"
	"time"
)

// MetricOpsRollup1moTableName is the PostgreSQL table name for this model.
const MetricOpsRollup1moTableName = "metric_ops_rollup_1mo"

// MetricOpsRollup1mo -- generated from schema.prisma model.
type MetricOpsRollup1mo struct {
	Id           string          `db:"id"`
	BucketStart  time.Time       `db:"bucket_start"`
	ThingId      *string         `db:"thing_id"`
	ThingType    string          `db:"thing_type"`
	MetricName   string          `db:"metric_name"`
	MetricKind   string          `db:"metric_kind"`
	DimensionKey string          `db:"dimension_key"`
	ValueAvg     *float64        `db:"value_avg"`
	ValueSum     *float64        `db:"value_sum"`
	ValueMin     *float64        `db:"value_min"`
	ValueMax     *float64        `db:"value_max"`
	SampleCount  int32           `db:"sample_count"`
	Metadata     json.RawMessage `db:"metadata"`
}
