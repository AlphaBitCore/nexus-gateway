package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
)


const (
	opsRetentionJobID          = "ops-retention"
	opsRetentionJobName        = "Ops Metrics & Diag Retention Purge"
	opsRetentionJobDescription = "Purges aged metric_ops_raw, metric_ops_rollup_1h/1d/1mo, and thing_diag_event rows. Per-class retention is read live from metric_ops_retention_config (one row per layer: runtime_raw, business_raw, runtime_1h, business_1h, runtime_1d, business_1d, runtime_1mo, business_1mo, diag_info, diag_warn, diag_error, diag_fatal). Deletes run in chunks of 10k rows so transactions stay short."
)

// opsRetentionDeleteLimit caps each DELETE to 10k rows so individual
// transactions are short and never block the live writers for long. The job
// loops per layer until the layer reports zero deletions in one iteration.
const opsRetentionDeleteLimit = 10000

// OpsRetentionJob purges aged ops-metric raw + rollup rows and diag events
// based on per-class retention rows in metric_ops_retention_config. Each
// layer is processed independently — failure in one never prevents the others.
type OpsRetentionJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// layer config SELECT + per-layer DELETE loops can be driven by
	// pgxmock without touching real metric_ops_* tables.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
}

// NewOpsRetention constructs the job. interval defaults to 24h.
func NewOpsRetention(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *OpsRetentionJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &OpsRetentionJob{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", opsRetentionJobID),
	}
}

func (j *OpsRetentionJob) ID() string              { return opsRetentionJobID }
func (j *OpsRetentionJob) Name() string            { return opsRetentionJobName }
func (j *OpsRetentionJob) Description() string     { return opsRetentionJobDescription }
func (j *OpsRetentionJob) Interval() time.Duration { return j.interval }

// opsRetentionLayer is one row from metric_ops_retention_config materialized
// as the SQL the layer requires.
type opsRetentionLayer struct {
	layer         string
	retentionDays int
}

// Run reads every retention-config row and applies it to the matching layer.
func (j *OpsRetentionJob) Run(ctx context.Context) error {
	layers, err := j.loadLayers(ctx)
	if err != nil {
		return fmt.Errorf("load retention config: %w", err)
	}

	now := time.Now().UTC()
	var total int64
	var errs []error

	for _, l := range layers {
		// retention_days <= 0 disables the layer (operator override). Never
		// delete history when the policy is "keep forever".
		if l.retentionDays <= 0 {
			continue
		}
		cutoff := now.AddDate(0, 0, -l.retentionDays)
		purged, perr := j.purgeLayer(ctx, l.layer, cutoff)
		if perr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", l.layer, perr))
			continue
		}
		if purged > 0 {
			j.logger.Info("purged ops layer",
				slog.String("layer", l.layer),
				slog.Int64("rows", purged),
				slog.String("cutoff", cutoff.Format(time.RFC3339)),
			)
		}
		total += purged
	}

	if total > 0 {
		j.logger.Info("ops-retention completed", "totalPurged", total)
	}
	return errors.Join(errs...)
}

