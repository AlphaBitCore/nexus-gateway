package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/store"
)

const (
	jobRetentionJobID          = "job-retention"
	jobRetentionJobName        = "Job Run History Retention"
	jobRetentionJobDescription = "Prunes job_run rows, keeping the N most recent runs per job."
)

// jobRetentionStore is the subset of *jobstore.Store this job needs. Defined
// as an interface so the test suite can inject a fake or pgxmock-backed store
// without spinning up a real jobstore.Store + Postgres.
type jobRetentionStore interface {
	PruneJobRuns(ctx context.Context, keepN int) (int64, error)
}

// JobRetention deletes older rows from `job_run` so the table stays bounded.
// The retention policy is per-job: each job keeps `keepPerJob` most recent
// runs regardless of total volume, which matches the admin UI's paginated
// run-history view (newest first, bounded page count).
type JobRetention struct {
	store      jobRetentionStore
	interval   time.Duration
	keepPerJob int
	logger     *slog.Logger
}

// NewJobRetention creates the retention job. keepPerJob must be positive; it
// defaults to 100 when zero or negative. interval defaults to 24h.
func NewJobRetention(store *jobstore.Store, interval time.Duration, keepPerJob int, logger *slog.Logger) *JobRetention {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if keepPerJob <= 0 {
		keepPerJob = 100
	}
	return &JobRetention{
		store:      store,
		interval:   interval,
		keepPerJob: keepPerJob,
		logger:     logger.With("job", jobRetentionJobID),
	}
}

func (j *JobRetention) ID() string              { return jobRetentionJobID }
func (j *JobRetention) Name() string            { return jobRetentionJobName }
func (j *JobRetention) Description() string     { return jobRetentionJobDescription }
func (j *JobRetention) Interval() time.Duration { return j.interval }

// RunOnStart fires the retention pass once on startup so a fresh replica
// does not wait a full interval before the first prune.
func (j *JobRetention) RunOnStart() bool { return true }

func (j *JobRetention) Run(ctx context.Context) error {
	n, err := j.store.PruneJobRuns(ctx, j.keepPerJob)
	if err != nil {
		return fmt.Errorf("prune job runs: %w", err)
	}
	if n > 0 {
		j.logger.Info("job-retention prune", "rows_deleted", n, "keep_per_job", j.keepPerJob)
	}
	return nil
}
