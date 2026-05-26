// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package observability

import (
	"time"
)

// MetricOpsRetentionConfigTableName is the PostgreSQL table name for this model.
const MetricOpsRetentionConfigTableName = "metric_ops_retention_config"

// MetricOpsRetentionConfig -- generated from schema.prisma model.
type MetricOpsRetentionConfig struct {
	Layer         string    `db:"layer"`
	RetentionDays int32     `db:"retention_days"`
	UpdatedAt     time.Time `db:"updated_at"`
	UpdatedBy     *string   `db:"updated_by"`
}
