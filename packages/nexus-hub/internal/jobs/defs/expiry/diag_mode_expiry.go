package expiry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
)


const (
	diagModeExpiryJobID          = "diag-mode-expiry"
	diagModeExpiryJobName        = "Diagnostic Mode Expiry"
	diagModeExpiryJobDescription = "Clears the diagModeUntil flag from thing.metadata once the corresponding thing_diag_mode_window has ended. Runs every minute; mutations are atomic per thing (no batch transaction)."
)

// DiagModeExpiryJob clears stale shadow flags. The diag-mode lifecycle is:
//
//  1. Operator sets thing.metadata.diagModeUntil + opens a thing_diag_mode_window.
//  2. The agent observes the flag in shadow and emits per-instance metrics.
//  3. The window closes (ended_at <= NOW()).
//  4. This job clears the metadata flag so the agent reverts to fleet emission.
//
// We mutate one thing at a time (no cross-thing transaction) so a slow row
// never blocks the others — operator-set diag mode is small in scale (a
// handful of agents at a time, not the whole fleet).
type DiagModeExpiryJob struct {
	// pool is typed against the package-level defs.PgxPool seam so tests can
	// drive the Query + per-thing UPDATE chain via pgxmock without
	// sharing the real thing + thing_diag_mode_window tables.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
}

// NewDiagModeExpiry constructs the job. interval defaults to 1 minute.
func NewDiagModeExpiry(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *DiagModeExpiryJob {
	if interval <= 0 {
		interval = time.Minute
	}
	return &DiagModeExpiryJob{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", diagModeExpiryJobID),
	}
}

func (j *DiagModeExpiryJob) ID() string              { return diagModeExpiryJobID }
func (j *DiagModeExpiryJob) Name() string            { return diagModeExpiryJobName }
func (j *DiagModeExpiryJob) Description() string     { return diagModeExpiryJobDescription }
func (j *DiagModeExpiryJob) Interval() time.Duration { return j.interval }

// Run finds every thing whose latest diag-mode window has ended but whose
// metadata still carries diagModeUntil, and clears the flag.
func (j *DiagModeExpiryJob) Run(ctx context.Context) error {
	// Distinct thing_ids in case a thing has multiple expired windows; we only
	// need to clear the flag once per thing.
	rows, err := j.pool.Query(ctx, `
		SELECT DISTINCT t.id
		  FROM thing t
		  JOIN thing_diag_mode_window w ON w.thing_id = t.id
		 WHERE w.ended_at <= NOW()
		   AND t.metadata ? 'diagModeUntil'
	`)
	if err != nil {
		return fmt.Errorf("query expired diag windows: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan expired thing id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate expired things: %w", err)
	}

	if len(ids) == 0 {
		return nil
	}

	var cleared int
	for _, id := range ids {
		// metadata - 'diagModeUntil' returns the JSONB with the key removed.
		// updated_at is bumped so downstream shadow-pull consumers notice.
		tag, err := j.pool.Exec(ctx, `
			UPDATE thing
			   SET metadata = metadata - 'diagModeUntil',
			       updated_at = NOW()
			 WHERE id = $1
			   AND metadata ? 'diagModeUntil'
		`, id)
		if err != nil {
			j.logger.Warn("clear diagModeUntil failed",
				slog.String("thing_id", id),
				slog.Any("error", err),
			)
			continue
		}
		if tag.RowsAffected() > 0 {
			cleared++
		}
	}

	if cleared > 0 {
		j.logger.Info("diag-mode flags cleared",
			slog.Int("things", cleared),
		)
	}
	return nil
}
