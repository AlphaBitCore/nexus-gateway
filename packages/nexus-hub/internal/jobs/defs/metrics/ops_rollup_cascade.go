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

// opsCascadeMode selects between fixed-duration (1d) and calendar-month (1mo)
// bucket boundaries.
type opsCascadeMode int

const (
	opsCascadeFixed opsCascadeMode = iota
	opsCascadeCalendarMonth
)

// opsCascadeConfig describes one cascade layer (1h→1d or 1d→1mo). The field
// values are static per layer; the runtime behavior (bootstrap, idempotent
// re-write, weighted-avg merge, calendar-month bucketing) is shared.
type opsCascadeConfig struct {
	id            string
	name          string
	description   string
	sourceTable   string
	targetTable   string
	watermarkName string
	// sourceBucketDur is the source-layer bucket length (1h or 1d). Used to
	// rewind the cursor by one source bucket on bootstrap so MIN(bucket_start)
	// in the source becomes the first iteration's bucket.
	sourceBucketDur time.Duration
	// targetBucketDur is the target-layer bucket length. Zero = calendar mode.
	targetBucketDur time.Duration
	mode            opsCascadeMode
	// sealedGrace is the headroom past the bucket boundary before we consider a
	// target bucket sealed. For 1d this guards against late 1h rollups landing
	// across midnight; for 1mo we always exclude the current month so this is
	// only used in fixed mode.
	sealedGrace time.Duration
}

// OpsRollupCascadeJob aggregates the source ops rollup tier into the target
// tier. The source tier already partitions per-thing vs fleet (thing_id NULL),
// so we just GROUP BY (bucket_start, thing_id, thing_type, metric, kind, dim);
// scalars use sample-count-weighted averaging, histograms merge element-wise.
//
// Idempotency: each target bucket is written in one transaction that DELETEs
// the prior rows for that bucket and advances the watermark, so re-runs and
// crash recovery converge to the same final state.
type OpsRollupCascadeJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// per-bucket cascade merge transaction is testable via pgxmock.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
	cfg      opsCascadeConfig
}

