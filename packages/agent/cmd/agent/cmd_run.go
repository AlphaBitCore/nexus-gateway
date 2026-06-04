package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/host/updater"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/attestation"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/auth"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/bootstrap"
	lifecycle "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/state"
	auditevent "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/diagnostics"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/localrollup"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
	config "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/schema"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/status"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	sharedintro "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
	metricsplatform "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/cmd/agent/platformshim"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/cmd/agent/wiring"
)

// emitShutdownGracefully emits an agent.shutdown lifecycle event and
// then sleeps briefly so the WS outbox has time to flush the message
// to Hub before the caller cancels the main context. Without the
// flush window, the WS write pump's select sees ctx.Done at the same
// instant outCh has a pending diag_event envelope, and exits before
// the write — losing the shutdown row from Hub's view.
//
// Call this at EVERY shutdown-triggering cancel site (signal handler,
// user-quit flag watcher, status-IPC SHUTDOWN) BEFORE the cancel().
// 200 ms is empirically enough on a healthy local-Hub round-trip.
func emitShutdownGracefully(e *lifecycle.Emitter, reason string) {
	if e == nil {
		return
	}
	e.Shutdown(reason)
	time.Sleep(200 * time.Millisecond)
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "agent.yaml", "path to agent config file")
	_ = fs.Parse(args)

	// User-controlled lifecycle gate. When the Swift menu-bar app's Quit
	// handler writes the user-quit flag, every subsequent launchd respawn
	// of this daemon should exit immediately and stay dead until the
	// user re-launches NexusAgent.app (which clears the flag).
	if _, err := os.Stat(userQuitFlagPath()); err == nil {
		fmt.Fprintf(os.Stderr, "nexus-agent: user-quit flag present at %s — exiting (remove the flag or re-launch the menu-bar app to bring the daemon back)\n", userQuitFlagPath())
		return 0
	}

	cfg, err := config.LoadFromFile(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}

	logger, err := logging.NewLogger(logging.Config{
		Level:        cfg.Log.Level,
		Format:       cfg.Log.Format,
		File:         cfg.Log.File,
		StackOnError: cfg.Log.StackOnError,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	slog.SetDefault(logger)

	// macOS: flush mDNSResponder on every daemon startup so launchd
	// respawns, OS reboots, and manual restarts all leave the user's
	// getaddrinfo cache fresh. Combined with the shutdown-side flush
	// (graceful shutdown path), the install/uninstall script flushes,
	// and the menu-bar Quit flush, this covers every "daemon comes up"
	// path so the user never has to type `dscacheutil -flushcache`
	// manually. See memory: feedback_macos_mdns_flush_after_ne_state_change.
	flushMDNSResponderIfDarwin()

	// Check crash loop before full startup.
	statusFile := cfg.AuditDBPath + ".status"
	if updater.DetectCrashLoop(os.Args[0], statusFile, 30*time.Second) {
		slog.Warn("rolled back to previous version due to crash loop")
	}
	_ = updater.WriteStartStatus(statusFile)

	// Ops metrics registry (L3 business + Prometheus).
	opsReg, processStartTime := wiring.InitOpsMetrics()

	// Hub HTTP client. enrollMgrRef is filled below; the closure captures
	// the pointer so it sees the updated value rather than nil at construction time.
	cDir := certDir(cfg)
	var enrollMgrRef *enrollment.Manager
	hubClient, err := wiring.InitHubClient(wiring.HubClientConfig{
		HubHTTPURL: cfg.HubHTTPURL,
		CertFile:   cfg.CertFile,
		KeyFile:    cfg.KeyFile,
		CACertFile: cfg.EffectiveHubCA(),
		DeviceTokenFn: func() string {
			tok, _ := auth.LoadDeviceToken(cDir)
			return tok
		},
		ThingIDFn: func() string {
			if enrollMgrRef != nil {
				return enrollMgrRef.ThingID()
			}
			return ""
		},
	})
	if err != nil {
		slog.Error("failed to init hub http client", "error", err)
		return 1
	}

	// Enrollment manager.
	hubEnroller, err := enrollment.NewHubEnrollClient(cfg.HubHTTPURL, cfg.EffectiveHubCA())
	if err != nil {
		slog.Error("failed to init hub enroll client", "error", err)
		return 1
	}
	enrollMgr := enrollment.NewManager(
		cDir,
		enrollment.WithHubEnroller(hubEnroller),
		enrollment.WithCertRenewer(hubClient),
	)
	enrollMgrRef = enrollMgr

	if !enrollMgr.IsEnrolled() {
		runCtx, runCancel := context.WithCancel(context.Background())
		defer runCancel()
		return runPendingEnrollment(runCtx, cfg, logger, enrollMgr, hubEnroller, hubClient)
	}
	thingID := enrollMgr.ThingID()
	slog.Info("device enrolled", "thing_id", thingID)

	cfgMgr := config.NewManager(cfg)
	defer cfgMgr.Close()

	// forward-declared for the agent_settings shadow adapter.
	var statusCollector *status.Collector

	// Hub thingclient (WebSocket primary).
	var tc *thingclient.Client
	if cfg.HubURL != "" {
		deviceToken, tokenErr := auth.LoadDeviceToken(cDir)
		if tokenErr != nil {
			logger.Warn("device token not found, audit uploads will use HTTP fallback only", "error", tokenErr)
		} else {
			var tcErr error
			tc, tcErr = wiring.InitThingClient(wiring.ThingClientConfig{
				HubURL:           cfg.HubURL,
				HubHTTPURL:       cfg.HubHTTPURL,
				ThingID:          thingID,
				Version:          version,
				DeviceToken:      deviceToken,
				Logger:           logger,
				ProcessStart:     processStartTime,
				OpsReg:           opsReg,
				ComposeVersionFn: platformshim.ComposeThingVersion,
			})
			if tcErr != nil {
				logger.Warn("thingclient init failed, audit uploads will use HTTP fallback only", "error", tcErr)
			}
		}
	}

	// OpenTelemetry.
	tp, _ := wiring.InitTelemetry(wiring.TelemetryConfig{
		OtelEnabled:      cfg.OtelEnabled,
		OtelEndpoint:     cfg.OtelEndpoint,
		OtelServiceName:  cfg.OtelServiceName,
		OtelSamplingRate: cfg.OtelSamplingRate,
	}, logger)
	if tp != nil {
		defer tp.Shutdown(context.Background()) //nolint:errcheck
	}

	// Local body capture intent (yaml localBodyCapture, default true). Nil
	// means unset → default-on, so users always see their own AI traffic
	// locally regardless of the Hub-pushed payload_capture (upload) config.
	localBodyCapture := cfg.LocalBodyCapture == nil || *cfg.LocalBodyCapture

	// Compliance + policy subsystem.
	comp := wiring.InitCompliance(wiring.ComplianceConfig{
		DefaultAction:             cfg.DefaultAction,
		ExemptionEnabled:          cfg.ExemptionEnabled,
		ExemptionFailureThreshold: cfg.ExemptionFailureThreshold,
		ExemptionWindowSec:        cfg.ExemptionWindowSec,
		ExemptionDurationSec:      cfg.ExemptionDurationSec,
		ExemptionAllowlist:        cfg.ExemptionAllowlist,
		ExemptionDenylist:         cfg.ExemptionDenylist,
		LocalBodyCapture:          localBodyCapture,
	}, logger)

	// Atomic flag updated by the agent_settings shadow applier and
	// read by the attestation Signer's enabled-lookup closure.
	// Initial value false so the agent stays silent on attestation
	// until Hub pushes a shadow with attestationEnabled=true.
	attestationEnabledFlag := &atomic.Bool{}

	// Shadow config appliers — all defined in configappliers.go.
	appliers := buildConfigAppliers(buildConfigAppliersArgs{
		cfg:                  cfg,
		cfgMgr:               cfgMgr,
		logger:               logger,
		statusCollectorPtr:   &statusCollector,
		agentPipeline:        comp.AgentPipeline,
		exemptionStore:       comp.ExemptionStore,
		payloadCaptureStore:  comp.PayloadCaptureStore,
		localCaptureStore:    comp.LocalCaptureStore,
		localBodyCapture:     localBodyCapture,
		streamingPolicyStore: comp.StreamingPolicyStore,
		policiesCache:        comp.PoliciesCache,
		attestationEnabled:   attestationEnabledFlag,
	})
	// Cancel the diag-mode expiry timer on shutdown. The level itself is left
	// as-is — the process is exiting.
	defer appliers.diagModeLevel.stop()

	// Construct the attestation Signer the bridge wiring installs as
	// the UpstreamTransport request injector. Reads the Ed25519
	// private key from <certDir>/attestation-key.pem (written by
	// enrollment). If the file is absent, InjectInto returns nil
	// header (fail-open) and the request flows unattested.
	attestationSigner := attestation.NewSigner(
		filepath.Join(cDir, "attestation-key.pem"),
		thingID,
		func() bool { return attestationEnabledFlag.Load() },
		logger,
	)

	// configCache is opened on the audit queue's SQLCipher DB below, once
	// the queue exists. The loader's per-key persist wrappers read it
	// through this late-bound getter; restoreCachedConfig replays it at
	// boot so an agent that starts while Hub is unreachable enforces its
	// last-known policy instead of empty resolvers.
	var configCache atomic.Pointer[shadow.Cache]
	cfgLoader, cfgRestoreMap := buildConfigLoader(configDispatchDeps{
		Logger:              logger,
		ThingID:             thingID,
		Outcomes:            tc.Outcomes(),
		HubHTTPURL:          cfg.HubHTTPURL,
		DeviceToken:         func() string { t, _ := auth.LoadDeviceToken(cDir); return t }(),
		Exemptions:          appliers.exemptions,
		KillSwitch:          appliers.killSwitchApply,
		AgentSettings:       appliers.agentSettings,
		DiagMode:            appliers.diagMode,
		InterceptionDomains: appliers.interceptionDomains,
		HookConfig:          appliers.hookConfig,
		PayloadCapture:      appliers.payloadCapture,
		StreamingCompliance: appliers.streamingCompliance,
		InstalledRulePacks:  appliers.installedRulePacks,
		UserContext:         appliers.userContext,
		ConfigCache:         configCache.Load,
	})

	// Runtime introspection registry.
	introspectReg := wiring.InitIntrospect(thingID, version)
	introspectReg.Register(sharedintro.SourceFunc{
		SourceName: "config.killswitch",
		Fn:         func(_ context.Context) (any, error) { return appliers.killSwitchObj.SnapshotState(), nil },
	})
	introspectReg.Register(sharedintro.SourceFunc{
		SourceName: "config.payload_capture",
		Fn:         func(_ context.Context) (any, error) { return comp.PayloadCaptureStore.Get(), nil },
	})

	// Wire Hub config-changed callback onto thingclient.
	if tc != nil {
		tc.OnConfigChanged(func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
			logger.Info("thing config change received", "config_keys", len(desired))
			reported, applyErr := cfgLoader.Apply(context.Background(), desired)
			logger.Info("config apply finished", "reported_keys", len(reported))
			return reported, applyErr
		})
	}

	// Config reload goroutine (must start before statusCollector is used).
	configCh := cfgMgr.Subscribe()
	go startConfigReloadGoroutine(configCh)

	// Audit queue (SQLCipher-encrypted).
	auditQueue, err := wiring.InitAuditQueue(cfg.AuditDBPath, logger)
	if err != nil {
		slog.Error("failed to open audit queue", "error", err)
		return 1
	}
	defer auditQueue.Close() //nolint:errcheck

	// Backpressure store + poller.
	backpressureStore := wiring.InitBackpressure(context.Background(), auditQueue, logger)

	// Diag subsystem (migrates DB, wires multi-handler logger).
	diagBundle, composedLogger, err := wiring.InitDiag(auditQueue, tc, thingID, version, opsReg, logger)
	if err != nil {
		slog.Error("failed to migrate pending_diag_event", "error", err)
		return 1
	}
	slog.SetDefault(composedLogger)
	logger = composedLogger
	recoveryCfg := diagBundle.RecoveryCfg
	defer shareddiag.Recover(recoveryCfg, nil)

	// Context + signal handling.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Bootstrap client (public TLS, not mTLS-pinned).
	bootstrapClient := bootstrap.New(cfg.HubHTTPURL, bootstrap.DefaultHTTPClient(), cfg.CpURL)
	warmBootstrap(ctx, bootstrapClient, logger)

	// Status collector (initialized early so heartbeat + drain can update it).
	statusCollector = wiring.InitStatusCollector(wiring.StatusCollectorConfig{
		Version:         version,
		ThingID:         thingID,
		HubHTTPURL:      cfg.HubHTTPURL,
		CpURL:           cfg.CpURL,
		CertFile:        cfg.CertFile,
		HeartbeatSec:    cfg.HeartbeatIntervalSec,
		AuditQueue:      auditQueue,
		ConfigMgr:       cfgMgr,
		EnrollMgr:       enrollMgr,
		Pauser:          appliers.pauser,
		BootstrapClient: bootstrapClient,
		ThingClient:     tc,
		Logger:          logger,
	})
	wiring.WireSnapshotCacheToCollector(statusCollector, comp.PoliciesCache)
	wiring.WireRecentEvents(statusCollector, auditQueue)

	// Offline config cache. Opened on the audit queue's SQLCipher DB so
	// applied policy is encrypted at rest alongside the audit log. Placed
	// after statusCollector so a restored agent_settings entry can reach
	// every subsystem it touches (the applier reads statusCollector). Once
	// stored, the loader's per-key persist wrappers mirror every apply
	// here. Replaying the cache now brings enforcement up immediately —
	// well before platform interception starts and before the first Hub
	// pull; a reachable Hub supersedes it via the config-startup refresh,
	// while an unreachable Hub leaves the agent on last-known policy
	// (stale but enforced — never fail-closed).
	if cache, cacheErr := shadow.NewCache(auditQueue.DB()); cacheErr != nil {
		logger.Warn("config_cache open failed; offline restore disabled", "error", cacheErr)
	} else {
		configCache.Store(cache)
		restoreCachedConfig(ctx, cache, cfgRestoreMap, logger)
	}

	staticInfo := metricsplatform.CaptureStaticInfo(metricsplatform.BuildInfo{
		ServiceVersion: "nexus-agent/" + version,
		StartTime:      processStartTime.Format(time.RFC3339),
	})

	// Thingclient callbacks (disconnect/reconnect/heartbeat).
	if tc != nil {
		tc.OnDisconnect(func() { statusCollector.SetGatewayConnected(false) })
		tc.OnHeartbeatTick(func() { statusCollector.SetLastHeartbeat(time.Now()) })
		tcLocal, staticInfoLocal := tc, staticInfo
		tc.OnReconnect(func() {
			statusCollector.SetGatewayConnected(true)
			statusCollector.SetLastHeartbeat(time.Now())
			ctxPush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := tcLocal.UpdateStaticInfo(ctxPush, staticInfoLocal); err != nil {
				logger.Warn("static_info push failed on reconnect", "error", err)
			}
			drained := diagBundle.ReconnectBuffer.Drain()
			for _, e := range drained {
				if pushErr := tcLocal.PushDiagEvent(ctxPush, e); pushErr != nil {
					logger.Debug("reconnect drain push failed", "error", pushErr, "messageHash", e.MessageHash)
				}
			}
		})
	}

	// Diag dedup-tick goroutine (10s cadence).
	go startDiagDedupGoroutine(ctx, tc, diagBundle, recoveryCfg, logger)

	// Connection bridge. The inspect path captures + spills oversize bodies
	// to the local spill store inside tlsbump; the flow-level path no longer
	// uploads bodies (passthrough/deny flows carry no decrypted HTTP body).
	connHandler := wiring.InitConnectionBridge(wiring.ConnectionBridgeConfig{
		PolicyEngine:            comp.PolicyEngine,
		AgentPipeline:           comp.AgentPipeline,
		AuditQueue:              auditQueue,
		ThingID:                 thingID,
		KillSwitch:              appliers.killSwitchObj,
		ProviderTrafficNotifier: statusCollector.MarkProviderTraffic,
	})

	// Spill uploader (Hub presign → S3) + local spill reader: the audit drain
	// reads each oversize body back from local disk and uploads it to S3,
	// shipping an S3 SpillRef to Hub. The local file stays put so the agent's
	// own detail view can read it without an S3 GET credential.
	spillUploader := wiring.InitSpillUploader(hubClient)
	var spillReader wiring.LocalSpillReader
	if store, spillErr := wiring.NewLocalSpillStore(); spillErr != nil {
		logger.Warn("audit drain: local spill store unavailable; oversize bodies will not upload", "error", spillErr)
	} else {
		spillReader = store
	}

	// Drain upload logic. comp.PayloadCaptureStore (Hub-pushed) is the body
	// UPLOAD gate: when its StoreRequest/ResponseBody flags are off the drain
	// ships metadata only and the body stays local.
	drainUpload := buildDrainUpload(ctx, tc, hubClient, auditQueue, statusCollector, cfgMgr, thingID, comp.PayloadCaptureStore, spillReader, spillUploader, logger)

	// Audit drain supervisor.
	var drainWg sync.WaitGroup
	drainWg.Add(1)
	go startAuditDrainSupervisor(ctx, auditQueue, &drainWg, drainUpload, recoveryCfg, cfg, logger)

	// Audit prune goroutine.
	go startAuditPruneGoroutine(ctx, auditQueue, recoveryCfg, cfg)

	// Local rollup goroutine.
	localRollup := wiring.InitLocalRollup(auditQueue, logger)
	go startLocalRollupGoroutine(ctx, localRollup, recoveryCfg)

	// Exemption cleanup + upload goroutines.
	go startExemptionCleanupGoroutine(ctx, comp.ExemptionStore, recoveryCfg)
	go startExemptionUploadGoroutine(ctx, comp.ExemptionStore, hubClient, thingID, recoveryCfg)

	// Auto-updater.
	up := wiring.InitUpdater(hubClient, cfg.UpdaterEnabled, cfg.UpdaterCheckSec, version, runtime.GOOS, os.Args[0])
	go func() {
		rcfg := recoveryCfg
		rcfg.Source = "updater"
		defer shareddiag.Recover(rcfg, nil)
		up.RunWithAvailabilityCallback(ctx, statusCollector.SetUpdateAvailable)
	}()

	// Drain pending crash DiagEvents to Hub before opening the WebSocket.
	drainPendingDiagEvents(ctx, diagBundle, hubClient, cDir, cfg, thingID, logger)

	// Start thingclient.
	if tc != nil {
		if err := tc.Start(ctx); err != nil {
			logger.Warn("thingclient start failed, falling back to HTTP audit upload", "error", err)
			tc = nil
		} else {
			defer func() {
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer shutdownCancel()
				_ = tc.Close(shutdownCtx)
			}()
			logger.Info("connected to Hub via thingclient", "thing_id", thingID)
			tcLocal, staticInfoLocal := tc, staticInfo
			go func() {
				rcfg := recoveryCfg
				rcfg.Source = "static-info-push"
				defer shareddiag.Recover(rcfg, nil)
				time.Sleep(500 * time.Millisecond)
				ctxPush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := tcLocal.UpdateStaticInfo(ctxPush, staticInfoLocal); err != nil {
					logger.Warn("static_info push failed at startup", "error", err)
				}
			}()
			loaderLocal := cfgLoader
			go func() {
				rcfg := recoveryCfg
				rcfg.Source = "config-startup-refresh"
				defer shareddiag.Recover(rcfg, nil)
				time.Sleep(750 * time.Millisecond)
				refreshCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				_, _ = loaderLocal.RefreshPullKeys(refreshCtx)
			}()
		}
	}

	// Lifecycle emitter + kill switch + pauser.
	lifecycleEmitter := wiring.InitLifecycleEmitter(tc, auditQueue, wiring.LifecycleEmitterConfig{
		ThingID:      thingID,
		AgentVersion: version,
		Logger:       logger,
	})

	// SSO auth state for IPC.
	hostname, _ := os.Hostname()
	ssoFlow := &enrollment.Flow{
		HubEnroller: hubEnroller, Manager: enrollMgr, Hostname: hostname,
		OS: runtime.GOOS, OSVersion: osVersion(), AgentVersion: version,
		ResolveCpURL: buildResolveCpURL(bootstrapClient),
	}
	authState := &ssoAuthState{flow: ssoFlow, mgr: enrollMgr, bootstrap: bootstrapClient}

	// Status server.
	statusSocketPath := guiSocketPath()
	statusServer := status.NewServer(
		statusSocketPath,
		statusCollector,
		func() (bool, string, error) {
			info, err := hubClient.CheckUpdate(ctx, version, runtime.GOOS)
			if err != nil {
				return false, "", err
			}
			return info.Available, info.Version, nil
		},
		func() (bool, string, error) { return true, "", nil }, // config pull no-op
		func() {
			emitShutdownGracefully(lifecycleEmitter, "ipc_shutdown")
			go func() { time.Sleep(250 * time.Millisecond); cancel() }()
		},
		auditQueue.QueryEvents,
		func() bool { q := cfgMgr.Get().QuitAllowed; return q == nil || *q },
		authState.authenticate,
	)
	statusServer.SetConfirmAuthFn(status.ConfirmAuthFn(authState.confirm))
	statusServer.SetCancelAuthFn(status.CancelAuthFn(authState.cancel))
	// #88 — wire the AI-only + Since filter path; UI Traffic page sends
	// `ai_only=1&since=<unix-ms>` URL params to QUERY_EVENTS.
	statusServer.SetQueryEventsFiltered(func(search, action string, aiOnly bool, sinceMs int64, offset, limit int) ([]auditevent.Event, int, error) {
		var since time.Time
		if sinceMs > 0 {
			since = time.UnixMilli(sinceMs)
		}
		return auditQueue.QueryEventsFiltered(auditqueue.QueryEventsFilter{
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
	// (spillReader); bodies already uploaded to S3 stay ref-only (no agent
	// S3 GET credential) and the UI shows a "view in Control Plane" affordance.
	statusServer.SetEventByID(func(id string) (*auditevent.Event, error) {
		ev, err := auditQueue.EventByID(id)
		if err != nil || ev == nil {
			return ev, err
		}
		wiring.HydrateLocalSpill(ev, spillReader)
		return ev, nil
	})

	// Wire IPC handlers onto the status server.
	wireStatusServerIPC(wireStatusServerIPCArgs{
		statusServer:     statusServer,
		statusCollector:  statusCollector,
		pauser:           appliers.pauser,
		lifecycleEmitter: lifecycleEmitter,
		cfgMgr:           cfgMgr,
		cfgLoader:        cfgLoader,
		localRollup:      localRollup,
		auditQueue:       auditQueue,
		policiesCache:    comp.PoliciesCache,
		tc:               tc,
		hubClient:        hubClient,
		diagCollector:    buildDiagCollector(cfg),
		bootstrapClient:  bootstrapClient,
		introspectReg:    introspectReg,
		cancel:           cancel,
		cDir:             cDir,
		version:          version,
		commit:           commit,
		builtAt:          builtAt,
	})

	// OpenBrowser + status API start.
	browserOpener := wiring.InitOpenBrowser()
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
	defer statusServer.Stop()

	slog.Info("agent running", "thing_id", thingID, "heartbeat", cfg.HeartbeatIntervalSec,
		"drain", cfg.AuditDrainIntervalSec, "statusAPI", statusSocketPath)

	// Self-intercept guard: write daemon PID so the NE filter can pass through own-process traffic.
	pidPath := "/var/run/nexus-agent/daemon.pid"
	writeDaemonPID(pidPath, logger)
	lifecycleEmitter.Startup()

	// Platform + connection bridge.
	plat := wiring.InitPlatform(cfg.PlatformBridgeAddress)

	// Publish interception mode.
	var interceptionModeRef atomic.Pointer[string]
	if r, ok := plat.(api.InterceptionModeReporter); ok {
		mode := string(r.InterceptionMode())
		interceptionModeRef.Store(&mode)
	}
	platformshim.WireDarwinBackpressure(plat, backpressureStore)
	if r, ok := plat.(api.InterceptionHealthReporter); ok {
		statusCollector.SetInterceptionHealthFn(func() status.InterceptionHealth {
			h := r.InterceptionHealth()
			return status.InterceptionHealth{
				StartedAt:        h.StartedAt,
				Connected:        h.Connected,
				ConnectionsTotal: h.ConnectionsTotal,
				ActiveSessions:   h.ActiveSessions,
				LastFlowAt:       h.LastFlowAt,
			}
		})
	}
	diagCollector := &diagnostics.Collector{
		HubHTTPURL: cfg.HubHTTPURL,
		CertPath:   cfg.CertFile,
		LogFile:    cfg.Log.File,
		TailLines:  50,
		InterceptionModeFn: func() string {
			if p := interceptionModeRef.Load(); p != nil {
				return *p
			}
			return ""
		},
	}
	statusServer.SetDiagnosticsFn(func(ctx context.Context) status.Diagnostics {
		s := diagCollector.Collect(ctx)
		return status.Diagnostics{
			HubReachable: s.HubReachable, CertPath: s.CertPath, LogTail: s.LogTail,
			InterceptionMode: s.InterceptionMode, Error: s.Error,
		}
	})

	// Build the shared Tier 1+2+3 normalize Registry once (shared with
	// Hub agent_audit / ai-gateway / compliance-proxy).
	normalizeRegistry := wiring.InitNormalizeRegistry()

	// Linux/Windows: wire the shared/tlsbump bridge deps onto the platform
	// BEFORE Start launches the accept loop, so inspect flows bump through
	// proxy.BumpFlow — the same engine macOS, the compliance proxy, and the
	// AI gateway use. macOS wires its own deps and starts the NE bridge
	// listener in WireDarwinBridge below.
	if runtime.GOOS != "darwin" {
		if bridgeDeps, depErr := wiring.BuildBridgeDeps(wiring.BridgeDepsArgs{
			Logger:        logger,
			AgentPipeline: comp.AgentPipeline,
			// Local capture store (always-on by default) gates body capture in
			// tlsbump — independent of the Hub upload config.
			PayloadCaptureStore:  comp.LocalCaptureStore,
			AuditQueue:           auditQueue,
			StreamingPolicyStore: comp.StreamingPolicyStore,
			NormalizeRegistry:    normalizeRegistry,
			AttestationSigner:    attestationSigner,
		}); depErr != nil {
			logger.Warn("bridge deps build failed; inspect flows fall through to passthrough", "error", depErr)
		} else if r, ok := plat.(api.BridgeDepsReceiver); ok {
			r.SetBridgeDeps(bridgeDeps)
			logger.Info("inspect flows wired through shared/tlsbump.BumpConnection")
		}
	}

	go func() {
		rcfg := recoveryCfg
		rcfg.Source = "platform-intercept"
		defer shareddiag.Recover(rcfg, nil)
		if err := plat.Start(ctx, connHandler); err != nil {
			slog.Warn("platform interception not available", "error", err)
		}
	}()
	defer plat.Stop() //nolint:errcheck

	bridgeCloser := platformshim.WireDarwinBridge(context.Background(), plat, platformshim.DarwinBridgeArgs{
		Logger:            logger,
		BridgeAddr:        cfg.MitmBridgeAddr,
		AgentPipeline:     comp.AgentPipeline,
		NormalizeRegistry: normalizeRegistry,
		// Local capture store (always-on by default) gates body capture in the
		// macOS NE bridge's tlsbump path — independent of the Hub upload config.
		PayloadCaptureStore:  comp.LocalCaptureStore,
		AuditQueue:           auditQueue,
		StreamingPolicyStore: comp.StreamingPolicyStore,
		AttestationSigner:    attestationSigner,
	})
	if bridgeCloser != nil {
		defer bridgeCloser.Close() //nolint:errcheck
	}

	// User-quit flag watcher goroutine.
	go startQuitFlagWatcher(ctx, lifecycleEmitter, cancel, recoveryCfg, logger)

	// Wait for shutdown.
	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig)
		emitShutdownGracefully(lifecycleEmitter, fmt.Sprintf("signal:%v", sig))
		cancel()
	case <-ctx.Done():
		if _, err := os.Stat(userQuitFlagPath()); err != nil {
			emitShutdownGracefully(lifecycleEmitter, "ctx_done")
		}
	}

	// Graceful shutdown: wait for audit drain (max 10s).
	slog.Info("draining audit queue...")
	drainDone := make(chan struct{})
	go func() {
		rcfg := recoveryCfg
		rcfg.Source = "shutdown-wait"
		defer shareddiag.Recover(rcfg, nil)
		drainWg.Wait()
		close(drainDone)
	}()
	select {
	case <-drainDone:
		slog.Info("audit queue drained")
	case <-time.After(10 * time.Second):
		slog.Warn("audit drain timeout after 10s")
	}

	// macOS: flush DNS cache + restart mDNSResponder right before exit so
	// the user's network goes cleanly back to native routing. After NE
	// filter teardown getaddrinfo can hang ~5 s on stale cache entries
	// while `dig` direct-to-UDP/53 still works — looks exactly like
	// "I quit the agent and now I can't reach Google." The daemon runs
	// as root and is the only process at this point with the privs to
	// killall -HUP mDNSResponder; the menu-bar host cannot. See memory:
	// feedback_macos_mdns_flush_after_ne_state_change.
	flushMDNSResponderIfDarwin()

	slog.Info("agent stopped")
	return 0
}

// runPendingEnrollment is the cold-boot path: the daemon's `run` command
// starts here when no device certificate exists on disk.
func runPendingEnrollment(
	ctx context.Context,
	cfg *config.AgentConfig,
	logger *slog.Logger,
	enrollMgr *enrollment.Manager,
	hubEnroller *enrollment.HubEnrollClient,
	hubClient *hub.Client,
) int {
	logger.Info("agent starting in pending-enrollment mode (no device cert on disk)")
	bootstrapClient := bootstrap.New(cfg.HubHTTPURL, bootstrap.DefaultHTTPClient(), cfg.CpURL)
	_ = hubClient
	warmBootstrap(ctx, bootstrapClient, logger)
	hostname, _ := os.Hostname()

	enrolledCh := make(chan struct{})
	var enrolledOnce sync.Once
	signalEnrolled := func() { enrolledOnce.Do(func() { close(enrolledCh) }) }

	ssoFlow := &enrollment.Flow{
		HubEnroller: hubEnroller, Manager: enrollMgr, Hostname: hostname,
		OS: runtime.GOOS, OSVersion: osVersion(), AgentVersion: version,
		ResolveCpURL: buildResolveCpURL(bootstrapClient),
	}
	authState := &ssoAuthState{
		flow:      ssoFlow,
		mgr:       enrollMgr,
		bootstrap: bootstrapClient,
		onSuccess: signalEnrolled,
	}
	tokenEnroll := func(token string) (string, error) {
		if err := enrollMgr.Enroll(ctx, token, hostname, runtime.GOOS, osVersion(), version); err != nil {
			return "", err
		}
		signalEnrolled()
		return enrollMgr.ThingID(), nil
	}

	collector := wiring.InitPendingStatusCollector(wiring.PendingStatusCollectorConfig{
		Version:         version,
		HubHTTPURL:      cfg.HubHTTPURL,
		CpURL:           cfg.CpURL,
		HeartbeatSec:    cfg.HeartbeatIntervalSec,
		EnrollMgr:       enrollMgr,
		BootstrapClient: bootstrapClient,
		QuitAllowed:     cfg.QuitAllowed,
	})

	statusSocketPath := guiSocketPath()
	statusServer := status.NewServer(
		statusSocketPath, collector,
		nil, nil, func() {}, nil,
		func() bool { q := cfg.QuitAllowed; return q == nil || *q },
		authState.authenticate,
	)
	statusServer.SetConfirmAuthFn(authState.confirm)
	statusServer.SetCancelAuthFn(authState.cancel)
	statusServer.SetTokenEnrollFn(tokenEnroll)

	go func() {
		if err := statusServer.Start(); err != nil {
			logger.Error("pending-enrollment status server failed", "error", err)
		}
	}()
	defer statusServer.Stop()

	logger.Info("waiting for enrollment via menu-bar UI",
		"socket", statusSocketPath,
		"hint", "open the Nexus Agent menu and sign in / paste a token")

	select {
	case <-ctx.Done():
		logger.Info("pending-enrollment mode: shutdown signal received before enrollment")
		return 0
	case <-enrolledCh:
		logger.Info("enrollment complete; exiting so service manager restarts the daemon with the full stack",
			"thing_id", enrollMgr.ThingID())
		time.Sleep(200 * time.Millisecond)
		return 0
	}
}

// warmBootstrap primes the agent-bootstrap cache. Tries synchronously
// once with a 10s timeout; on failure (network glitch, Hub slow, NE
// transient block) spawns a background goroutine that retries with
// 5s → 60s exponential backoff until the context is cancelled or a
// fetch succeeds. Without this retry the daemon stuck permanently
// on "Contacting the gateway…" after a single transient failure and
// required manual `launchctl bootout` + restart to recover.
func warmBootstrap(ctx context.Context, c *bootstrap.Client, logger *slog.Logger) {
	warmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := c.Get(warmCtx)
	if err == nil {
		logger.Info("agent-bootstrap warmed", "controlPlaneURL", info.ControlPlaneURL, "deviceAuthMode", info.DeviceAuthMode)
		return
	}
	logger.Warn("agent-bootstrap warm failed, will retry in background", "error", err)
	go func() {
		backoff := 5 * time.Second
		const maxBackoff = 60 * time.Second
		attempt := 1
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			attempt++
			rCtx, rCancel := context.WithTimeout(ctx, 10*time.Second)
			info, err := c.Get(rCtx)
			rCancel()
			if err == nil {
				logger.Info("agent-bootstrap warmed (background retry)",
					"controlPlaneURL", info.ControlPlaneURL,
					"deviceAuthMode", info.DeviceAuthMode,
					"attempt", attempt)
				return
			}
			logger.Warn("agent-bootstrap retry failed", "error", err, "attempt", attempt, "next_in_sec", int(backoff.Seconds()))
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}()
}

func buildResolveCpURL(bc *bootstrap.Client) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		info, err := bc.Get(ctx)
		if err != nil {
			return "", err
		}
		if info.ControlPlaneURL == "" {
			return "", fmt.Errorf("hub bootstrap returned empty controlPlaneURL")
		}
		return info.ControlPlaneURL, nil
	}
}

