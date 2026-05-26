package drift

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

const (
	exemptionGCJobID          = "exemption-gc"
	exemptionGCJobName        = "Active Exemption Garbage Collection"
	exemptionGCJobDescription = "Fires a Cat B invalidate signal when compliance exemption grants have recently expired so compliance-proxy refreshes its in-memory view without waiting for the next admin mutation."
)

// exemptionGCQuerier is the narrow surface this job uses to ask the
// compliance_exemption_grant table whether any active grant expired
// since the previous tick. Pgx's QueryRow signature is enough — no
// other DB access is needed.
type exemptionGCQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// exemptionGCUpdater is the narrow surface this job uses from
// manager.Manager to fire a Cat B invalidate signal at the
// compliance-proxy fleet. We reuse UpdateConfig (with State=nil) rather
// than introducing a parallel API; the Hub manager already treats
// State=nil as the Cat B invalidate convention.
type exemptionGCUpdater interface {
	UpdateConfig(ctx context.Context, req manager.UpdateConfigRequest) (*manager.UpdateConfigResponse, error)
}

// ExemptionGCJob nudges compliance-proxy to re-read its exemption set
// when at least one grant has expired since the last tick. Under the
// PR-11 Cat B architecture compliance-proxy is the source of truth for
// the in-memory snapshot (it reads compliance_exemption_grant directly
// on Hub-pushed invalidate signals); this job collapses the previous
// 5-minute "trim-and-push" projection into a stateless tickle so a
// recently-expired grant becomes invisible to the proxy within one tick
// rather than at the next admin mutation.
type ExemptionGCJob struct {
	db       exemptionGCQuerier
	updater  exemptionGCUpdater
	interval time.Duration
	logger   *slog.Logger
}

// NewExemptionGC constructs the job. interval defaults to 5m when zero/negative.
func NewExemptionGC(db exemptionGCQuerier, updater exemptionGCUpdater, interval time.Duration, logger *slog.Logger) *ExemptionGCJob {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &ExemptionGCJob{
		db:       db,
		updater:  updater,
		interval: interval,
		logger:   logger.With("job", exemptionGCJobID),
	}
}

func (j *ExemptionGCJob) ID() string              { return exemptionGCJobID }
func (j *ExemptionGCJob) Name() string            { return exemptionGCJobName }
func (j *ExemptionGCJob) Description() string     { return exemptionGCJobDescription }
func (j *ExemptionGCJob) Interval() time.Duration { return j.interval }

// recentlyExpiredQuery counts grants whose expires_at fell into the
// previous tick window (since now - interval) and which were active at
// the time of expiry (inactive=false). The `now - interval` lower bound
// keeps the count bounded under steady-state operation; the strict
// upper bound `expires_at <= now` ensures we only signal AFTER expiry,
// not at effective-from boundary crossings (those flow through admin
// mutations).
const recentlyExpiredQuery = `
	SELECT COUNT(*)
	FROM compliance_exemption_grant
	WHERE NOT inactive
	  AND expires_at <= $1
	  AND expires_at > $2
`

// Run counts recently-expired grants and fires a Cat B invalidate if
// any were found. A zero count is a clean no-op — no broadcast, no log
// noise — so the steady-state cost is one COUNT(*) every interval.
func (j *ExemptionGCJob) Run(ctx context.Context) error {
	now := time.Now().UTC()
	windowStart := now.Add(-j.interval)
	var count int64
	if err := j.db.QueryRow(ctx, recentlyExpiredQuery, now, windowStart).Scan(&count); err != nil {
		return fmt.Errorf("count recently expired grants: %w", err)
	}
	if count == 0 {
		return nil
	}

	if _, err := j.updater.UpdateConfig(ctx, manager.UpdateConfigRequest{
		ThingType: "compliance-proxy",
		ConfigKey: configkey.Exemptions,
		State:     nil, // Cat B invalidate signal — compliance-proxy reads DB itself.
		Action:    "gc",
		ActorID:   "system:exemption-gc",
		ActorName: "exemption-gc",
	}); err != nil {
		return fmt.Errorf("invalidate exemptions: %w", err)
	}

	j.logger.Info("invalidated compliance-proxy.exemptions after expiry",
		"recentlyExpired", count,
		"windowSeconds", int(j.interval.Seconds()),
	)
	return nil
}
