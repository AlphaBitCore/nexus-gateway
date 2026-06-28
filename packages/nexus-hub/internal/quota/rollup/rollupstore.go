// Package rollupstore provides schema-level helpers for metric rollup tables
// (metric_rollup_5m / _1h / _1d / _1mo) and the shared rollup_watermark
// table. Consumed by the Hub scheduler jobs that produce rollups. The Control
// Plane reads the same tables through its own metricsstore / thingstore
// packages, which mirror these SQL conventions rather than importing this one.
//
// All functions accept the minimal PgxPool interface (satisfied by
// *pgxpool.Pool in production) or pgx.Tx directly so the package has no
// dependency on any service-specific store wrapper, and unit tests can drive
// every SQL path via pgxmock without a live Postgres.
package rollupstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// PgxPool is the minimum pgx pool surface this package needs. The concrete
// *pgxpool.Pool satisfies it in production; pgxmock's PgxPoolIface satisfies
// it in tests, letting every SQL path be unit-tested without a live Postgres.
// Mirrors the PgxPool seam in packages/control-plane/internal/store,
// packages/ai-gateway/internal/store, packages/nexus-hub/internal/store, and
// packages/shared/storage/configstore.
//
// PgxPool interface seam — production passes *pgxpool.Pool; tests pass
// pgxmock. Lifts this package off the integration-only allowlist while
// honoring the [[tests-only-own-data]] binding: unit tests do not
// mutate the shared dev DB.
type PgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Watermark helpers

// GetWatermark returns the last committed bucket for jobName, or zero time if
// no row exists yet. Zero time is the signal "start from the beginning of
// available data"; callers decide how to backfill from there.
func GetWatermark(ctx context.Context, pool PgxPool, jobName string) (time.Time, error) {
	var wm time.Time
	err := pool.QueryRow(ctx,
		`SELECT "watermark" FROM "rollup_watermark" WHERE "jobName" = $1`,
		jobName,
	).Scan(&wm)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get watermark %q: %w", jobName, err)
	}
	return wm, nil
}

