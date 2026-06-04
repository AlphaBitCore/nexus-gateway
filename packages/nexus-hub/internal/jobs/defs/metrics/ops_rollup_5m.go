package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/opsmetrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/rollup"
)

const (
	opsRollup5mJobID          = "ops-rollup-5m"
	opsRollup5mJobName        = "Ops Metrics Rollup (5 minute)"
	opsRollup5mJobDescription = "Aggregates metric_ops_raw into metric_ops_rollup_5m every minute. Services keep per-instance rows; agents keep per-instance rows only when in diagnostic mode for the bucket and otherwise collapse into a single fleet-aggregate row (thing_id IS NULL) per metric+dimension. The 1h/1d/1mo tiers cascade from this 5m layer."

	opsWatermarkName = "ops_5m"
	opsBucketDur     = 5 * time.Minute
	// opsSealedGrace defers processing of a 5-minute bucket until at least 1
	// minute after it ends so straggler samples (clock skew, batch flush
	// latency) land in the rollup before it seals.
	opsSealedGrace = time.Minute

	opsRollupTable = "metric_ops_rollup_5m"
	opsRawTable    = "metric_ops_raw"
)

// OpsRollup5mJob aggregates metric_ops_raw rows into metric_ops_rollup_5m.
// Per spec §8.4, the partitioning rule is:
//
//   - thing_type = 'service'      → always per-instance (thing_id preserved)
//   - thing_type = 'agent', diag  → per-instance for that hour
//   - thing_type = 'agent', plain → folded into one fleet row per metric+dim
//     with thing_id IS NULL
//
// Each bucket is written in a single transaction that DELETEs the prior rows
// and advances the watermark, so re-runs are idempotent and crash recovery
// produces the same final state.
type OpsRollup5mJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// per-bucket aggregation transaction is testable via pgxmock.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
}

// NewOpsRollup5m constructs the job. interval defaults to 1 minute (the
// scheduler's natural rollup tick — short enough that fresh data appears in
// the UI within a couple minutes of the 5-minute bucket sealing).
func NewOpsRollup5m(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *OpsRollup5mJob {
	if interval <= 0 {
		interval = time.Minute
	}
	return &OpsRollup5mJob{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", opsRollup5mJobID),
	}
}

func (j *OpsRollup5mJob) ID() string              { return opsRollup5mJobID }
func (j *OpsRollup5mJob) Name() string            { return opsRollup5mJobName }
func (j *OpsRollup5mJob) Description() string     { return opsRollup5mJobDescription }
func (j *OpsRollup5mJob) Interval() time.Duration { return j.interval }

// RunOnStart returns true so the job processes any pending buckets right
// after Hub boot rather than waiting up to one full interval.
func (j *OpsRollup5mJob) RunOnStart() bool { return true }

func (j *OpsRollup5mJob) Run(ctx context.Context) error {
	cursor, ok, err := j.resolveCursor(ctx)
	if err != nil {
		return err
	}
	if !ok {
		// No raw data to process — no-op.
		j.logger.Debug("no raw data yet — skipping run")
		return nil
	}

	latestSealed := time.Now().UTC().Add(-opsSealedGrace).Truncate(opsBucketDur)
	if !cursor.Before(latestSealed) {
		return nil
	}

	var count int
	for bucket := cursor; !bucket.After(latestSealed.Add(-opsBucketDur)); bucket = bucket.Add(opsBucketDur) {
		if err := j.processOneBucket(ctx, bucket); err != nil {
			return fmt.Errorf("bucket %s: %w", bucket.Format(time.RFC3339), err)
		}
		count++
	}

	if count > 0 {
		j.logger.Info("ops rollup completed", "buckets", count)
	}
	return nil
}

// resolveCursor decides where to start iterating. The natural cursor is the
// hour right after the watermark, but if metric_ops_raw holds data older than
// that (carry-forward bootstrap case), we rewind to the hour containing
// MIN(sampled_at) so historical samples are not skipped.
//
// Returns (cursor, true, nil) when there is data to consider, or
// (zero, false, nil) when metric_ops_raw is empty (no-op).
func (j *OpsRollup5mJob) resolveCursor(ctx context.Context) (time.Time, bool, error) {
	watermark, err := rollupstore.GetWatermark(ctx, j.pool, opsWatermarkName)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("get watermark: %w", err)
	}

	oldest, haveOldest, err := j.minSampledAt(ctx)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("min sampled_at: %w", err)
	}
	if !haveOldest {
		return time.Time{}, false, nil
	}

	// Bootstrap: rewind to the hour containing the oldest sample so the
	// first iteration covers it. The loop processes [cursor, cursor+1h),
	// hence we truncate (not subtract another hour) — unlike the rollup_5m
	// pattern which uses watermark + bucketDuration to compute the first
	// processed bucket. Here cursor already IS the first processed bucket.
	bootstrap := oldest.UTC().Truncate(opsBucketDur)

	// Cold start, or a watermark seeded into the future (the d86ca4f6 NOW()
	// seed): the watermark is not a real "last committed bucket" marker, so
	// advancing from it would strand the historical raw that predates the
	// seed. Rewind to the hour containing the oldest surviving sample.
	if watermark.IsZero() || watermark.UTC().After(time.Now().UTC()) {
		return bootstrap, true, nil
	}

	// Steady state: resume at the hour AFTER the last committed bucket. Never
	// rewind below the watermark — every hour <= watermark is already rolled
	// up. Re-scanning them on every tick is the ops-rollup death-loop: with
	// multi-day metric_ops_raw retention (~7d ≈ 168 hours), the old condition
	// (bootstrap.Before(advanceFromWatermark), always true while any older raw
	// survives) restarted the catch-up from MIN(sampled_at) every run, burning
	// the whole run budget re-aggregating completed hours before ever reaching
	// the first new bucket — so the watermark never advanced and the bucket
	// after it timed out forever. If retention has purged raw past the
	// watermark, skip the empty gap forward to the oldest surviving sample.
	advanceFromWatermark := watermark.UTC().Truncate(opsBucketDur).Add(opsBucketDur)
	if advanceFromWatermark.Before(bootstrap) {
		return bootstrap, true, nil
	}
	return advanceFromWatermark, true, nil
}

