package drift

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	staleThingJobID          = "stale-thing-sweep"
	staleThingJobName        = "Stale Thing Sweep"
	staleThingJobDescription = "Marks Things offline when their last_seen_at exceeds the per-category threshold."
)

// StaleThingConfig holds the per-category offline thresholds.
type StaleThingConfig struct {
	// ServiceThreshold applies to control-plane / ai-gateway / compliance-proxy Things.
	// Defaults to 30s when zero or negative.
	ServiceThreshold time.Duration
	// AgentThreshold applies to desktop agent Things. Defaults to 5m.
	AgentThreshold time.Duration
}

// staleStore is the subset of *store.Store this job needs.
type staleStore interface {
	MarkStaleOffline(ctx context.Context, types []string, threshold time.Duration) (int64, error)
}

// StaleThingJob marks Things offline when their last_seen_at exceeds the
// threshold for their type category. Runs on Hub because Hub owns the thing
// table; the scheduler's PG advisory lock prevents duplicate work across replicas.
type StaleThingJob struct {
	store    staleStore
	interval time.Duration
	logger   *slog.Logger
	cfg      StaleThingConfig
}

// NewStaleThingJob constructs the job. interval controls how often Run is invoked.
// If cfg thresholds are zero or negative, sensible defaults are applied.
func NewStaleThingJob(s staleStore, interval time.Duration, logger *slog.Logger, cfg StaleThingConfig) *StaleThingJob {
	if cfg.ServiceThreshold <= 0 {
		cfg.ServiceThreshold = 30 * time.Second
	}
	if cfg.AgentThreshold <= 0 {
		cfg.AgentThreshold = 5 * time.Minute
	}
	return &StaleThingJob{
		store:    s,
		interval: interval,
		logger:   logger.With("job", staleThingJobID),
		cfg:      cfg,
	}
}

func (j *StaleThingJob) ID() string              { return staleThingJobID }
func (j *StaleThingJob) Name() string            { return staleThingJobName }
func (j *StaleThingJob) Description() string     { return staleThingJobDescription }
func (j *StaleThingJob) Interval() time.Duration { return j.interval }

// Run marks stale agents and services offline in two separate updates so
// different thresholds apply per category.
func (j *StaleThingJob) Run(ctx context.Context) error {
	agentCount, err := j.store.MarkStaleOffline(ctx, []string{"agent"}, j.cfg.AgentThreshold)
	if err != nil {
		return fmt.Errorf("mark agents offline: %w", err)
	}
	svcCount, err := j.store.MarkStaleOffline(ctx, []string{"control-plane", "ai-gateway", "compliance-proxy"}, j.cfg.ServiceThreshold)
	if err != nil {
		return fmt.Errorf("mark services offline: %w", err)
	}
	if agentCount > 0 || svcCount > 0 {
		j.logger.Info("stale-thing sweep",
			slog.Int64("agents_marked", agentCount),
			slog.Int64("services_marked", svcCount),
		)
	}
	return nil
}