func buildDiagCollector(cfg *config.AgentConfig) *diagnostics.Collector {
	return &diagnostics.Collector{
		HubHTTPURL: cfg.HubHTTPURL,
		CertPath:   cfg.CertFile,
		LogFile:    cfg.Log.File,
		TailLines:  50,
	}
}

func writeDaemonPID(pidPath string, logger *slog.Logger) {
	if err := os.MkdirAll("/var/run/nexus-agent", 0755); err != nil {
		logger.Warn("self-intercept guard: mkdir failed", "error", err)
		return
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		logger.Warn("self-intercept guard: write daemon.pid failed", "path", pidPath, "error", err)
		return
	}
	logger.Info("self-intercept guard: wrote daemon PID for NE filter", "path", pidPath, "pid", os.Getpid())
}

func startQuitFlagWatcher(ctx context.Context, e *lifecycle.Emitter, cancel context.CancelFunc, recoveryCfg shareddiag.RecoveryConfig, logger *slog.Logger) {
	rcfg := recoveryCfg
	rcfg.Source = "user-quit-flag-watcher"
	defer shareddiag.Recover(rcfg, nil)
	flagPath := userQuitFlagPath()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(flagPath); err == nil {
				slog.Info("user-quit flag detected at runtime, initiating graceful shutdown", "path", flagPath)
				emitShutdownGracefully(e, "user_quit_flag")
				cancel()
				return
			}
		}
	}
}