// minSampledAt returns the smallest sampled_at across metric_ops_raw, with a
// boolean flag (false → table is empty).
func (j *OpsRollup5mJob) minSampledAt(ctx context.Context) (time.Time, bool, error) {
	var earliest *time.Time
	if err := j.pool.QueryRow(ctx, `SELECT MIN(sampled_at) FROM `+opsRawTable).Scan(&earliest); err != nil {
		return time.Time{}, false, err
	}
	if earliest == nil {
		return time.Time{}, false, nil
	}
	return *earliest, true, nil
}

// processOneBucket aggregates one hour of metric_ops_raw into
// metric_ops_rollup_5m within a single transaction that also advances the
// watermark.
func (j *OpsRollup5mJob) processOneBucket(ctx context.Context, bucket time.Time) error {
	bucketEnd := bucket.Add(opsBucketDur)

	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := j.deleteBucket(ctx, tx, bucket); err != nil {
		return err
	}

	diagAgents, err := j.diagAgentsInWindow(ctx, tx, bucket, bucketEnd)
	if err != nil {
		return err
	}

	if err := j.insertScalarPerThing(ctx, tx, bucket, bucketEnd, diagAgents); err != nil {
		return err
	}
	if err := j.insertScalarFleet(ctx, tx, bucket, bucketEnd, diagAgents); err != nil {
		return err
	}
	if err := j.insertHistograms(ctx, tx, bucket, bucketEnd, diagAgents); err != nil {
		return err
	}

	if err := rollupstore.SetWatermark(ctx, tx, opsWatermarkName, bucket); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// deleteBucket clears any rollup rows already present for `bucket` so the
// re-insert below is idempotent.
func (j *OpsRollup5mJob) deleteBucket(ctx context.Context, tx pgx.Tx, bucket time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM `+opsRollupTable+` WHERE bucket_start = $1`, bucket); err != nil {
		return fmt.Errorf("delete rollup bucket: %w", err)
	}
	return nil
}

// diagAgentsInWindow returns the set of agent thing_ids whose diagnostic-mode
// window overlaps [bucket, bucketEnd).
func (j *OpsRollup5mJob) diagAgentsInWindow(ctx context.Context, tx pgx.Tx, bucket, bucketEnd time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT thing_id
		  FROM thing_diag_mode_window
		 WHERE ended_at > $1 AND started_at < $2
	`, bucket, bucketEnd)
	if err != nil {
		return nil, fmt.Errorf("query diag agents: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan diag agent: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate diag agents: %w", err)
	}
	if ids == nil {
		ids = []string{} // pgx requires a non-nil slice for ANY($1::text[]).
	}
	return ids, nil
}

