package thingstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// ThingMetricsQuery describes a per-Thing rollup read. ThingID is mandatory
// (handler enforces; an empty value returns ErrThingMetricsQueryNoThingID
// rather than scanning every Thing's rows). The remaining fields mirror
// metrics.MetricsQuery so existing analytics helpers (BuildResult,
// MetricsResult shaping) can be reused unchanged at the handler layer.
type ThingMetricsQuery struct {
	ThingID      string
	Metrics      []string
	DimensionKey string
	SubDimension string
	StartTime    time.Time
	EndTime      time.Time
}

// ErrThingMetricsQueryNoThingID protects callers from accidentally issuing an
// unbounded scan across every Thing's rollup history.
var ErrThingMetricsQueryNoThingID = errors.New("thing rollup query requires ThingID")

// QueryThingRollup auto-selects the per-Thing rollup table based on the query
// time range (5m / 1h / 1d / 1mo) and returns matching rows for a single
// Thing. Unlike QueryRollupAware on the fleet side, this does NOT split at the
// merge watermark — per-Thing dashboards tolerate the few-minute lag of the
// coarse table, and the split logic doubles code surface. If sub-bucket
// freshness matters, callers can pass a shorter window to force 5m granule.
func (store *Store) QueryThingRollup(ctx context.Context, q ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
	if q.ThingID == "" {
		return nil, ErrThingMetricsQueryNoThingID
	}
	if !q.EndTime.After(q.StartTime) {
		return nil, nil
	}

	gran := metrics.SelectGranularity(q.StartTime, q.EndTime)
	table := "thing_" + gran.TableName()

	where := `"thing_id" = $1 AND "bucketStart" >= $2 AND "bucketStart" < $3`
	args := []any{q.ThingID, q.StartTime, q.EndTime}
	argIdx := 4

	if len(q.Metrics) > 0 {
		ph := make([]string, len(q.Metrics))
		for i, m := range q.Metrics {
			ph[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, m)
			argIdx++
		}
		where += ` AND "metricName" IN (` + strings.Join(ph, ", ") + `)`
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

	rows, err := store.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query thing rollup: %w", err)
	}
	return scanThingRollupRows(rows)
}

// ThingRollupHasAnyRecent returns true if the given Thing has at least one
// rollup row in [start, end). Used by the admin stats handler to distinguish
// "rollup disabled for this Thing type" from "no traffic yet": when an agent
// Thing has no rows but the toggle is ON, callers can show a neutral "no
// traffic" message instead of the disabled banner.
func (store *Store) ThingRollupHasAnyRecent(ctx context.Context, thingID string, start, end time.Time) (bool, error) {
	if thingID == "" {
		return false, ErrThingMetricsQueryNoThingID
	}
	gran := metrics.SelectGranularity(start, end)
	table := "thing_" + gran.TableName()
	var exists bool
	q := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM "%s" WHERE "thing_id" = $1 AND "bucketStart" >= $2 AND "bucketStart" < $3)`, table)
	if err := store.pool.QueryRow(ctx, q, thingID, start, end).Scan(&exists); err != nil {
		return false, fmt.Errorf("thing rollup has any: %w", err)
	}
	return exists, nil
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