// NewOpsRollup1h constructs the 5m→1h cascade job. interval defaults to 5m.
// The raw→5m aggregation (per-thing/fleet collapse, diag-mode window,
// histogram folding) is owned by ops-rollup-5m; this job just merges sealed
// 5-minute buckets into 1-hour buckets, so 1h/1d/1mo all share one code path.
func NewOpsRollup1h(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *OpsRollupCascadeJob {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	cfg := opsCascadeConfig{
		id:              "ops-rollup-1h",
		name:            "Ops Metrics Rollup (1 hour)",
		description:     "Aggregates metric_ops_rollup_5m into metric_ops_rollup_1h every 5 minutes. Sample-count-weighted averages; histograms merge element-wise. The fleet vs per-thing distinction (thing_id NULL vs not) is preserved from the 5m layer.",
		sourceTable:     "metric_ops_rollup_5m",
		targetTable:     "metric_ops_rollup_1h",
		watermarkName:   "ops_1h",
		sourceBucketDur: 5 * time.Minute,
		targetBucketDur: time.Hour,
		mode:            opsCascadeFixed,
		// One full source bucket (5m) of headroom — late 5m rollups from
		// straggler raw samples must land before the hour seals.
		sealedGrace: 5 * time.Minute,
	}
	return &OpsRollupCascadeJob{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", cfg.id),
		cfg:      cfg,
	}
}

// NewOpsRollup1d constructs the 1h→1d cascade job. interval defaults to 1h.
func NewOpsRollup1d(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *OpsRollupCascadeJob {
	if interval <= 0 {
		interval = time.Hour
	}
	cfg := opsCascadeConfig{
		id:              "ops-rollup-1d",
		name:            "Ops Metrics Rollup (1 day)",
		description:     "Aggregates metric_ops_rollup_1h into metric_ops_rollup_1d every hour. Sample-count-weighted averages; histograms merge element-wise. The fleet vs per-thing distinction (thing_id NULL vs not) is preserved from the 1h layer.",
		sourceTable:     "metric_ops_rollup_1h",
		targetTable:     "metric_ops_rollup_1d",
		watermarkName:   "ops_1d",
		sourceBucketDur: time.Hour,
		targetBucketDur: 24 * time.Hour,
		mode:            opsCascadeFixed,
		// One full source bucket of headroom — late-arriving 1h aggregations
		// from straggler raw samples need to land before the day is sealed.
		sealedGrace: time.Hour,
	}
	return &OpsRollupCascadeJob{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", cfg.id),
		cfg:      cfg,
	}
}

// NewOpsRollup1mo constructs the 1d→1mo cascade job with calendar-month
// buckets (variable length: 28-31 days). interval defaults to 24h.
func NewOpsRollup1mo(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *OpsRollupCascadeJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	cfg := opsCascadeConfig{
		id:              "ops-rollup-1mo",
		name:            "Ops Metrics Rollup (1 month)",
		description:     "Aggregates metric_ops_rollup_1d into metric_ops_rollup_1mo once per day, using calendar-month bucket boundaries (28-31 days). The current (unsealed) month is always excluded.",
		sourceTable:     "metric_ops_rollup_1d",
		targetTable:     "metric_ops_rollup_1mo",
		watermarkName:   "ops_1mo",
		sourceBucketDur: 24 * time.Hour,
		targetBucketDur: 0, // calendar mode
		mode:            opsCascadeCalendarMonth,
	}
	return &OpsRollupCascadeJob{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", cfg.id),
		cfg:      cfg,
	}
}

func (j *OpsRollupCascadeJob) ID() string              { return j.cfg.id }
func (j *OpsRollupCascadeJob) Name() string            { return j.cfg.name }
func (j *OpsRollupCascadeJob) Description() string     { return j.cfg.description }
func (j *OpsRollupCascadeJob) Interval() time.Duration { return j.interval }

// RunOnStart returns true so the job processes any pending buckets right after
// Hub boot rather than waiting up to one full interval.
func (j *OpsRollupCascadeJob) RunOnStart() bool { return true }

func (j *OpsRollupCascadeJob) Run(ctx context.Context) error {
	if j.cfg.mode == opsCascadeCalendarMonth {
		return j.runCalendarMonth(ctx)
	}
	return j.runFixed(ctx)
}

// runFixed processes fixed-duration target buckets (e.g. 1d).
func (j *OpsRollupCascadeJob) runFixed(ctx context.Context) error {
	cursor, ok, err := j.resolveFixedCursor(ctx)
	if err != nil {
		return err
	}
	if !ok {
		j.logger.Debug("no source data yet — skipping run")
		return nil
	}

	latestSealed := time.Now().UTC().Add(-j.cfg.sealedGrace).Truncate(j.cfg.targetBucketDur)
	if !cursor.Before(latestSealed) {
		return nil
	}

	var count int
	for bucket := cursor; !bucket.After(latestSealed.Add(-j.cfg.targetBucketDur)); bucket = bucket.Add(j.cfg.targetBucketDur) {
		if err := j.processOneBucket(ctx, bucket, bucket.Add(j.cfg.targetBucketDur)); err != nil {
			return fmt.Errorf("bucket %s: %w", bucket.Format(time.RFC3339), err)
		}
		count++
	}

	if count > 0 {
		j.logger.Info("ops cascade rollup completed", "buckets", count)
	}
	return nil
}

// runCalendarMonth processes calendar-month target buckets (1mo). The current
// (unsealed) month is always excluded.
func (j *OpsRollupCascadeJob) runCalendarMonth(ctx context.Context) error {
	cursor, ok, err := j.resolveCalendarCursor(ctx)
	if err != nil {
		return err
	}
	if !ok {
		j.logger.Debug("no source data yet — skipping run")
		return nil
	}

	now := time.Now().UTC()
	currentMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var count int
	for monthStart := cursor; monthStart.Before(currentMonthStart); monthStart = nextOpsMonth(monthStart) {
		monthEnd := nextOpsMonth(monthStart)
		if err := j.processOneBucket(ctx, monthStart, monthEnd); err != nil {
			return fmt.Errorf("bucket %s: %w", monthStart.Format("2006-01"), err)
		}
		count++
	}

	if count > 0 {
		j.logger.Info("ops cascade rollup completed", "buckets", count)
	}
	return nil
}

// resolveFixedCursor picks the first target bucket to process. If the source
// table holds rows older than the watermark+1 (the carry-forward bootstrap
// case), we rewind to the target bucket containing the oldest source row so
// historical data is not skipped.
func (j *OpsRollupCascadeJob) resolveFixedCursor(ctx context.Context) (time.Time, bool, error) {
	watermark, err := rollupstore.GetWatermark(ctx, j.pool, j.cfg.watermarkName)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("get watermark: %w", err)
	}

	oldest, haveOldest, err := j.minSourceBucket(ctx)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("min source bucket: %w", err)
	}
	if !haveOldest {
		return time.Time{}, false, nil
	}

	// Bootstrap: align the oldest source bucket to the target boundary; that
	// is the first iteration's bucket (matching ops_rollup_1h's pattern where
	// `cursor` is the first processed bucket, not its predecessor).
	bootstrap := oldest.UTC().Truncate(j.cfg.targetBucketDur)

	// Cold start, or a watermark seeded into the future → bootstrap to the
	// oldest source bucket. Otherwise advance from the watermark and never
	// rewind below it: re-merging already-completed target buckets on every run
	// (the source layer retains 90+ days) is the same wasted full-history
	// re-scan that wedged ops_rollup_1h. If the source has been purged past the
	// watermark, skip the empty gap forward to the oldest surviving bucket.
	if watermark.IsZero() || watermark.UTC().After(time.Now().UTC()) {
		return bootstrap, true, nil
	}
	advanceFromWatermark := watermark.UTC().Truncate(j.cfg.targetBucketDur).Add(j.cfg.targetBucketDur)
	if advanceFromWatermark.Before(bootstrap) {
		return bootstrap, true, nil
	}
	return advanceFromWatermark, true, nil
}

