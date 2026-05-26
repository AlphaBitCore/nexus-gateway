package shadow

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
)

// OfflineFallback loads the last-known-good config from SQLite when the
// Dashboard Backend is unreachable. The grace period controls how long the
// agent operates on stale config before switching to fail-closed mode.
type OfflineFallback struct {
	queue       *auditqueue.Queue
	gracePeriod time.Duration
	logger      *slog.Logger
}

// NewOfflineFallback creates an offline fallback manager.
// gracePeriod is the maximum allowed staleness before fail-closed (default 7 days).
func NewOfflineFallback(queue *auditqueue.Queue, gracePeriod time.Duration, logger *slog.Logger) *OfflineFallback {
	if gracePeriod <= 0 {
		gracePeriod = 7 * 24 * time.Hour
	}
	return &OfflineFallback{
		queue:       queue,
		gracePeriod: gracePeriod,
		logger:      logger,
	}
}

// LoadCached loads the most recent config snapshot from SQLite.
// Returns nil if no snapshot exists.
func (o *OfflineFallback) LoadCached() (*ConfigSnapshot, error) {
	version, data, err := o.queue.LoadLatestConfigSnapshot()
	if err != nil {
		return nil, fmt.Errorf("load cached config: %w", err)
	}

	var snap ConfigSnapshot
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		return nil, fmt.Errorf("unmarshal cached config: %w", err)
	}
	snap.Version = version

	o.logger.Info("loaded cached config snapshot",
		slog.Int("version", version),
	)
	return &snap, nil
}

// SaveSnapshot persists a config snapshot to SQLite for offline fallback.
func (o *OfflineFallback) SaveSnapshot(snap *ConfigSnapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal config snapshot: %w", err)
	}
	return o.queue.SaveConfigSnapshot(snap.Version, string(data))
}

// IsStale returns true if the snapshot's age exceeds the grace period.
func (o *OfflineFallback) IsStale(snap *ConfigSnapshot) bool {
	if snap.FetchedAt.IsZero() {
		return true
	}
	return time.Since(snap.FetchedAt) > o.gracePeriod
}
