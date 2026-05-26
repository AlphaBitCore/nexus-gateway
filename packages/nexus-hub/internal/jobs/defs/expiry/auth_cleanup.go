package expiry

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	authCleanupJobID          = "auth-cleanup"
	authCleanupJobName        = "Auth Token Cleanup"
	authCleanupJobDescription = "Deletes expired refresh and revoked token rows every hour."
)

// authCleanupStore is the subset of *store.Store this job needs.
// Using an interface keeps the job testable without a live DB.
type authCleanupStore interface {
	DeleteExpiredRevokedTokens(ctx context.Context) (int64, error)
	DeleteExpiredRefreshTokens(ctx context.Context) (int64, error)
}

// AuthCleanupJob deletes expired rows from RevokedToken and RefreshToken
// every hour. The scheduler's PG advisory lock prevents duplicate runs across
// Hub replicas.
type AuthCleanupJob struct {
	store    authCleanupStore
	interval time.Duration
	logger   *slog.Logger
}

// NewAuthCleanup constructs the job. interval is forced to one hour when zero
// or negative so callers can safely pass time.Hour without a guard.
func NewAuthCleanup(st authCleanupStore, interval time.Duration, logger *slog.Logger) *AuthCleanupJob {
	if interval <= 0 {
		interval = time.Hour
	}
	return &AuthCleanupJob{
		store:    st,
		interval: interval,
		logger:   logger.With("job", authCleanupJobID),
	}
}

func (j *AuthCleanupJob) ID() string              { return authCleanupJobID }
func (j *AuthCleanupJob) Name() string            { return authCleanupJobName }
func (j *AuthCleanupJob) Description() string     { return authCleanupJobDescription }
func (j *AuthCleanupJob) Interval() time.Duration { return j.interval }

// Run deletes expired rows from both token tables. Both DELETEs run regardless
// of individual failures so a partial outage in one table never starves the other.
func (j *AuthCleanupJob) Run(ctx context.Context) error {
	revokedDeleted, revokedErr := j.store.DeleteExpiredRevokedTokens(ctx)
	refreshDeleted, refreshErr := j.store.DeleteExpiredRefreshTokens(ctx)

	if revokedDeleted > 0 || refreshDeleted > 0 {
		j.logger.Info("auth cleanup",
			slog.Int64("revoked_deleted", revokedDeleted),
			slog.Int64("refresh_deleted", refreshDeleted),
		)
	}

	return errors.Join(revokedErr, refreshErr)
}