func startConfigReloadGoroutine(configCh <-chan config.ConfigDiff) {
	// Drains config-diff notifications. Kept as a sink so subscribers
	// downstream of cfgMgr.Swap don't see channel back-pressure.
	for diff := range configCh {
		_ = diff
	}
}

func startDiagDedupGoroutine(ctx context.Context, tc *thingclient.Client, diagBundle wiring.DiagBundle, recoveryCfg shareddiag.RecoveryConfig, logger *slog.Logger) {
	rcfg := recoveryCfg
	rcfg.Source = "diag-dedup-tick"
	defer shareddiag.Recover(rcfg, nil)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	wsConnectedFn := func() bool { return tc != nil && tc.Mode() == thingclient.ModeWSConnected }
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			summaries := diagBundle.Dedup.Tick()
			for _, s := range summaries {
				if wsConnectedFn() && tc != nil {
					if err := tc.PushDiagEvent(ctx, s); err != nil {
						logger.Debug("dedup summary push failed", "error", err, "messageHash", s.MessageHash)
					}
				} else {
					diagBundle.ReconnectBuffer.Add(s)
				}
			}
		}
	}
}

func drainPendingDiagEvents(ctx context.Context, diagBundle wiring.DiagBundle, hubClient *hub.Client, cDir string, cfg *config.AgentConfig, thingID string, logger *slog.Logger) {
	pending, err := diagBundle.LocalBuffer.Pending()
	if err != nil {
		logger.Warn("diag pending count failed", "error", err)
		return
	}
	if pending == 0 {
		return
	}
	drainToken, _ := auth.LoadDeviceToken(cDir)
	if drainToken == "" {
		logger.Warn("diag drain skipped: no device token available", "pending", pending)
		return
	}
	drainCtx, drainCancel := context.WithTimeout(ctx, 30*time.Second)
	drainErr := diag.DrainPending(drainCtx, diag.DrainConfig{
		Buffer:      diagBundle.LocalBuffer,
		HTTPClient:  hubClient.HTTPClient(),
		HubURL:      cfg.HubHTTPURL,
		DeviceToken: drainToken,
		ThingID:     thingID,
		Log:         logger,
	})
	drainCancel()
	if drainErr != nil {
		logger.Warn("diag drain failed; will retry next startup", "error", drainErr, "pending", pending)
	} else {
		logger.Info("diag drain complete", "pending_before", pending)
	}
}

