package wiring

import (
	"context"
	"log/slog"
	"time"

	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/bootstrap"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/protectionpause"
	config "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/schema"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/status"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/policies"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// StatusCollectorConfig groups the dependencies needed to build the
// status collector.
type StatusCollectorConfig struct {
	Version         string
	ThingID         string
	HubHTTPURL      string
	CpURL           string
	CertFile        string
	HeartbeatSec    int
	AuditQueue      *auditqueue.Queue
	ConfigMgr       *config.Manager
	EnrollMgr       *enrollment.Manager
	Pauser          *protectionpause.Pauser
	BootstrapClient *bootstrap.Client
	ThingClient     *thingclient.Client
	Logger          *slog.Logger
}

// InitStatusCollector builds the status collector from the provided config.
// DeviceAuthModeFn uses a 200 ms best-effort timeout so it never blocks
// GetStatus on a network call. The bootstrap client caches for 60s so most
// reads are in-process.
func InitStatusCollector(cfg StatusCollectorConfig) *status.Collector {
	var statusThingClient status.ThingStateAccessor
	if cfg.ThingClient != nil {
		statusThingClient = cfg.ThingClient
	}
	return status.NewCollector(status.CollectorConfig{
		Version:      cfg.Version,
		DeviceID:     cfg.ThingID,
		DashboardURL: cfg.HubHTTPURL,
		DownloadURL:  ComposeAgentDownloadURL(cfg.CpURL),
		CertExpiresAt: ReadCertExpiry(cfg.CertFile),
		HeartbeatSec: cfg.HeartbeatSec,
		UnsyncedCountFn: cfg.AuditQueue.UnsyncedCount,
		TodayStatsFn: buildTodayStatsFn(cfg.AuditQueue),
		ThingClient:  statusThingClient,
		TrustLevelFn: cfg.EnrollMgr.TrustLevel,
		DeviceAuthModeFn: buildDeviceAuthModeFn(cfg.BootstrapClient),
		SSOEmailFn:    cfg.EnrollMgr.SSOEmail,
		PausedFn:      cfg.Pauser.IsPaused,
		PausedUntilFn: cfg.Pauser.ResumesAt,
		QuitAllowedFn: func() bool { q := cfg.ConfigMgr.Get().QuitAllowed; return q == nil || *q },
	})
}

func buildTodayStatsFn(q *auditqueue.Queue) func() status.TodayStats {
	return func() status.TodayStats {
		ins, pass, deny, us, up := q.ComputeTodayStats()
		return status.TodayStats{
			Inspected:          ins,
			Passthrough:        pass,
			Denied:             deny,
			AvgUsOverheadMs:    us,
			AvgUpstreamTotalMs: up,
		}
	}
}

func buildDeviceAuthModeFn(bc *bootstrap.Client) func() string {
	return func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		info, err := bc.Get(ctx)
		if err != nil {
			return ""
		}
		return info.DeviceAuthMode
	}
}

// ConfigPullNoOpFn returns the no-op config-pull closure used in the
// agent (config comes via Hub shadow push, not direct HTTP pull from CP).
func ConfigPullNoOpFn() func() (bool, string, error) {
	return func() (bool, string, error) { return true, "", nil }
}

// WireSnapshotCacheToCollector wires the policies cache into the status
// collector so ConfigSummary.{InterceptionDomains,HooksEnabled,
// ActiveExemptions} reads the same source the Policies page does.
func WireSnapshotCacheToCollector(
	collector *status.Collector,
	cache *policies.SnapshotCache,
) {
	collector.SetSnapshotCacheGetter(cache.Get)
}

// WireRecentEvents wires the "recent activity" feed (#74) into the
// status collector so the Overview renders recent traffic events.
func WireRecentEvents(collector *status.Collector, q *auditqueue.Queue) {
	collector.SetRecentEventsFn(func(limit int) []status.RecentEvent {
		evs, _, err := q.QueryEvents("", "", 0, limit)
		if err != nil || len(evs) == 0 {
			return nil
		}
		out := make([]status.RecentEvent, 0, len(evs))
		for _, e := range evs {
			out = append(out, status.RecentEvent{
				Time:        e.Timestamp.UTC().Format(time.RFC3339),
				ProcessName: e.SourceProcess,
				DestHost:    e.TargetHost,
				Action:      e.Action,
			})
		}
		return out
	})
}

// PendingStatusCollectorConfig is the minimal config for the pre-enrollment
// status collector. No audit queue, no thingclient, no real heartbeat.
type PendingStatusCollectorConfig struct {
	Version         string
	HubHTTPURL      string
	CpURL           string
	HeartbeatSec    int
	EnrollMgr       *enrollment.Manager
	BootstrapClient *bootstrap.Client
	QuitAllowed     *bool
}

// InitPendingStatusCollector builds the minimal status collector for the
// pre-enrollment (pending-enrollment mode) path.
func InitPendingStatusCollector(cfg PendingStatusCollectorConfig) *status.Collector {
	return status.NewCollector(status.CollectorConfig{
		Version:         cfg.Version,
		DeviceID:        "",
		DashboardURL:    cfg.HubHTTPURL,
		DownloadURL:     ComposeAgentDownloadURL(cfg.CpURL),
		HeartbeatSec:    cfg.HeartbeatSec,
		UnsyncedCountFn: func() int { return 0 },
		TrustLevelFn:    cfg.EnrollMgr.TrustLevel,
		DeviceAuthModeFn: buildDeviceAuthModeFn(cfg.BootstrapClient),
		QuitAllowedFn:    func() bool { q := cfg.QuitAllowed; return q == nil || *q },
	})
}