// insertScalarPerThing aggregates non-histogram rows into metric_ops_rollup_5m
// per (thing_id, metric, dimension) for services and diag-mode agents.
//
// The writer side puts the canonical Thing.type into metric_ops_raw.thing_type
// (`ai-gateway`, `compliance-proxy`, `nexus-hub`, `control-plane`, or `agent`)
// — not a generic `'service'` bucket. So "server-side Things" is anything
// whose thing_type is not `agent`. Filtering on `thing_type = 'service'`
// matched zero rows in prod and left every Stats / Metrics tab empty.
func (j *OpsRollup5mJob) insertScalarPerThing(ctx context.Context, tx pgx.Tx, bucket, bucketEnd time.Time, diagAgents []string) error {
	const q = `
		INSERT INTO metric_ops_rollup_5m
			(id, bucket_start, thing_id, thing_type, metric_name, metric_kind, dimension_key,
			 value_avg, value_sum, value_min, value_max, sample_count, metadata)
		SELECT gen_random_uuid(),
		       $1 AS bucket_start,
		       thing_id, thing_type, metric_name, metric_kind, dimension_key,
		       AVG(value), SUM(value), MIN(value), MAX(value), COUNT(*), NULL
		  FROM ` + opsRawTable + `
		 WHERE sampled_at >= $1 AND sampled_at < $2
		   AND metric_kind <> 'histogram'
		   AND (
		         thing_type <> 'agent'
		         OR (thing_type = 'agent' AND thing_id = ANY($3::text[]))
		       )
		 GROUP BY thing_id, thing_type, metric_name, metric_kind, dimension_key
	`
	if _, err := tx.Exec(ctx, q, bucket, bucketEnd, diagAgents); err != nil {
		return fmt.Errorf("insert per-thing scalars: %w", err)
	}
	return nil
}

// insertScalarFleet aggregates non-histogram agent rows whose thing_id is
// outside the diag-mode set into one fleet row per metric+dim with thing_id
// NULL.
func (j *OpsRollup5mJob) insertScalarFleet(ctx context.Context, tx pgx.Tx, bucket, bucketEnd time.Time, diagAgents []string) error {
	const q = `
		INSERT INTO metric_ops_rollup_5m
			(id, bucket_start, thing_id, thing_type, metric_name, metric_kind, dimension_key,
			 value_avg, value_sum, value_min, value_max, sample_count, metadata)
		SELECT gen_random_uuid(),
		       $1 AS bucket_start,
		       NULL AS thing_id,
		       'agent' AS thing_type,
		       metric_name, metric_kind, dimension_key,
		       AVG(value), SUM(value), MIN(value), MAX(value), COUNT(*), NULL
		  FROM ` + opsRawTable + `
		 WHERE sampled_at >= $1 AND sampled_at < $2
		   AND metric_kind <> 'histogram'
		   AND thing_type = 'agent'
		   AND thing_id <> ALL($3::text[])
		 GROUP BY metric_name, metric_kind, dimension_key
	`
	if _, err := tx.Exec(ctx, q, bucket, bucketEnd, diagAgents); err != nil {
		return fmt.Errorf("insert fleet scalars: %w", err)
	}
	return nil
}

// histogramRawRow is one metric_ops_raw histogram sample, used to fold N
// samples into one rollup row in Go (SQL AVG/SUM are meaningless for the
// element-wise bucket array stored in metadata).
type histogramRawRow struct {
	thingID    string
	thingType  string
	metricName string
	dimKey     string
	metadata   []byte
}

// histogramAcc accumulates merged buckets + sample count for one rollup row.
type histogramAcc struct {
	thingID    *string // nil = fleet
	thingType  string
	metricName string
	dimKey     string
	buckets    opsmetrics.HistogramBuckets
	count      int64
}