// SetWatermark upserts the watermark for jobName using the provided
// transaction. Callers typically set the watermark in the same transaction
// that wrote the corresponding rollup rows so the two advance atomically.
func SetWatermark(ctx context.Context, tx pgx.Tx, jobName string, wm time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO "rollup_watermark" ("jobName", "watermark", "updatedAt")
		VALUES ($1, $2, NOW())
		ON CONFLICT ("jobName") DO UPDATE
		SET "watermark" = EXCLUDED."watermark",
		    "updatedAt" = NOW()
	`, jobName, wm)
	if err != nil {
		return fmt.Errorf("set watermark %q: %w", jobName, err)
	}
	return nil
}

// WatermarkStatus is the public shape exposed by the "list watermarks" API.
type WatermarkStatus struct {
	JobName   string    `json:"jobName"`
	Watermark time.Time `json:"watermark"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ListWatermarks returns all rows from rollup_watermark ordered by jobName.
func ListWatermarks(ctx context.Context, pool PgxPool) ([]WatermarkStatus, error) {
	rows, err := pool.Query(ctx,
		`SELECT "jobName", "watermark", "updatedAt"
		 FROM "rollup_watermark"
		 ORDER BY "jobName"`,
	)
	if err != nil {
		return nil, fmt.Errorf("list watermarks: %w", err)
	}
	defer rows.Close()

	var out []WatermarkStatus
	for rows.Next() {
		var w WatermarkStatus
		if err := rows.Scan(&w.JobName, &w.Watermark, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan watermark: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate watermarks: %w", err)
	}
	return out, nil
}

// Bucket helpers

// InsertRollupRows bulk-inserts rows into the specified rollup table inside
// the provided transaction. Table must be one of the rollup tables:
// metric_rollup_5m / _1h / _1d / _1mo. On conflict (same bucket+metric+
// dimension) the row is updated in place so re-running a job is safe.
func InsertRollupRows(ctx context.Context, tx pgx.Tx, table string, rows []metrics.RollupRow) error {
	if len(rows) == 0 {
		return nil
	}

	query := fmt.Sprintf(
		`INSERT INTO "%s" ("id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt")
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6::jsonb, NOW())
		 ON CONFLICT ("bucketStart", "metricName", "dimensionKey", "subDimension") DO UPDATE
		 SET "value" = EXCLUDED."value", "metadata" = EXCLUDED."metadata", "updatedAt" = NOW()`,
		table,
	)

	for _, r := range rows {
		meta := "null"
		if len(r.Metadata) > 0 {
			meta = string(r.Metadata)
		}
		if _, err := tx.Exec(ctx, query,
			r.BucketStart, r.MetricName, r.DimensionKey, r.SubDimension, r.Value, meta,
		); err != nil {
			return fmt.Errorf("insert rollup rows into %s: %w", table, err)
		}
	}
	return nil
}

// DeleteRollupBucket removes every row for bucketStart in the given table
// inside the provided transaction. Used by the 5m "correction" job to clear a
// bucket before reinserting it with corrected data.
func DeleteRollupBucket(ctx context.Context, tx pgx.Tx, table string, bucketStart time.Time) error {
	q := fmt.Sprintf(`DELETE FROM "%s" WHERE "bucketStart" = $1`, table)
	if _, err := tx.Exec(ctx, q, bucketStart); err != nil {
		return fmt.Errorf("delete rollup bucket %s/%v: %w", table, bucketStart, err)
	}
	return nil
}

// PurgeRollupBefore removes every row with bucketStart < cutoff from the
// given table. Returns the number of rows deleted. Used by retention jobs.
func PurgeRollupBefore(ctx context.Context, pool PgxPool, table string, cutoff time.Time) (int64, error) {
	q := fmt.Sprintf(`DELETE FROM "%s" WHERE "bucketStart" < $1`, table)
	tag, err := pool.Exec(ctx, q, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge rollup %s before %v: %w", table, cutoff, err)
	}
	return tag.RowsAffected(), nil
}

// EarliestBucketStart returns the smallest bucketStart in the given table.
// The second return value is false when the table is empty. Used by merge
// jobs on cold start to decide how far back to backfill: if the source table
// already holds data older than the configured initial lookback, the merge
// should cover it rather than leaking history into an unqueryable gap.
func EarliestBucketStart(ctx context.Context, pool PgxPool, table string) (time.Time, bool, error) {
	var earliest *time.Time
	q := fmt.Sprintf(`SELECT MIN("bucketStart") FROM "%s"`, table)
	if err := pool.QueryRow(ctx, q).Scan(&earliest); err != nil {
		return time.Time{}, false, fmt.Errorf("earliest bucket %s: %w", table, err)
	}
	if earliest == nil {
		return time.Time{}, false, nil
	}
	return *earliest, true, nil
}

// EarliestTrafficEventTimestamp returns the smallest timestamp in
// traffic_event. The second return value is false when the table is empty.
// Used by the rollup-5m job on cold start to decide how far back to
// backfill: if traffic_event already holds events older than the configured
// initial lookback (e.g. after a fresh seed with historical timestamps, or
// after a restart that retained traffic data), the aggregator should cover
// them instead of leaking history into an unqueryable gap.
func EarliestTrafficEventTimestamp(ctx context.Context, pool PgxPool) (time.Time, bool, error) {
	var earliest *time.Time
	if err := pool.QueryRow(ctx, `SELECT MIN("timestamp") FROM traffic_event`).Scan(&earliest); err != nil {
		return time.Time{}, false, fmt.Errorf("earliest traffic_event timestamp: %w", err)
	}
	if earliest == nil {
		return time.Time{}, false, nil
	}
	return *earliest, true, nil
}

// RollupHasData returns true if at least one row in the given table falls in
// [start, end). Used as a pre-check before running an aggregation or merge.
func RollupHasData(ctx context.Context, pool PgxPool, table string, start, end time.Time) (bool, error) {
	var exists bool
	q := fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM "%s" WHERE "bucketStart" >= $1 AND "bucketStart" < $2)`,
		table,
	)
	if err := pool.QueryRow(ctx, q, start, end).Scan(&exists); err != nil {
		return false, fmt.Errorf("rollup has data %s: %w", table, err)
	}
	return exists, nil
}

// Merge-source readers

// QueryRollupMergeSource returns every row in sourceTable whose bucketStart
// is in [from, to), ordered by bucketStart. Used by merge jobs that roll 5m
// data up into 1h, 1h into 1d, and so on.
func QueryRollupMergeSource(ctx context.Context, pool PgxPool, sourceTable string, from, to time.Time) ([]metrics.RollupRow, error) {
	sql := fmt.Sprintf(`
		SELECT "id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"
		FROM "%s"
		WHERE "bucketStart" >= $1 AND "bucketStart" < $2
		ORDER BY "bucketStart" ASC
	`, sourceTable)

	rows, err := pool.Query(ctx, sql, from, to)
	if err != nil {
		return nil, fmt.Errorf("query rollup merge source %s: %w", sourceTable, err)
	}
	return scanRollupRows(rows)
}

// QueryRollup reads rollup rows matching a MetricsQuery. It auto-selects the
// rollup table by calling metrics.SelectGranularity on the query time range.
// Returned rows are ordered by bucketStart ascending.
func QueryRollup(ctx context.Context, pool PgxPool, q metrics.MetricsQuery) ([]metrics.RollupRow, error) {
	gran := metrics.SelectGranularity(q.StartTime, q.EndTime)
	table := gran.TableName()

	where := `"bucketStart" >= $1 AND "bucketStart" < $2`
	args := []any{q.StartTime, q.EndTime}
	argIdx := 3

	if len(q.Metrics) > 0 {
		placeholders := make([]string, len(q.Metrics))
		for i, m := range q.Metrics {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, m)
			argIdx++
		}
		where += ` AND "metricName" IN (` + strings.Join(placeholders, ", ") + `)`
	}

	if q.DimensionKey == "" {
		where += ` AND "dimensionKey" = ''`
	} else {
		where += fmt.Sprintf(` AND "dimensionKey" LIKE $%d`, argIdx)
		args = append(args, q.DimensionKey+"=%")
		argIdx++
	}

	if q.SubDimension != "" {
		where += fmt.Sprintf(` AND "subDimension" = $%d`, argIdx)
		args = append(args, q.SubDimension)
	}

	sql := fmt.Sprintf(`
		SELECT "id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"
		FROM "%s"
		WHERE %s
		ORDER BY "bucketStart" ASC
	`, table, where)

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query rollup: %w", err)
	}
	return scanRollupRows(rows)
}

func scanRollupRows(rows pgx.Rows) ([]metrics.RollupRow, error) {
	defer rows.Close()

	var result []metrics.RollupRow
	for rows.Next() {
		var r metrics.RollupRow
		var meta []byte
		if err := rows.Scan(
			&r.ID, &r.BucketStart, &r.MetricName,
			&r.DimensionKey, &r.SubDimension,
			&r.Value, &meta, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan rollup row: %w", err)
		}
		if meta != nil {
			r.Metadata = json.RawMessage(meta)
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rollup rows: %w", err)
	}
	return result, nil
}

// Thing rollup helpers (table = thing_metric_rollup_*)

// InsertThingRollupRows bulk-inserts per-Thing rollup rows. table must be one
// of thing_metric_rollup_5m / _1h / _1d / _1mo. Same idempotency contract as
// InsertRollupRows; ON CONFLICT on the composite key including thing_id.
func InsertThingRollupRows(ctx context.Context, tx pgx.Tx, table string, rows []metrics.ThingRollupRow) error {
	if len(rows) == 0 {
		return nil
	}
	query := fmt.Sprintf(
		`INSERT INTO "%s" ("id", "bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt")
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7::jsonb, NOW())
		 ON CONFLICT ("bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension") DO UPDATE
		 SET "value" = EXCLUDED."value", "metadata" = EXCLUDED."metadata", "updatedAt" = NOW()`,
		table,
	)
	for _, r := range rows {
		meta := "null"
		if len(r.Metadata) > 0 {
			meta = string(r.Metadata)
		}
		if _, err := tx.Exec(ctx, query,
			r.BucketStart, r.ThingID, r.MetricName, r.DimensionKey, r.SubDimension, r.Value, meta,
		); err != nil {
			return fmt.Errorf("insert thing rollup rows into %s: %w", table, err)
		}
	}
	return nil
}

// DeleteThingRollupBucket removes every row for bucketStart in the given thing
// rollup table. Used by the 5m correction path to clear a bucket before
// reinserting it. Bucket-wide delete is intentional — re-aggregation rebuilds
// every per-Thing row for that bucket.
func DeleteThingRollupBucket(ctx context.Context, tx pgx.Tx, table string, bucketStart time.Time) error {
	q := fmt.Sprintf(`DELETE FROM "%s" WHERE "bucketStart" = $1`, table)
	if _, err := tx.Exec(ctx, q, bucketStart); err != nil {
		return fmt.Errorf("delete thing rollup bucket %s/%v: %w", table, bucketStart, err)
	}
	return nil
}

// PurgeThingRollupBefore removes every row with bucketStart < cutoff. Used by
// retention jobs that prune per-Thing history at the same schedule as the
// fleet rollup retention.
func PurgeThingRollupBefore(ctx context.Context, pool PgxPool, table string, cutoff time.Time) (int64, error) {
	q := fmt.Sprintf(`DELETE FROM "%s" WHERE "bucketStart" < $1`, table)
	tag, err := pool.Exec(ctx, q, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge thing rollup %s before %v: %w", table, cutoff, err)
	}
	return tag.RowsAffected(), nil
}

// QueryThingRollupMergeSource returns thing rollup rows in [from, to) for one
// thing rollup table. Used by the per-Thing merge cascade (5m→1h→1d→1mo).
// Rows are ordered by bucketStart, thing_id so the merge consumer can batch
// per-bucket-per-thing groupings without external sorting.
func QueryThingRollupMergeSource(ctx context.Context, pool PgxPool, sourceTable string, from, to time.Time) ([]metrics.ThingRollupRow, error) {
	sql := fmt.Sprintf(`
		SELECT "id", "bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"
		FROM "%s"
		WHERE "bucketStart" >= $1 AND "bucketStart" < $2
		ORDER BY "bucketStart" ASC, "thing_id" ASC
	`, sourceTable)

	rows, err := pool.Query(ctx, sql, from, to)
	if err != nil {
		return nil, fmt.Errorf("query thing rollup merge source %s: %w", sourceTable, err)
	}
	return scanThingRollupRows(rows)
}

// ThingMetricsQuery describes a per-Thing rollup read. The reader strictly
// requires ThingID to prevent unbounded scans; passing "" returns an error.
type ThingMetricsQuery struct {
	ThingID      string
	Metrics      []string
	DimensionKey string
	SubDimension string
	StartTime    time.Time
	EndTime      time.Time
}

// QueryThingRollup reads per-Thing rollup rows for one ThingID. Granule is
// auto-selected via metrics.SelectGranularity. ThingID is mandatory.
func QueryThingRollup(ctx context.Context, pool PgxPool, q ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
	if q.ThingID == "" {
		return nil, errors.New("query thing rollup: thingID is required")
	}
	gran := metrics.SelectGranularity(q.StartTime, q.EndTime)
	table := "thing_" + gran.TableName()

	where := `"thing_id" = $1 AND "bucketStart" >= $2 AND "bucketStart" < $3`
	args := []any{q.ThingID, q.StartTime, q.EndTime}
	argIdx := 4

	if len(q.Metrics) > 0 {
		placeholders := make([]string, len(q.Metrics))
		for i, m := range q.Metrics {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, m)
			argIdx++
		}
		where += ` AND "metricName" IN (` + strings.Join(placeholders, ", ") + `)`
	}

	if q.DimensionKey == "" {
		where += ` AND "dimensionKey" = ''`
	} else {
		where += fmt.Sprintf(` AND "dimensionKey" LIKE $%d`, argIdx)
		args = append(args, q.DimensionKey+"=%")
		argIdx++
	}

	if q.SubDimension != "" {
		where += fmt.Sprintf(` AND "subDimension" = $%d`, argIdx)
		args = append(args, q.SubDimension)
	}

	sql := fmt.Sprintf(`
		SELECT "id", "bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"
		FROM "%s"
		WHERE %s
		ORDER BY "bucketStart" ASC
	`, table, where)

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query thing rollup: %w", err)
	}
	return scanThingRollupRows(rows)
}

func scanThingRollupRows(rows pgx.Rows) ([]metrics.ThingRollupRow, error) {
	defer rows.Close()

	var result []metrics.ThingRollupRow
	for rows.Next() {
		var r metrics.ThingRollupRow
		var meta []byte
		if err := rows.Scan(
			&r.ID, &r.BucketStart, &r.ThingID, &r.MetricName,
			&r.DimensionKey, &r.SubDimension,
			&r.Value, &meta, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan thing rollup row: %w", err)
		}
		if meta != nil {
			r.Metadata = json.RawMessage(meta)
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate thing rollup rows: %w", err)
	}
	return result, nil
}

// EarliestThingBucketStart returns the smallest bucketStart in the given thing
// rollup table. Used by thing merge jobs on cold start.
func EarliestThingBucketStart(ctx context.Context, pool PgxPool, table string) (time.Time, bool, error) {
	var earliest *time.Time
	q := fmt.Sprintf(`SELECT MIN("bucketStart") FROM "%s"`, table)
	if err := pool.QueryRow(ctx, q).Scan(&earliest); err != nil {
		return time.Time{}, false, fmt.Errorf("earliest thing bucket %s: %w", table, err)
	}
	if earliest == nil {
		return time.Time{}, false, nil
	}
	return *earliest, true, nil
}
