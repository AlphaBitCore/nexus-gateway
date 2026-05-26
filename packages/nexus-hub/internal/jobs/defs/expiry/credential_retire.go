package expiry

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
)


const (
	credRetireJobID   = "credential-retire"
	credRetireJobName = "Credential Retire"
	credRetireJobDesc = "Advances credentials from retiring → retired (after drain window) and hard-deletes retired credentials past their retireAt date."

	// credRetireDrainWindow is how long a credential must remain in the
	// 'retiring' state before the job auto-advances it to 'retired'.
	// The window lets in-flight requests drain before the key is deleted.
	credRetireDrainWindow = 24 * time.Hour

	// credRetireDeleteDelay is how long after a credential enters 'retired'
	// state before the job hard-deletes it. Provides an audit window.
	credRetireDeleteDelay = 7 * 24 * time.Hour
)

// CredentialRetireJob advances the credential lifecycle for pool rotation:
//
//   - Credentials in 'retiring' state for longer than credRetireDrainWindow
//     are advanced to 'retired' with retireAt = now + credRetireDeleteDelay.
//   - Credentials in 'retired' state past their retireAt are hard-deleted.
//
// This is safe because retiring credentials are excluded from pool selection
// (the AI Gateway filters status != 'active'), so no live traffic uses them
// after status transitions away from 'active'.
type CredentialRetireJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the test
	// suite can inject pgxmock without sharing real Credential rows.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
}

// NewCredentialRetire constructs the job. interval defaults to 1 hour.
func NewCredentialRetire(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *CredentialRetireJob {
	if interval <= 0 {
		interval = time.Hour
	}
	return &CredentialRetireJob{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", credRetireJobID),
	}
}

func (j *CredentialRetireJob) ID() string              { return credRetireJobID }
func (j *CredentialRetireJob) Name() string            { return credRetireJobName }
func (j *CredentialRetireJob) Description() string     { return credRetireJobDesc }
func (j *CredentialRetireJob) Interval() time.Duration { return j.interval }

func (j *CredentialRetireJob) Run(ctx context.Context) error {
	now := time.Now().UTC()
	drainCutoff := now.Add(-credRetireDrainWindow)
	retireAt := now.Add(credRetireDeleteDelay)

	// Step 1: advance retiring → retired for credentials past the drain window.
	retiredTag, err := j.pool.Exec(ctx, `
		UPDATE "Credential"
		SET status = 'retired',
		    "retireAt" = $1,
		    "updatedAt" = NOW()
		WHERE status = 'retiring'
		  AND "updatedAt" <= $2
	`, retireAt, drainCutoff)
	if err != nil {
		j.logger.Error("credential-retire: advance retiring→retired failed", "error", err)
		return err
	}
	if retiredTag.RowsAffected() > 0 {
		j.logger.Info("credential-retire: advanced retiring→retired",
			"count", retiredTag.RowsAffected(),
			"retireAt", retireAt.Format(time.RFC3339))
	}

	// Step 2: hard-delete retired credentials past their retireAt.
	deletedTag, err := j.pool.Exec(ctx, `
		DELETE FROM "Credential"
		WHERE status = 'retired'
		  AND "retireAt" IS NOT NULL
		  AND "retireAt" <= $1
	`, now)
	if err != nil {
		j.logger.Error("credential-retire: delete expired retired credentials failed", "error", err)
		return err
	}
	if deletedTag.RowsAffected() > 0 {
		j.logger.Info("credential-retire: deleted expired retired credentials",
			"count", deletedTag.RowsAffected())
	}

	return nil
}