// resolveCalendarCursor picks the first month to process. Calendar-month
// rewinding aligns the oldest source row to the first day of its month.
func (j *OpsRollupCascadeJob) resolveCalendarCursor(ctx context.Context) (time.Time, bool, error) {
	watermark, err := rollupstore.GetWatermark(ctx, j.pool, j.cfg.watermarkName)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("get watermark: %w", err)
	}

	oldest, haveOldest, err := j.minSourceBucket(ctx)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("min source bucket: %w", err)
	}
	if !haveOldest {
		return time.Time{}, false, nil
	}

	bootstrap := firstOfMonth(oldest)

	// Same rule as resolveFixedCursor: bootstrap only on cold start or a
	// future-seeded watermark; otherwise advance to the month after the
	// watermark and never rewind below it.
	if watermark.IsZero() || watermark.UTC().After(time.Now().UTC()) {
		return bootstrap, true, nil
	}
	advanceFromWatermark := nextOpsMonth(watermark)
	if advanceFromWatermark.Before(bootstrap) {
		return bootstrap, true, nil
	}
	return advanceFromWatermark, true, nil
}

// minSourceBucket returns the smallest bucket_start in the source rollup table.
func (j *OpsRollupCascadeJob) minSourceBucket(ctx context.Context) (time.Time, bool, error) {
	var earliest *time.Time
	q := fmt.Sprintf(`SELECT MIN(bucket_start) FROM %s`, j.cfg.sourceTable)
	if err := j.pool.QueryRow(ctx, q).Scan(&earliest); err != nil {
		return time.Time{}, false, err
	}
	if earliest == nil {
		return time.Time{}, false, nil
	}
	return *earliest, true, nil
}

