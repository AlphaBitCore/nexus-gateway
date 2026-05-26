package metricsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// InsertRollupRows — bulk-insert into a rollup table within a transaction

// InsertRollupRows inserts rows into the specified metrics rollup table using
// the provided transaction. Table must be one of: metric_rollup_5m,
// metric_rollup_1h, metric_rollup_1d, metric_rollup_1mo.
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
		_, err := tx.Exec(ctx, query, r.BucketStart, r.MetricName, r.DimensionKey, r.SubDimension, r.Value, meta)
		if err != nil {
			return fmt.Errorf("insert rollup rows into %s: %w", table, err)
		}
	}
	return nil
}

// DeleteRollupBucket — delete all rows for a specific bucket_start

// DeleteRollupBucket deletes all rows matching bucketStart in the given table
// within the provided transaction.
func DeleteRollupBucket(ctx context.Context, tx pgx.Tx, table string, bucketStart time.Time) error {
	q := fmt.Sprintf(`DELETE FROM "%s" WHERE "bucketStart" = $1`, table)
	_, err := tx.Exec(ctx, q, bucketStart)
	if err != nil {
		return fmt.Errorf("delete rollup bucket %s/%v: %w", table, bucketStart, err)
	}
	return nil
}

// Watermark — read/write the rollup_watermark table

// GetWatermark returns the current watermark for the named job.
// Returns pgx.ErrNoRows if the watermark has not been initialized.
func (store *Store) GetWatermark(ctx context.Context, jobName string) (time.Time, error) {
	var wm time.Time
	err := store.pool.QueryRow(ctx,
		`SELECT "watermark" FROM "rollup_watermark" WHERE "jobName" = $1`,
		jobName,
	).Scan(&wm)
	if err != nil {
		return time.Time{}, fmt.Errorf("get watermark %q: %w", jobName, err)
	}
	return wm, nil
}

// SetWatermark upserts the watermark for the named job within the provided
// transaction.
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

// QueryRollup — read rollup data for a MetricsQuery

// QueryRollup reads rollup rows matching the given MetricsQuery. It
// auto-selects the rollup table based on the query time range.
func (store *Store) QueryRollup(ctx context.Context, q metrics.MetricsQuery) ([]metrics.RollupRow, error) {
	gran := metrics.SelectGranularity(q.StartTime, q.EndTime)
	return store.queryRollupOnTable(ctx, q, gran.TableName(), q.StartTime, q.EndTime)
}

// queryRollupOnTable runs the rollup SELECT against an explicit table over
// an explicit [from, to) window. Extracted from QueryRollup so callers that
// span the merge watermark (see QueryRollupAware) can issue two queries
// against different tables and concatenate the results.
func (store *Store) queryRollupOnTable(ctx context.Context, q metrics.MetricsQuery, table string, from, to time.Time) ([]metrics.RollupRow, error) {
	if !to.After(from) {
		// Empty window — caller is asking for [t, t) which has no rows.
		// Skip the round-trip and return empty.
		return nil, nil
	}

	where := `"bucketStart" >= $1 AND "bucketStart" < $2`
	args := []any{from, to}
	argIdx := 3

	// Optional metric name filter.
	if len(q.Metrics) > 0 {
		placeholders := make([]string, len(q.Metrics))
		for i, m := range q.Metrics {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, m)
			argIdx++
		}
		where += ` AND "metricName" IN (` + strings.Join(placeholders, ", ") + `)`
	}

	// Dimension filter.
	if q.DimensionKey == "" {
		// Global: match rows with empty dimensionKey.
		where += ` AND "dimensionKey" = ''`
	} else {
		// Match dimension prefix, e.g. "provider=%".
		where += fmt.Sprintf(` AND "dimensionKey" LIKE $%d`, argIdx)
		args = append(args, q.DimensionKey+"=%")
		argIdx++
	}

	// Sub-dimension filter.
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

	rows, err := store.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query rollup: %w", err)
	}
	return scanRollupRows(rows)
}

