// Package expiry — passthrough_expiry job.
//
// Auto-reverts emergency-passthrough rows whose expires_at has passed
// across the three tier tables (global / adapter / provider). Flips
// enabled=true → false on the expired rows so the next admin-API read
// reflects the disabled state and the audit trail records the
// auto-revert (the row's updated_at advances).
//
// Runtime safety: ai-gateway's passthrough.Snapshot.active() already
// filters tiers with expires_at < now() at lookup time — so the
// runtime kill-switch is structurally bounded by expires_at even
// before this job runs. This job is for AUDIT + admin-UI correctness,
// not for the live request path.
package expiry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/jackc/pgx/v5/pgxpool"
)

const passthroughExpiryJobID = "passthrough.expiry"

// PassthroughExpiryJob is the 60s tick that auto-reverts expired
// emergency-passthrough rows across the three config tables.
type PassthroughExpiryJob struct {
	// pool is typed against the package-level defs.PgxPool seam so tests can
	// drive the three UPDATEs without sharing real config tables.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
}

// NewPassthroughExpiryJob wires the job. interval is typically 60s
// (configurable for tests).
func NewPassthroughExpiryJob(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *PassthroughExpiryJob {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &PassthroughExpiryJob{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", passthroughExpiryJobID),
	}
}

func (j *PassthroughExpiryJob) ID() string   { return passthroughExpiryJobID }
func (j *PassthroughExpiryJob) Name() string { return "Passthrough Expiry Auto-Revert" }
func (j *PassthroughExpiryJob) Description() string {
	return "Flips enabled=false on emergency-passthrough rows whose expires_at has passed across global/adapter/provider tiers."
}
func (j *PassthroughExpiryJob) Interval() time.Duration { return j.interval }
func (j *PassthroughExpiryJob) RunOnStart() bool        { return true }

// Run scans the three tables. The CHECK constraints in the migration
// reject updates that would violate the invariants (expires_at NOT
// NULL when enabled), so the only well-formed state is the row's
// current state at insert time. Flipping enabled=true → false leaves
// expires_at unchanged (it's already in the past, which is fine when
// enabled=false).
func (j *PassthroughExpiryJob) Run(ctx context.Context) error {
	var (
		gCount, aCount, pCount int64
	)
	if rows, err := j.pool.Exec(ctx,
		`UPDATE gateway_passthrough_config_global
		    SET enabled = false, updated_at = NOW()
		  WHERE enabled = true AND expires_at <= NOW()`,
	); err == nil {
		gCount = rows.RowsAffected()
	} else {
		return fmt.Errorf("revert global tier: %w", err)
	}

	if rows, err := j.pool.Exec(ctx,
		`UPDATE gateway_passthrough_config_adapter
		    SET enabled = false, updated_at = NOW()
		  WHERE enabled = true AND expires_at <= NOW()`,
	); err == nil {
		aCount = rows.RowsAffected()
	} else {
		return fmt.Errorf("revert adapter tier: %w", err)
	}

	if rows, err := j.pool.Exec(ctx,
		`UPDATE gateway_passthrough_config_provider
		    SET enabled = false, updated_at = NOW()
		  WHERE enabled = true AND expires_at <= NOW()`,
	); err == nil {
		pCount = rows.RowsAffected()
	} else {
		return fmt.Errorf("revert provider tier: %w", err)
	}

	if gCount+aCount+pCount > 0 {
		j.logger.Info("auto-reverted expired passthrough rows",
			"event", "passthrough_expiry_revert",
			"global_reverted", gCount,
			"adapter_reverted", aCount,
			"provider_reverted", pCount,
		)
	}
	return nil
}
