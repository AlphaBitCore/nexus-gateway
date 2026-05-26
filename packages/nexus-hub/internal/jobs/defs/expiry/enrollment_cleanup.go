package expiry

import (
	"context"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

const (
	enrollmentCleanupJobID          = "enrollment-cleanup"
	enrollmentCleanupJobName        = "Enrollment Token Cleanup"
	enrollmentCleanupJobDescription = "Marks expired pending agent enrollment tokens as expired."
)

// enrollmentTokenStore is the subset of *store.Store this job needs.
// Defined as an interface so the test suite can inject a fake without
// touching real Postgres.
type enrollmentTokenStore interface {
	CleanupExpiredEnrollmentTokens(ctx context.Context) (int64, error)
}

// EnrollmentTokenCleanup marks expired pending enrollment_token rows as expired.
type EnrollmentTokenCleanup struct {
	store    enrollmentTokenStore
	interval time.Duration
	logger   *slog.Logger
}

// NewEnrollmentTokenCleanup registers the periodic cleanup job (same name as the former CP scheduler job).
func NewEnrollmentTokenCleanup(st *store.Store, interval time.Duration, logger *slog.Logger) *EnrollmentTokenCleanup {
	return &EnrollmentTokenCleanup{
		store:    st.EnrollStore(),
		interval: interval,
		logger:   logger.With("job", enrollmentCleanupJobID),
	}
}

func (j *EnrollmentTokenCleanup) ID() string              { return enrollmentCleanupJobID }
func (j *EnrollmentTokenCleanup) Name() string            { return enrollmentCleanupJobName }
func (j *EnrollmentTokenCleanup) Description() string     { return enrollmentCleanupJobDescription }
func (j *EnrollmentTokenCleanup) Interval() time.Duration { return j.interval }

func (j *EnrollmentTokenCleanup) Run(ctx context.Context) error {
	n, err := j.store.CleanupExpiredEnrollmentTokens(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		j.logger.Info("enrollment token cleanup", "expired", n)
	}
	return nil
}