// rollupTailFor returns the finer-granularity table to query for the
// portion of a range that has not yet been merged into the chosen
// granularity. 5m has no finer source — empty string disables the tail
// query for that case (the writer aggregates traffic_event directly).
func rollupTailFor(gran metrics.Granularity) (table string, watermarkJob string) {
	switch gran {
	case metrics.Granularity1h:
		return metrics.Granularity5m.TableName(), "merge-1h"
	case metrics.Granularity1d:
		return metrics.Granularity1h.TableName(), "merge-1d"
	case metrics.Granularity1mo:
		return metrics.Granularity1d.TableName(), "merge-1mo"
	default:
		return "", ""
	}
}

// QueryRollupAware extends QueryRollup to handle the merge-watermark
// boundary. Sealed buckets (already aggregated into the chosen
// granularity) come from the coarse table; the in-flight portion past
// the watermark falls through to the finer table directly.
//
// Without this split, a 7-day query for /analytics/cost?groupBy=model
// goes to metric_rollup_1h and returns the last bucket merged 5–10
// minutes ago — operators see "where's the request I just sent?" until
// the merge tick fires. The watermark-aware path returns up-to-the-
// minute data from metric_rollup_5m for the trailing window while
// keeping the bulk of the range on the coarse table for query cost.
//
// The returned rows mix granularities at the boundary: caller's
// BuildResult sums all rows by (DimensionKey, MetricName) for group-by
// queries (so granularity does not matter), and emits per-bucket rows
// for time-series queries (where the trailing finer-grained buckets
// add detail rather than gaps — strictly more information than the
// pre-aware behaviour).
func (store *Store) QueryRollupAware(ctx context.Context, q metrics.MetricsQuery) ([]metrics.RollupRow, error) {
	gran := metrics.SelectGranularity(q.StartTime, q.EndTime)
	coarseTable := gran.TableName()
	tailTable, watermarkJob := rollupTailFor(gran)

	if tailTable == "" {
		// 5m granularity (or unknown) — no finer table to fall through to.
		return store.queryRollupOnTable(ctx, q, coarseTable, q.StartTime, q.EndTime)
	}

	wm, err := store.GetWatermark(ctx, watermarkJob)
	if err != nil || wm.IsZero() {
		// Watermark missing or unreadable — fall back to coarse-only so
		// analytics never returns an error just because the rollup
		// pipeline has not initialised yet.
		return store.queryRollupOnTable(ctx, q, coarseTable, q.StartTime, q.EndTime)
	}

	// Clamp watermark into [start, end] so the split window is always
	// valid even when the watermark sits outside the requested range.
	boundary := wm
	if boundary.Before(q.StartTime) {
		boundary = q.StartTime
	}
	if boundary.After(q.EndTime) {
		boundary = q.EndTime
	}

	sealedRows, err := store.queryRollupOnTable(ctx, q, coarseTable, q.StartTime, boundary)
	if err != nil {
		return nil, err
	}
	tailRows, err := store.queryRollupOnTable(ctx, q, tailTable, boundary, q.EndTime)
	if err != nil {
		return nil, err
	}
	if len(tailRows) == 0 {
		return sealedRows, nil
	}
	if len(sealedRows) == 0 {
		return tailRows, nil
	}
	return append(sealedRows, tailRows...), nil
}

// QueryRollupCascade — full 1mo→1d→1h→5m cascade for aggregate queries