// loadLayers reads every active retention row.
func (j *OpsRetentionJob) loadLayers(ctx context.Context) ([]opsRetentionLayer, error) {
	rows, err := j.pool.Query(ctx, `SELECT layer, retention_days FROM metric_ops_retention_config`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []opsRetentionLayer
	for rows.Next() {
		var l opsRetentionLayer
		if err := rows.Scan(&l.layer, &l.retentionDays); err != nil {
			return nil, fmt.Errorf("scan retention row: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// purgeLayer routes one retention row to the appropriate DELETE strategy. The
// layer name encodes both the class (runtime/business/diag-{level}) and the
// table tier (raw/1h/1d/1mo).
func (j *OpsRetentionJob) purgeLayer(ctx context.Context, layer string, cutoff time.Time) (int64, error) {
	switch layer {
	case "runtime_raw":
		return j.deleteLooping(ctx, `
			DELETE FROM metric_ops_raw
			 WHERE id IN (
			   SELECT id FROM metric_ops_raw
			    WHERE sampled_at < $1 AND metric_name LIKE 'runtime.%'
			    LIMIT $2
			 )
		`, cutoff)
	case "business_raw":
		return j.deleteLooping(ctx, `
			DELETE FROM metric_ops_raw
			 WHERE id IN (
			   SELECT id FROM metric_ops_raw
			    WHERE sampled_at < $1 AND metric_name NOT LIKE 'runtime.%'
			    LIMIT $2
			 )
		`, cutoff)

	case "runtime_1h":
		return j.deleteRollup(ctx, "metric_ops_rollup_1h", "runtime", cutoff)
	case "business_1h":
		return j.deleteRollup(ctx, "metric_ops_rollup_1h", "business", cutoff)
	case "runtime_1d":
		return j.deleteRollup(ctx, "metric_ops_rollup_1d", "runtime", cutoff)
	case "business_1d":
		return j.deleteRollup(ctx, "metric_ops_rollup_1d", "business", cutoff)
	case "runtime_1mo":
		return j.deleteRollup(ctx, "metric_ops_rollup_1mo", "runtime", cutoff)
	case "business_1mo":
		return j.deleteRollup(ctx, "metric_ops_rollup_1mo", "business", cutoff)

	case "diag_warn":
		return j.deleteDiag(ctx, "warn", cutoff)
	case "diag_error":
		return j.deleteDiag(ctx, "error", cutoff)
	case "diag_fatal":
		return j.deleteDiag(ctx, "fatal", cutoff)
	// diag_info covers lifecycle + any other info-level diag events.
	// Added when agent lifecycle events (agent.startup / shutdown /
	// paused / resumed / sso_login) started flowing through the
	// diag pipeline (#63) — without this layer those rows
	// accumulated forever in thing_diag_event. Lifecycle events are
	// the higher-cardinality of the two info-level shapes, but the
	// volume is still tiny (~10 rows / day / device) so a single
	// retention layer is enough.
	case "diag_info":
		return j.deleteDiag(ctx, "info", cutoff)

	default:
		j.logger.Warn("unknown retention layer — ignoring", "layer", layer)
		return 0, nil
	}
}

// deleteRollup loops chunked deletes against one rollup table for one class.
func (j *OpsRetentionJob) deleteRollup(ctx context.Context, table, class string, cutoff time.Time) (int64, error) {
	var likeOp string
	if class == "runtime" {
		likeOp = "LIKE 'runtime.%%'"
	} else {
		likeOp = "NOT LIKE 'runtime.%%'"
	}
	q := fmt.Sprintf(`
		DELETE FROM %s
		 WHERE id IN (
		   SELECT id FROM %s
		    WHERE bucket_start < $1 AND metric_name %s
		    LIMIT $2
		 )
	`, table, table, likeOp)
	return j.deleteLooping(ctx, q, cutoff)
}

// deleteDiag loops chunked deletes against thing_diag_event for one level.
func (j *OpsRetentionJob) deleteDiag(ctx context.Context, level string, cutoff time.Time) (int64, error) {
	q := `
		DELETE FROM thing_diag_event
		 WHERE id IN (
		   SELECT id FROM thing_diag_event
		    WHERE occurred_at < $1 AND level = $2
		    LIMIT $3
		 )
	`
	var total int64
	for {
		tag, err := j.pool.Exec(ctx, q, cutoff, level, opsRetentionDeleteLimit)
		if err != nil {
			return total, err
		}
		n := tag.RowsAffected()
		total += n
		if n < opsRetentionDeleteLimit {
			return total, nil
		}
	}
}

// deleteLooping runs the chunked-DELETE template until one iteration deletes
// fewer than the limit (i.e. the table is drained for that filter).
//
// q must contain two positional placeholders ($1 = cutoff, $2 = limit).
func (j *OpsRetentionJob) deleteLooping(ctx context.Context, q string, cutoff time.Time) (int64, error) {
	var total int64
	for {
		tag, err := j.pool.Exec(ctx, q, cutoff, opsRetentionDeleteLimit)
		if err != nil {
			return total, err
		}
		n := tag.RowsAffected()
		total += n
		if n < opsRetentionDeleteLimit {
			return total, nil
		}
	}
}
