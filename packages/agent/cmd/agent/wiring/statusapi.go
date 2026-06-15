package wiring

import (
	"context"
	"log/slog"
	"net/url"
	"runtime"
	"time"

	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/bootstrap"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/protectionpause"
	lifecycle "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/state"
	auditevent "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/policies"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
	config "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/schema"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/status"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	sharedintro "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
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
		Version:          cfg.Version,
		DeviceID:         cfg.ThingID,
		DashboardURL:     cfg.HubHTTPURL,
		DownloadURL:      ComposeAgentDownloadURL(cfg.CpURL),
		CertExpiresAt:    ReadCertExpiry(cfg.CertFile),
		HeartbeatSec:     cfg.HeartbeatSec,
		UnsyncedCountFn:  cfg.AuditQueue.UnsyncedCount,
		TodayStatsFn:     buildTodayStatsFn(cfg.AuditQueue),
		ThingClient:      statusThingClient,
		TrustLevelFn:     cfg.EnrollMgr.TrustLevel,
		DeviceAuthModeFn: buildDeviceAuthModeFn(cfg.BootstrapClient),
		SSOEmailFn:       cfg.EnrollMgr.SSOEmail,
		PausedFn:         cfg.Pauser.IsPaused,
		PausedUntilFn:    cfg.Pauser.ResumesAt,
		QuitAllowedFn:    func() bool { q := cfg.ConfigMgr.Get().QuitAllowed; return q == nil || *q },
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

// StatusServerDeps groups the dependencies for the steady-state status IPC
// server (the enrolled daemon's full command surface).
type StatusServerDeps struct {
	SocketPath string
	Collector  *status.Collector
	HubClient  *hub.Client
	Ctx        context.Context
	Cancel     context.CancelFunc
	Version    string
	Emitter    *lifecycle.Emitter
	AuditQueue *auditqueue.Queue
	ConfigMgr  *config.Manager
	Auth       *SSOAuthState
	// SpillReader hydrates locally-spilled oversize bodies for the detail
	// drawer; nil leaves spilled bodies ref-only.
	SpillReader LocalSpillReader
}

// InitStatusServer builds the status IPC server with the core command set:
// update check, shutdown (lifecycle-emitting), event queries (plain,
// filtered, by-id with local spill hydration), quit-allowed gate, and the
// SSO authenticate/confirm/cancel triple.
func InitStatusServer(d StatusServerDeps) *status.Server {
	statusServer := status.NewServer(
		d.SocketPath,
		d.Collector,
		func() (bool, string, error) {
			info, err := d.HubClient.CheckUpdate(d.Ctx, d.Version, runtime.GOOS)
			if err != nil {
				return false, "", err
			}
			return info.Available, info.Version, nil
		},
		ConfigPullNoOpFn(),
		func() {
			EmitShutdownGracefully(d.Emitter, "ipc_shutdown")
			go func() { time.Sleep(250 * time.Millisecond); d.Cancel() }()
		},
		d.AuditQueue.QueryEvents,
		func() bool { q := d.ConfigMgr.Get().QuitAllowed; return q == nil || *q },
		d.Auth.Authenticate,
	)
	statusServer.SetConfirmAuthFn(status.ConfirmAuthFn(d.Auth.Confirm))
	statusServer.SetCancelAuthFn(status.CancelAuthFn(d.Auth.Cancel))
	// AI-only + Since filter path; the UI Traffic page sends
	// `ai_only=1&since=<unix-ms>` URL params to QUERY_EVENTS.
	statusServer.SetQueryEventsFiltered(func(search, action string, aiOnly bool, sinceMs int64, offset, limit int) ([]auditevent.Event, int, error) {
		var since time.Time
		if sinceMs > 0 {
			since = time.UnixMilli(sinceMs)
		}
		return d.AuditQueue.QueryEventsFiltered(auditqueue.QueryEventsFilter{
			Search: search,
			Action: action,
			AIOnly: aiOnly,
			Since:  since,
			Offset: offset,
			Limit:  limit,
		})
	})
	// Detail-by-id: the drawer fetches body + normalized + spill on demand.
	// Oversize bodies that spilled locally are read back off disk here
	// (SpillReader); bodies already uploaded to S3 stay ref-only (no agent
	// S3 GET credential) and the UI shows a "view in Control Plane" affordance.
	statusServer.SetEventByID(func(id string) (*auditevent.Event, error) {
		ev, err := d.AuditQueue.EventByID(id)
		if err != nil || ev == nil {
			return ev, err
		}
		HydrateLocalSpill(ev, d.SpillReader)
		return ev, nil
	})
	return statusServer
}

// StartStatusAPI wires the open-browser helper (allowed hosts resolved from
// the bootstrap Control Plane URL), launches the status server accept loop,
// and installs the runtime-introspection snapshot command.
func StartStatusAPI(
	statusServer *status.Server,
	bootstrapClient *bootstrap.Client,
	introspectReg *sharedintro.Registry,
	recoveryCfg shareddiag.RecoveryConfig,
) {
	browserOpener := InitOpenBrowser()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if info, err := bootstrapClient.Get(ctx); err == nil && info.ControlPlaneURL != "" {
			if u, perr := url.Parse(info.ControlPlaneURL); perr == nil && u.Hostname() != "" {
				browserOpener.SetAllowedHosts(u.Hostname())
			}
		}
	}()
	statusServer.SetOpenBrowserFn(browserOpener.Open)
	go func() {
		rcfg := recoveryCfg
		rcfg.Source = "status-api"
		defer shareddiag.Recover(rcfg, nil)
		_ = statusServer.Start()
	}()
	statusServer.SetRuntimeFn(func(ctx context.Context) any { return introspectReg.Snapshot(ctx) })
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
		Version:          cfg.Version,
		DeviceID:         "",
		DashboardURL:     cfg.HubHTTPURL,
		DownloadURL:      ComposeAgentDownloadURL(cfg.CpURL),
		HeartbeatSec:     cfg.HeartbeatSec,
		UnsyncedCountFn:  func() int { return 0 },
		TrustLevelFn:     cfg.EnrollMgr.TrustLevel,
		DeviceAuthModeFn: buildDeviceAuthModeFn(cfg.BootstrapClient),
		QuitAllowedFn:    func() bool { q := cfg.QuitAllowed; return q == nil || *q },
	})
}