func startAuditPruneGoroutine(ctx context.Context, auditQueue *auditqueue.Queue, recoveryCfg shareddiag.RecoveryConfig, cfg *config.AgentConfig) {
	rcfg := recoveryCfg
	rcfg.Source = "audit-prune"
	defer shareddiag.Recover(rcfg, nil)
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	retention := time.Duration(cfg.AuditRetentionDays) * 24 * time.Hour
	if retention <= 0 {
		retention = 30 * 24 * time.Hour
	}
	runPrune := func() {
		if pruned, err := auditQueue.PruneSynced(retention); err != nil {
			slog.Error("audit_events prune failed", "error", err)
		} else if pruned > 0 {
			slog.Info("pruned synced audit_events rows", "count", pruned)
		}
		if pruned, err := auditQueue.PruneAuditLocal(retention); err != nil {
			slog.Error("audit_local prune failed", "error", err)
		} else if pruned > 0 {
			slog.Info("pruned audit_local rows", "count", pruned)
		}
		if pruned, err := auditQueue.PruneLifecycle(retention); err != nil {
			slog.Error("lifecycle_event prune failed", "error", err)
		} else if pruned > 0 {
			slog.Info("pruned lifecycle_event rows", "count", pruned)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runPrune()
		}
	}
}

func startLocalRollupGoroutine(ctx context.Context, localRollup *localrollup.Aggregator, recoveryCfg shareddiag.RecoveryConfig) {
	rcfg := recoveryCfg
	rcfg.Source = "local-rollup"
	defer shareddiag.Recover(rcfg, nil)
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	if err := localRollup.Tick(ctx); err != nil {
		slog.Warn("local rollup initial tick failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := localRollup.Tick(ctx); err != nil {
				slog.Warn("local rollup tick failed", "error", err)
			}
		}
	}
}