// insertHistograms reads every histogram raw row in the bucket, folds them
// into per-thing-or-fleet buckets, and writes one rollup row per (thing|fleet,
// metric, dim).
func (j *OpsRollup5mJob) insertHistograms(ctx context.Context, tx pgx.Tx, bucket, bucketEnd time.Time, diagAgents []string) error {
	rows, err := tx.Query(ctx, `
		SELECT thing_id, thing_type, metric_name, dimension_key, metadata
		  FROM `+opsRawTable+`
		 WHERE sampled_at >= $1 AND sampled_at < $2
		   AND metric_kind = 'histogram'
	`, bucket, bucketEnd)
	if err != nil {
		return fmt.Errorf("query histogram raw: %w", err)
	}

	var raw []histogramRawRow
	for rows.Next() {
		var r histogramRawRow
		if err := rows.Scan(&r.thingID, &r.thingType, &r.metricName, &r.dimKey, &r.metadata); err != nil {
			rows.Close()
			return fmt.Errorf("scan histogram raw: %w", err)
		}
		raw = append(raw, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate histogram raw: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}

	diagSet := make(map[string]struct{}, len(diagAgents))
	for _, id := range diagAgents {
		diagSet[id] = struct{}{}
	}

	// Composite key for accumulator map. thingID may be nil for fleet rows.
	type accKey struct {
		thingID    string // empty string sentinel for fleet
		isFleet    bool
		thingType  string
		metricName string
		dimKey     string
	}

	acc := make(map[accKey]*histogramAcc, len(raw))
	for _, r := range raw {
		buckets, perr := opsmetrics.ParseHistogramBuckets(r.metadata)
		if perr != nil {
			j.logger.Warn("skip unparseable histogram",
				"thing_id", r.thingID, "metric", r.metricName, "error", perr)
			continue
		}

		var key accKey
		var entry *histogramAcc

		// Branch on agent vs server-side. Raw writer uses the canonical
		// Thing.type — `ai-gateway` / `compliance-proxy` / `nexus-hub` /
		// `control-plane` / `agent`. Anything non-agent goes to the
		// per-thing rollup row (no fleet aggregation for server Things).
		switch r.thingType {
		case "agent":
			if _, inDiag := diagSet[r.thingID]; inDiag {
				key = accKey{thingID: r.thingID, thingType: "agent", metricName: r.metricName, dimKey: r.dimKey}
				if e, ok := acc[key]; ok {
					entry = e
				} else {
					id := r.thingID
					entry = &histogramAcc{thingID: &id, thingType: "agent", metricName: r.metricName, dimKey: r.dimKey}
					acc[key] = entry
				}
			} else {
				key = accKey{isFleet: true, thingType: "agent", metricName: r.metricName, dimKey: r.dimKey}
				if e, ok := acc[key]; ok {
					entry = e
				} else {
					entry = &histogramAcc{thingID: nil, thingType: "agent", metricName: r.metricName, dimKey: r.dimKey}
					acc[key] = entry
				}
			}
		case "":
			// Empty thing_type is a data integrity bug — skip silently.
			continue
		default:
			// Server-side Thing (ai-gateway / compliance-proxy / nexus-hub
			// / control-plane). Carry the specific type forward so the
			// Stats tab can group correctly.
			key = accKey{thingID: r.thingID, thingType: r.thingType, metricName: r.metricName, dimKey: r.dimKey}
			if e, ok := acc[key]; ok {
				entry = e
			} else {
				id := r.thingID
				entry = &histogramAcc{thingID: &id, thingType: r.thingType, metricName: r.metricName, dimKey: r.dimKey}
				acc[key] = entry
			}
		}

		entry.buckets = opsmetrics.MergeHistogramBuckets(entry.buckets, buckets)
		entry.count++
	}

	for _, e := range acc {
		metaBytes, merr := opsmetrics.EncodeHistogramBuckets(e.buckets)
		if merr != nil {
			return fmt.Errorf("encode merged histogram: %w", merr)
		}
		sum := float64(opsmetrics.SumHistogramBuckets(e.buckets))

		_, err := tx.Exec(ctx, `
			INSERT INTO metric_ops_rollup_5m
				(id, bucket_start, thing_id, thing_type, metric_name, metric_kind, dimension_key,
				 value_avg, value_sum, value_min, value_max, sample_count, metadata)
			VALUES ($1, $2, $3, $4, $5, 'histogram', $6, NULL, $7, NULL, NULL, $8, $9::jsonb)
		`,
			uuid.NewString(), bucket, e.thingID, e.thingType, e.metricName, e.dimKey,
			sum, e.count, string(metaBytes),
		)
		if err != nil {
			return fmt.Errorf("insert histogram rollup row: %w", err)
		}
	}
	return nil
}