// processOneBucket aggregates one target-layer bucket inside a single
// transaction that also clears any prior rows for that bucket and advances
// the watermark, so re-runs are idempotent.
func (j *OpsRollupCascadeJob) processOneBucket(ctx context.Context, bucketStart, bucketEnd time.Time) error {
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE bucket_start = $1`, j.cfg.targetTable), bucketStart); err != nil {
		return fmt.Errorf("delete target bucket: %w", err)
	}

	if err := j.insertScalars(ctx, tx, bucketStart, bucketEnd); err != nil {
		return err
	}
	if err := j.insertHistograms(ctx, tx, bucketStart, bucketEnd); err != nil {
		return err
	}

	if err := rollupstore.SetWatermark(ctx, tx, j.cfg.watermarkName, bucketStart); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// insertScalars merges all non-histogram source rows into target rows. The
// per-thing vs fleet distinction is preserved from the source layer (the
// source already split the rows when it ran), so we just GROUP BY thing_id
// (NULL-safe via COALESCE) along with the other identity columns.
//
// AVG-of-AVG is sample-count-weighted:
//
//	new_avg = SUM(value_avg * sample_count) / SUM(sample_count)
//
// Doing it inside SQL keeps the merge atomic and avoids loading every source
// row into Go for fleet aggregates that may include thousands of agents.
func (j *OpsRollupCascadeJob) insertScalars(ctx context.Context, tx pgx.Tx, bucketStart, bucketEnd time.Time) error {
	q := fmt.Sprintf(`
		INSERT INTO %s
			(id, bucket_start, thing_id, thing_type, metric_name, metric_kind, dimension_key,
			 value_avg, value_sum, value_min, value_max, sample_count, metadata)
		SELECT gen_random_uuid(),
		       $1 AS bucket_start,
		       thing_id, thing_type, metric_name, metric_kind, dimension_key,
		       CASE WHEN SUM(sample_count) > 0
		            THEN SUM(COALESCE(value_avg, 0) * sample_count) / SUM(sample_count)
		            ELSE NULL END AS value_avg,
		       SUM(value_sum)                                AS value_sum,
		       MIN(value_min)                                AS value_min,
		       MAX(value_max)                                AS value_max,
		       SUM(sample_count)                             AS sample_count,
		       NULL                                          AS metadata
		  FROM %s
		 WHERE bucket_start >= $1 AND bucket_start < $2
		   AND metric_kind <> 'histogram'
		 GROUP BY thing_id, thing_type, metric_name, metric_kind, dimension_key
	`, j.cfg.targetTable, j.cfg.sourceTable)
	if _, err := tx.Exec(ctx, q, bucketStart, bucketEnd); err != nil {
		return fmt.Errorf("insert scalars: %w", err)
	}
	return nil
}

// histogramSourceRow is one source histogram row, used to fold N source rows
// into one target row in Go (SQL element-wise array merge is awkward).
type opsCascadeHistRow struct {
	thingID    *string
	thingType  string
	metricName string
	dimKey     string
	metadata   []byte
	count      int64
}

// histogramAcc holds merged buckets + summed sample count for one target row.
type opsCascadeHistAcc struct {
	thingID    *string // nil = fleet
	thingType  string
	metricName string
	dimKey     string
	buckets    opsmetrics.HistogramBuckets
	count      int64
}

// insertHistograms reads every histogram row from the source for this bucket
// and folds them into one target row per (thing|fleet, metric, dim).
func (j *OpsRollupCascadeJob) insertHistograms(ctx context.Context, tx pgx.Tx, bucketStart, bucketEnd time.Time) error {
	q := fmt.Sprintf(`
		SELECT thing_id, thing_type, metric_name, dimension_key, metadata, sample_count
		  FROM %s
		 WHERE bucket_start >= $1 AND bucket_start < $2
		   AND metric_kind = 'histogram'
	`, j.cfg.sourceTable)
	rows, err := tx.Query(ctx, q, bucketStart, bucketEnd)
	if err != nil {
		return fmt.Errorf("query histogram source: %w", err)
	}

	var raw []opsCascadeHistRow
	for rows.Next() {
		var r opsCascadeHistRow
		if err := rows.Scan(&r.thingID, &r.thingType, &r.metricName, &r.dimKey, &r.metadata, &r.count); err != nil {
			rows.Close()
			return fmt.Errorf("scan histogram source: %w", err)
		}
		raw = append(raw, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate histogram source: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}

	type accKey struct {
		thingID    string
		isFleet    bool
		thingType  string
		metricName string
		dimKey     string
	}

	acc := make(map[accKey]*opsCascadeHistAcc, len(raw))
	for _, r := range raw {
		buckets, perr := opsmetrics.ParseHistogramBuckets(r.metadata)
		if perr != nil {
			j.logger.Warn("skip unparseable histogram",
				"metric", r.metricName, "error", perr)
			continue
		}

		var key accKey
		if r.thingID == nil {
			key = accKey{isFleet: true, thingType: r.thingType, metricName: r.metricName, dimKey: r.dimKey}
		} else {
			key = accKey{thingID: *r.thingID, thingType: r.thingType, metricName: r.metricName, dimKey: r.dimKey}
		}

		entry, ok := acc[key]
		if !ok {
			entry = &opsCascadeHistAcc{
				thingID:    r.thingID,
				thingType:  r.thingType,
				metricName: r.metricName,
				dimKey:     r.dimKey,
			}
			acc[key] = entry
		}
		entry.buckets = opsmetrics.MergeHistogramBuckets(entry.buckets, buckets)
		entry.count += r.count
	}

	for _, e := range acc {
		metaBytes, merr := opsmetrics.EncodeHistogramBuckets(e.buckets)
		if merr != nil {
			return fmt.Errorf("encode merged histogram: %w", merr)
		}
		sum := float64(opsmetrics.SumHistogramBuckets(e.buckets))

		insertQ := fmt.Sprintf(`
			INSERT INTO %s
				(id, bucket_start, thing_id, thing_type, metric_name, metric_kind, dimension_key,
				 value_avg, value_sum, value_min, value_max, sample_count, metadata)
			VALUES ($1, $2, $3, $4, $5, 'histogram', $6, NULL, $7, NULL, NULL, $8, $9::jsonb)
		`, j.cfg.targetTable)

		if _, err := tx.Exec(ctx, insertQ,
			uuid.NewString(), bucketStart, e.thingID, e.thingType, e.metricName, e.dimKey,
			sum, e.count, string(metaBytes),
		); err != nil {
			return fmt.Errorf("insert histogram target row: %w", err)
		}
	}
	return nil
}

// firstOfMonth returns the first day of the calendar month containing t (UTC).
func firstOfMonth(t time.Time) time.Time {
	y, m, _ := t.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// nextOpsMonth returns the first day of the calendar month following t. It
// mirrors the existing nextMonth helper used by rollup_merge.go but lives in
// this file to avoid coupling the ops cascade to the AI traffic merge layer.
func nextOpsMonth(t time.Time) time.Time {
	y, m, _ := t.UTC().Date()
	return time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
}
