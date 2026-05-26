package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/siem"
)

const (
	siemBridgeJobID          = "siem-bridge"
	siemBridgeJobName        = "SIEM Bridge"
	siemBridgeJobDescription = "Polls traffic_event and AdminAuditLog for new rows, classifies them, and forwards them to the configured SIEM sink. Checkpoints are persisted in system_metadata."
)

// SIEMBridgeJob wraps a siem.Bridge as a Hub scheduler Job.
type SIEMBridgeJob struct {
	bridge   *siem.Bridge
	interval time.Duration
	logger   *slog.Logger
}

// NewSIEMBridge constructs the job. The interval is taken from the bridge
// configuration (set from siem.config.pollIntervalSeconds); pass 0 to let
// the bridge default (30s) take effect.
func NewSIEMBridge(bridge *siem.Bridge, logger *slog.Logger) *SIEMBridgeJob {
	return &SIEMBridgeJob{
		bridge:   bridge,
		interval: bridge.PollInterval(),
		logger:   logger.With("job", siemBridgeJobID),
	}
}

func (j *SIEMBridgeJob) ID() string              { return siemBridgeJobID }
func (j *SIEMBridgeJob) Name() string            { return siemBridgeJobName }
func (j *SIEMBridgeJob) Description() string     { return siemBridgeJobDescription }
func (j *SIEMBridgeJob) Interval() time.Duration { return j.interval }

// Run delegates to the bridge's Poll method. Bridge.Poll already handles
// its own errors internally via logging and never panics — the scheduler
// just needs a nil return to mark the run successful.
func (j *SIEMBridgeJob) Run(ctx context.Context) error {
	j.bridge.Poll(ctx)
	return nil
}