// QueryRollupCascade reads aggregate rollup data for [q.StartTime, q.EndTime)
// using a full four-level cascade: metric_rollup_1mo → metric_rollup_1d →
// metric_rollup_1h → metric_rollup_5m. Each level covers the window between
// consecutive merge watermarks, so the union is always complete up to the
// latest sealed 5-minute bucket — regardless of how long each coarse table
// has been accumulating data.
//
// Use this for totals and group-by queries where bucket granularity is
// irrelevant (BuildResult sums all rows by MetricName / DimensionKey). For
// time-series charts at a fixed granularity (daily bars, hourly sparklines),
// continue using QueryRollupOnTable or QueryRollupAware.
func (store *Store) QueryRollupCascade(ctx context.Context, q metrics.MetricsQuery) ([]metrics.RollupRow, error) {
	// clamp clamps t into [q.StartTime, q.EndTime].
	clamp := func(t time.Time) time.Time {
		if t.Before(q.StartTime) {
			return q.StartTime
		}
		if t.After(q.EndTime) {
			return q.EndTime
		}
		return t
	}

	// wmBoundary returns the clamped merge watermark for jobName, or
	// q.StartTime when the watermark is missing (table has no data yet),
	// which collapses that segment to an empty [x,x) window.
	wmBoundary := func(jobName string) time.Time {
		wm, err := store.GetWatermark(ctx, jobName)
		if err != nil || wm.IsZero() {
			return q.StartTime
		}
		return clamp(wm)
	}

	b1mo := wmBoundary("merge-1mo")
	b1d := wmBoundary("merge-1d")
	b1h := wmBoundary("merge-1h")

	// Enforce monotonic ordering: each boundary must be >= the coarser one
	// (1d merge can never be ahead of 1mo merge, etc.).
	if b1d.Before(b1mo) {
		b1d = b1mo
	}
	if b1h.Before(b1d) {
		b1h = b1d
	}

	type segment struct {
		table string
		from  time.Time
		to    time.Time
	}
	segments := []segment{
		{metrics.Granularity1mo.TableName(), q.StartTime, b1mo},
		{metrics.Granularity1d.TableName(), b1mo, b1d},
		{metrics.Granularity1h.TableName(), b1d, b1h},
		{metrics.Granularity5m.TableName(), b1h, q.EndTime},
	}

	var allRows []metrics.RollupRow
	for _, seg := range segments {
		if !seg.to.After(seg.from) {
			continue // empty window — skip
		}
		rows, err := store.queryRollupOnTable(ctx, q, seg.table, seg.from, seg.to)
		if err != nil {
			return nil, err
		}
		allRows = append(allRows, rows...)
	}
	return allRows, nil
}

// QueryRollupMergeSource — read rows from a source table in a time range

// QueryRollupMergeSource reads all rollup rows from sourceTable where
// bucketStart is in [from, to). Used by merge jobs to aggregate finer-grained
// data into coarser tables.
func (store *Store) QueryRollupMergeSource(ctx context.Context, sourceTable string, from, to time.Time) ([]metrics.RollupRow, error) {
	sql := fmt.Sprintf(`
		SELECT "id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"
		FROM "%s"
		WHERE "bucketStart" >= $1 AND "bucketStart" < $2
		ORDER BY "bucketStart" ASC
	`, sourceTable)

	rows, err := store.pool.Query(ctx, sql, from, to)
	if err != nil {
		return nil, fmt.Errorf("query rollup merge source %s: %w", sourceTable, err)
	}
	return scanRollupRows(rows)
}

// PurgeRollupBefore — delete old rows

// PurgeRollupBefore deletes all rows in the given table with bucketStart
// before the cutoff timestamp. Returns the number of rows deleted.
func (store *Store) PurgeRollupBefore(ctx context.Context, table string, cutoff time.Time) (int64, error) {
	q := fmt.Sprintf(`DELETE FROM "%s" WHERE "bucketStart" < $1`, table)
	tag, err := store.pool.Exec(ctx, q, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge rollup %s before %v: %w", table, cutoff, err)
	}
	return tag.RowsAffected(), nil
}

// RollupHasData — existence check

// RollupHasData returns true if at least one row exists in the given table
// with bucketStart in [start, end).
func (store *Store) RollupHasData(ctx context.Context, table string, start, end time.Time) (bool, error) {
	var exists bool
	q := fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM "%s" WHERE "bucketStart" >= $1 AND "bucketStart" < $2)`,
		table,
	)
	err := store.pool.QueryRow(ctx, q, start, end).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("rollup has data %s: %w", table, err)
	}
	return exists, nil
}

// scanRollupRows — shared row scanner

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
