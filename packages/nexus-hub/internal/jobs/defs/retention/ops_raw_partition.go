package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	opsRawPartitionJobID          = "ops-raw-partition"
	opsRawPartitionJobName        = "Ops Raw Partition Maintenance"
	opsRawPartitionJobDescription = "Maintains the daily RANGE partitions of metric_ops_raw: pre-creates upcoming day partitions and DROPs partitions older than the configured retention horizon (NEXUS_HUB_SCHEDULER_OPS_RAW_DAYS, default 30 days). Replaces the chunked row-by-row DELETE for raw ops-metric samples — dropping a whole-day partition is an O(1) metadata operation."

	// opsRawParent is the partitioned parent table.
	opsRawParent = "metric_ops_raw"
	// opsRawPartitionPrefix is the child-partition naming convention. The
	// YYYYMMDD suffix IS the partition's inclusive FROM date (one partition per
	// UTC day), so the drop logic can derive the boundary from the name without
	// parsing pg_get_expr(relpartbound).
	opsRawPartitionPrefix = "metric_ops_raw_p"
	// opsRawPartitionDateLayout formats the YYYYMMDD suffix.
	opsRawPartitionDateLayout = "20060102"
)

// opsRawAheadDays is how many days of future partitions to keep pre-created so
// inserts never hit a missing-partition error if the job is delayed. Covers
// today + a couple days of headroom; "yesterday" is included to self-heal if
// the job missed a tick across a day boundary.
var opsRawAheadOffsets = []int{-1, 0, 1, 2}

// OpsRawPartitionJob pre-creates upcoming daily partitions of metric_ops_raw
// and drops partitions whose whole day is older than the retention horizon.
type OpsRawPartitionJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// CREATE/DROP partition statements are testable via pgxmock.
	pool          defs.PgxPool
	interval      time.Duration
	retentionDays int
	logger        *slog.Logger
}

// NewOpsRawPartition constructs the job. interval defaults to 6h; retentionDays
// defaults to 30 when non-positive.
func NewOpsRawPartition(pool *pgxpool.Pool, interval time.Duration, retentionDays int, logger *slog.Logger) *OpsRawPartitionJob {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	if retentionDays <= 0 {
		retentionDays = 30
	}
	return &OpsRawPartitionJob{
		pool:          pool,
		interval:      interval,
		retentionDays: retentionDays,
		logger:        logger.With("job", opsRawPartitionJobID),
	}
}

func (j *OpsRawPartitionJob) ID() string              { return opsRawPartitionJobID }
func (j *OpsRawPartitionJob) Name() string            { return opsRawPartitionJobName }
func (j *OpsRawPartitionJob) Description() string     { return opsRawPartitionJobDescription }
func (j *OpsRawPartitionJob) Interval() time.Duration { return j.interval }

// RunOnStart returns true so the partition window is correct immediately after
// Hub boot rather than waiting up to one full interval (a missing current-day
// partition would reject every metric insert).
func (j *OpsRawPartitionJob) RunOnStart() bool { return true }

// Run ensures upcoming partitions exist, then drops aged ones. A failure to
// drop never blocks creation: creation is what keeps writes alive, so it runs
// first and its error short-circuits; drops are best-effort-joined.
func (j *OpsRawPartitionJob) Run(ctx context.Context) error {
	now := time.Now().UTC()
	if err := j.ensurePartitions(ctx, now); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}
	return j.dropAgedPartitions(ctx, now)
}

// partitionDay truncates t to the start of its UTC day.
func partitionDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ensurePartitions creates (idempotently) one daily partition for each offset
// in opsRawAheadOffsets relative to today.
func (j *OpsRawPartitionJob) ensurePartitions(ctx context.Context, now time.Time) error {
	today := partitionDay(now)
	for _, off := range opsRawAheadOffsets {
		from := today.AddDate(0, 0, off)
		to := from.AddDate(0, 0, 1)
		name := opsRawPartitionPrefix + from.Format(opsRawPartitionDateLayout)
		// FROM inclusive, TO exclusive. Explicit +00 so the bound is parsed as
		// UTC regardless of the server's timezone setting.
		stmt := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')`,
			name, opsRawParent,
			from.Format("2006-01-02"), to.Format("2006-01-02"),
		)
		if _, err := j.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create partition %s: %w", name, err)
		}
	}
	return nil
}

// dropAgedPartitions lists every child partition and drops those whose entire
// day is older than the retention cutoff. A partition pD covers [D, D+1); it is
// safe to drop once D+1 <= cutoff (no row in it is newer than the cutoff).
func (j *OpsRawPartitionJob) dropAgedPartitions(ctx context.Context, now time.Time) error {
	cutoff := partitionDay(now).AddDate(0, 0, -j.retentionDays)

	rows, err := j.pool.Query(ctx, `
		SELECT c.relname
		  FROM pg_inherits i
		  JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = $1
	`, opsRawParent)
	if err != nil {
		return fmt.Errorf("list partitions: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("scan partition name: %w", err)
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate partitions: %w", err)
	}

	var errs []error
	var dropped int
	for _, name := range names {
		from, ok := parseOpsRawPartitionDate(name)
		if !ok {
			// Not a date-suffixed partition we manage — leave it alone.
			continue
		}
		// upper bound = from + 1 day; drop only when the whole day predates cutoff.
		if !from.AddDate(0, 0, 1).After(cutoff) {
			if _, err := j.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
				errs = append(errs, fmt.Errorf("drop %s: %w", name, err))
				continue
			}
			dropped++
			j.logger.Info("dropped aged ops-raw partition",
				slog.String("partition", name),
				slog.String("cutoff", cutoff.Format("2006-01-02")),
			)
		}
	}
	if dropped > 0 {
		j.logger.Info("ops-raw-partition completed", "dropped", dropped, "retentionDays", j.retentionDays)
	}
	return errors.Join(errs...)
}

// parseOpsRawPartitionDate extracts the inclusive FROM date encoded in a child
// partition name (metric_ops_raw_pYYYYMMDD). Returns ok=false for any name that
// does not match the convention so unmanaged tables are never dropped.
func parseOpsRawPartitionDate(name string) (time.Time, bool) {
	suffix, ok := strings.CutPrefix(name, opsRawPartitionPrefix)
	if !ok || len(suffix) != len(opsRawPartitionDateLayout) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(opsRawPartitionDateLayout, suffix, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