func startExemptionCleanupGoroutine(ctx context.Context, exemptionStore *exemption.Store, recoveryCfg shareddiag.RecoveryConfig) {
	rcfg := recoveryCfg
	rcfg.Source = "exemption-cleanup"
	defer shareddiag.Recover(rcfg, nil)
	exemptionStore.RunCleanupLoop(ctx, 5*time.Minute)
}

func startExemptionUploadGoroutine(ctx context.Context, exemptionStore *exemption.Store, hubClient *hub.Client, thingID string, recoveryCfg shareddiag.RecoveryConfig) {
	rcfg := recoveryCfg
	rcfg.Source = "exemption-upload"
	defer shareddiag.Recover(rcfg, nil)
	reported := make(map[string]bool)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, e := range exemptionStore.PendingAutoExemptions() {
				if reported[e.Host] {
					continue
				}
				upCtx, upCancel := context.WithTimeout(ctx, 10*time.Second)
				err := hubClient.UploadExemption(upCtx, hub.ExemptionUpload{
					ThingID:   thingID,
					Host:      e.Host,
					Reason:    e.Reason,
					ExpiresAt: e.ExpiresAt,
				})
				upCancel()
				if err != nil {
					slog.Warn("failed to upload exemption", "host", e.Host, "error", err)
				} else {
					reported[e.Host] = true
					slog.Info("exemption reported to hub", "host", e.Host)
				}
			}
		}
	}
}

