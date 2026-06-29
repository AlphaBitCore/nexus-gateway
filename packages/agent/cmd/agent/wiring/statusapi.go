package wiring

import (
	"bytes"
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
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
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

// WireRecentEvents wires the "recent activity" feed into the
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
	// NormalizeRegistry is the shared Tier 1+2+3 chain used to recompute the
	// normalized projection at view time from the stored (already-redacted)
	// body. The write path no longer persists normalized, so the detail drawer
	// reconstructs it on demand — the same model the control plane uses. Nil
	// disables recompute (the drawer then shows only what an old row stored).
	NormalizeRegistry *normalizecore.Registry
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
		recomputeNormalizedForView(ev, d.NormalizeRegistry)
		return ev, nil
	})
	return statusServer
}

// recomputeNormalizedForView reconstructs the detail event's normalized
// projections at view time from the stored, already-redacted inline bodies —
// the same model the control plane uses. The agent write path no longer
// persists the normalized projection (it is redundant: the redacted body is the
// source of truth), so the columns are empty for current rows and the drawer
// recomputes here on demand. A row that still carries a stored projection (an
// upload from an older agent build) is left untouched. Best-effort: an empty
// registry, empty body, or a normalize miss leaves the direction empty.
func recomputeNormalizedForView(ev *auditevent.Event, reg *normalizecore.Registry) {
	if ev == nil || reg == nil {
		return
	}
	audit := normalizecore.BuildAuditFn(reg, nil)
	if audit == nil {
		return
	}
	// ev.IngressFormat is the domain-matched adapter id (interception_domain.
	// adapter_id) persisted as the authoritative normalize adapter — the same
	// value traffic_event.ingress_format carries, so the agent-UI and CP-UI
	// recompute resolve via the identical key and agree byte-for-byte. When it
	// is empty (an older row captured before this field, or a flow that matched
	// no domain adapter) the registry falls back to request path + content
	// sniff, preserving the prior behaviour. The agent never keys on a provider:
	// ev.ProviderName is a best-effort body-sniff guess, not the wire shape.
	adapter := ev.IngressFormat
	if len(ev.NormalizedRequest) == 0 && len(ev.PayloadRequest) > 0 {
		if raw, _, _ := audit("request", "application/json", adapter, ev.ModelName, ev.Path, false, ev.PayloadRequest); len(raw) > 0 {
			ev.NormalizedRequest = raw
		}
	}
	if len(ev.NormalizedResponse) == 0 && len(ev.PayloadResponse) > 0 {
		contentType, stream := "application/json", false
		if looksLikeSSE(ev.PayloadResponse) {
			contentType, stream = "text/event-stream", true
		}
		if raw, _, _ := audit("response", contentType, adapter, ev.ModelName, ev.Path, stream, ev.PayloadResponse); len(raw) > 0 {
			ev.NormalizedResponse = raw
		}
	}
}

// looksLikeSSE reports whether a captured response body is a Server-Sent Events
// stream, so the view-time recompute selects the streaming decoder. The agent
// audit row does not store the response content type, so the body is sniffed:
// SSE frames begin with a "data:" or "event:" field after optional leading
// whitespace.
func looksLikeSSE(body []byte) bool {
	b := bytes.TrimLeft(body, " \t\r\n")
	return bytes.HasPrefix(b, []byte("data:")) || bytes.HasPrefix(b, []byte("event:"))
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
