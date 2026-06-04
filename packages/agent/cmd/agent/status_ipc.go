package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/auth"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/bootstrap"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/protectionpause"
	lifecycle "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/state"
	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/diagnostics"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/localrollup"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/policies"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
	config "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/schema"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/status"
	sharedintro "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"

	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
)

// wireStatusServerIPCArgs groups all the objects needed to wire the full
// set of IPC handlers onto the status server.
type wireStatusServerIPCArgs struct {
	statusServer     *status.Server
	statusCollector  *status.Collector
	pauser           *protectionpause.Pauser
	lifecycleEmitter *lifecycle.Emitter
	cfgMgr           *config.Manager
	cfgLoader        *cfgloader.Loader
	localRollup      *localrollup.Aggregator
	auditQueue       *auditqueue.Queue
	policiesCache    *policies.SnapshotCache
	tc               *thingclient.Client
	hubClient        *hub.Client
	diagCollector    *diagnostics.Collector
	bootstrapClient  *bootstrap.Client
	introspectReg    *sharedintro.Registry
	cancel           context.CancelFunc
	cDir             string
	version          string
	commit           string
	builtAt          string
}

// wireStatusServerIPC wires all IPC handlers onto an already-constructed
// status.Server.
func wireStatusServerIPC(a wireStatusServerIPCArgs) {
	// Wire the user-pause Pauser. Wrapped to emit lifecycle events on
	// user-initiated pause/resume.
	a.statusServer.SetPauseProtectionFn(func(seconds int) time.Time {
		resumeAt := a.pauser.Pause(seconds)
		a.lifecycleEmitter.Paused(seconds)
		return resumeAt
	})
	a.statusServer.SetResumeProtectionFn(func() {
		a.pauser.Resume()
		a.lifecycleEmitter.Resumed()
	})

	// QUERY_LIFECYCLE_EVENTS: Activity tab lifecycle history.
	a.statusServer.SetQueryLifecycleFn(a.auditQueue.QueryLifecycle)

	// GET_APPLIED_CONFIG: Dashboard's Policies tab.
	a.statusServer.SetGetAppliedConfigFn(func() any {
		return policies.Build(a.tc, a.policiesCache)
	})

	// REFRESH_POLICIES IPC: Dashboard's "Refresh now" button.
	a.statusServer.SetRefreshPoliciesFn(func(ctx context.Context) error {
		_, _ = a.cfgLoader.RefreshPullKeys(ctx)
		return nil
	})

	// QUERY_STATS: Stats page pre-aggregated metrics from local rollup.
	a.statusServer.SetQueryStatsFn(buildQueryStatsFn(a.localRollup))

	// UNENROLL: Dashboard Sign Out.
	a.statusServer.SetSignOutFn(func(_ context.Context) error {
		slog.Info("sign-out requested via IPC; clearing enrollment")
		if err := auth.ClearEnrollment(a.cDir); err != nil {
			return err
		}
		go func() {
			time.Sleep(200 * time.Millisecond)
			a.cancel()
		}()
		return nil
	})

	// REPORT_PROXY_INSTALL: macOS menu-bar host posts extension install outcomes.
	a.statusServer.SetProxyInstallReportFn(func(report status.ProxyInstallReport) {
		level := slog.LevelInfo
		if report.Outcome != "ok" {
			level = slog.LevelError
		}
		slog.Log(context.Background(), level, "proxy install report",
			"stage", report.Stage,
			"outcome", report.Outcome,
			"error", report.Error,
			"appVersion", report.AppVersion,
		)
	})

	// VERSION command: surfaces the daemon's build identity.
	a.statusServer.SetVersionFn(func() status.VersionInfo {
		return status.VersionInfo{
			Version: a.version,
			Commit:  a.commit,
			BuiltAt: a.builtAt,
			OS:      runtime.GOOS,
			Arch:    runtime.GOARCH,
		}
	})
}

// buildQueryStatsFn constructs the QUERY_STATS IPC closure.
func buildQueryStatsFn(localRollup *localrollup.Aggregator) func(ctx context.Context, req status.QueryStatsRequest) (status.QueryStatsResponse, error) {
	return func(ctx context.Context, req status.QueryStatsRequest) (status.QueryStatsResponse, error) {
		now := time.Now().UTC()
		end := now
		if req.EndRFC3339 != "" {
			if t, err := time.Parse(time.RFC3339, req.EndRFC3339); err == nil {
				end = t
			} else {
				return status.QueryStatsResponse{}, fmt.Errorf("invalid end (need RFC3339): %w", err)
			}
		}
		start := end.Add(-24 * time.Hour)
		if req.StartRFC3339 != "" {
			if t, err := time.Parse(time.RFC3339, req.StartRFC3339); err == nil {
				start = t
			} else {
				return status.QueryStatsResponse{}, fmt.Errorf("invalid start (need RFC3339): %w", err)
			}
		}
		if !end.After(start) {
			return status.QueryStatsResponse{}, fmt.Errorf("end must be after start")
		}
		rows, err := localRollup.QueryRollup(ctx, localrollup.Query{
			StartTime:    start,
			EndTime:      end,
			MetricNames:  req.Metrics,
			DimensionKey: req.DimensionKey,
			SubDimension: req.SubDimension,
		})
		if err != nil {
			return status.QueryStatsResponse{}, err
		}
		out := make([]status.QueryStatsRow, 0, len(rows))
		for _, r := range rows {
			row := status.QueryStatsRow{
				BucketStart:  r.BucketStart.UTC().Format(time.RFC3339),
				MetricName:   r.MetricName,
				DimensionKey: r.DimensionKey,
				SubDimension: r.SubDimension,
				Value:        r.Value,
			}
			if r.Metadata != "" {
				var anyMeta any
				if jerr := json.Unmarshal([]byte(r.Metadata), &anyMeta); jerr == nil {
					row.Metadata = anyMeta
				}
			}
			out = append(out, row)
		}
		return status.QueryStatsResponse{
			StartTime: start.Format(time.RFC3339),
			EndTime:   end.Format(time.RFC3339),
			Granule:   localrollup.Granule(start, end),
			Rows:      out,
		}, nil
	}
}