func startAuditDrainSupervisor(
	ctx context.Context,
	auditQueue *auditqueue.Queue,
	drainWg *sync.WaitGroup,
	drainUpload func([]auditevent.Event) error,
	recoveryCfg shareddiag.RecoveryConfig,
	cfg *config.AgentConfig,
	logger *slog.Logger,
) {
	rcfg := recoveryCfg
	rcfg.Source = "audit-drain"
	defer shareddiag.Recover(rcfg, nil)
	defer drainWg.Done()
	_ = logger
	interval := time.Duration(cfg.AuditDrainIntervalSec) * time.Second
	auditQueue.DrainLoop(ctx, interval, cfg.AuditBatchSize, drainUpload)
}

// buildDrainUpload constructs the audit drain upload closure.
// Defined here to keep the drain logic co-located with the supervisor.
func buildDrainUpload(
	ctx context.Context,
	tc *thingclient.Client,
	hubClient *hub.Client,
	auditQueue *auditqueue.Queue,
	statusCollector *status.Collector,
	cfgMgr *config.Manager,
	thingID string,
	uploadGate *payloadcapture.Store,
	spillReader wiring.LocalSpillReader,
	spillUploader wiring.SpillS3Uploader,
	logger *slog.Logger,
) func([]auditevent.Event) error {
	const maxBatchPayloadBytes = 10 * 1024 * 1024
	uploadLevelFn := func() string { return cfgMgr.Get().TrafficUploadLevel }
	filterFn := func(events []auditevent.Event) []auditevent.Event {
		filtered := make([]auditevent.Event, 0, len(events))
		for _, e := range events {
			if wiring.ShouldUploadFlow(e, uploadLevelFn) {
				filtered = append(filtered, e)
			}
		}
		return filtered
	}
	return func(events []auditevent.Event) error {
		events = filterFn(events)
		if len(events) == 0 {
			return nil
		}
		batchCtx, span := otel.Tracer("nexus-agent").Start(ctx, "audit.upload_batch")
		span.SetAttributes(attribute.Int("nexus.event_count", len(events)))
		defer span.End()

		// Body UPLOAD gate (Hub payload_capture config): strip the body (inline
		// bytes + spill ref) from the wire copy for any direction the server
		// config doesn't want uploaded. The body always stays in the local
		// store — this governs only what ships to Hub. Applied before the S3
		// upload so a PUT is never spent on a body that won't ship.
		uploadReq, uploadResp := true, true
		if uploadGate != nil {
			gate := uploadGate.Get()
			uploadReq, uploadResp = gate.StoreRequestBody, gate.StoreResponseBody
		}
		events = wiring.GateBodyUpload(events, uploadReq, uploadResp)

		// Oversize bodies still slated for upload were spilled to the local
		// store at capture time; read them back and upload to S3 via the Hub
		// presign flow, swapping the wire ref. Fail-open: the local file stays
		// put, metadata still ships. Both wire branches (WS + HTTP) consume the
		// converted batch.
		events = wiring.UploadDrainSpills(batchCtx, events, spillReader, spillUploader, logger)

		if tc != nil {
			hubEvents := make([]map[string]any, len(events))
			for i, e := range events {
				hubEvents[i] = wiring.AuditEventToMap(e)
			}
			chunks := wiring.SplitByPayloadBudget(hubEvents, maxBatchPayloadBytes)
			for _, chunk := range chunks {
				data, err := json.Marshal(chunk)
				if err != nil {
					return fmt.Errorf("marshal audit events: %w", err)
				}
				// 2026-05-24: switched from UploadAuditWithRetry (POSTs to
				// /things/audit, cp-shape envelope, silently drops agent
				// PayloadRequest/Response fields → 100% body NULL in DB)
				// to UploadAgentAuditWithRetry (POSTs raw array to
				// /things/agent-audit handler that knows AgentAuditEvent).
				if _, err = tc.UploadAgentAuditWithRetry(batchCtx, data, 3); err != nil {
					return err
				}
			}
			statusCollector.SetLastSyncTime(time.Now())
			return nil
		}

		hubHTTPEvents := wiring.BuildHTTPAuditEvents(events)
		httpChunks := wiring.SplitHTTPByPayloadBudget(hubHTTPEvents, maxBatchPayloadBytes)
		for _, chunk := range httpChunks {
			if _, err := hubClient.UploadAudit(batchCtx, thingID, chunk); err != nil {
				return err
			}
		}
		statusCollector.SetLastSyncTime(time.Now())
		return nil
	}
}
