package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
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

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/attestation"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/auth"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/bootstrap"
	lifecycle "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/state"
	auditevent "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/backpressure"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/diagnostics"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/localrollup"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
	config "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/schema"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/status"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	metricsplatform "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/cmd/agent/platformshim"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/cmd/agent/wiring"
)

// cmdRun drives the enrolled daemon's full lifecycle: flag parsing, the
// ordered subsystem constructor calls (one wiring call per subsystem — the
// boot ORDER below is load-bearing), and the shutdown sequencing. Subsystem
// construction lives in cmd/agent/wiring; per-key shadow config plumbing in
// configappliers.go / configdispatch.go / configcache.go; IPC command wiring
// in status_ipc.go.
func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "agent.yaml", "path to agent config file")
	_ = fs.Parse(args)

	cfg, err := config.LoadFromFile(*configPath)
	if err != nil {
		// Config can't load → the quitAllowed policy is unknowable. Preserve the
		// historical fast-exit when the user-quit flag is present so a quit +
		// broken-config combination does not respawn-loop under launchd; a
		// broken-config daemon cannot enforce compliance anyway, and editing the
		// root-owned config file requires privilege an unprivileged bypass lacks.
		if _, statErr := os.Stat(userQuitFlagPath()); statErr == nil {
			fmt.Fprintf(os.Stderr, "nexus-agent: user-quit flag present and config load failed — exiting\n")
			return 0
		}
		slog.Error("failed to load config", "error", err)
		return 1
	}

	// User-controlled lifecycle gate. When the Swift menu-bar app's Quit
	// handler writes the user-quit flag, every subsequent launchd respawn
	// of this daemon exits immediately and stays dead until the user
	// re-launches NexusAgent.app (which clears the flag). The check runs AFTER
	// config load so it can honor the quitAllowed policy: on a quitAllowed=false
	// fleet the flag is IGNORED, otherwise any local user able to create the
	// world-readable flag file would defeat the no-quit policy — the same bypass
	// the IPC SHUTDOWN gate already prevents.
	startupQuitAllowed := cfg.QuitAllowed == nil || *cfg.QuitAllowed
	if userQuitFlagShouldExit(userQuitFlagPath(), startupQuitAllowed) {
		fmt.Fprintf(os.Stderr, "nexus-agent: user-quit flag present at %s — exiting (remove the flag or re-launch the menu-bar app to bring the daemon back)\n", userQuitFlagPath())
		return 0
	}

	logger, err := wiring.InitLogger(cfg.Log.Level, cfg.Log.Format, cfg.Log.File, cfg.Log.StackOnError)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}

	// macOS: flush mDNSResponder on every daemon startup so launchd
	// respawns, OS reboots, and manual restarts all leave the user's
	// getaddrinfo cache fresh. Combined with the shutdown-side flush
	// (graceful shutdown path), the install/uninstall script flushes,
	// and the menu-bar Quit flush, this covers every "daemon comes up"
	// path so the user never has to type `dscacheutil -flushcache`
	// manually. See memory: feedback_macos_mdns_flush_after_ne_state_change.
	flushMDNSResponderIfDarwin()

	// Check crash loop before full startup.
	wiring.InitCrashLoopGuard(os.Args[0], cfg.AuditDBPath)

	// Ops metrics registry (L3 business + Prometheus).
	opsReg, processStartTime := wiring.InitOpsMetrics()

	// Hub HTTP client. enrollMgrRef is filled below; the closure captures
	// the pointer so it sees the updated value rather than nil at construction time.
	cDir := certDir(cfg)
	var enrollMgrRef *enrollment.Manager
	hubClient, err := wiring.InitHubClient(wiring.HubClientConfig{
		HubHTTPURL:   cfg.HubHTTPURL,
		CertFile:     cfg.CertFile,
		KeyFile:      cfg.KeyFile,
		CACertFile:   cfg.EffectiveHubCA(),
		DeviceCAFile: filepath.Join(paths.DefaultPaths().StateDir, "device-ca.pem"),
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

	// Enrollment manager (device-token renewal rides hubClient's HTTP path).
	// The ONE place the run command constructs the platform keystore (macOS
	// Keychain / Windows DPAPI / Linux file). Everything downstream takes it
	// as a parameter so tests can inject a memory store — wiring code opening
	// the real Keychain under `go test` triggers OS permission prompts
	// (enforced by scripts/check-keystore-seam.sh).
	platformKeystore := keystore.NewPlatformStore()

	enrollMgr, hubEnroller, err := wiring.InitEnrollment(cfg.HubHTTPURL, cfg.EffectiveHubCA(), cDir, platformKeystore, hubClient)
	if err != nil {
		slog.Error("failed to init hub enroll client", "error", err)
		return 1
	}
	enrollMgrRef = enrollMgr

	if !enrollMgr.IsEnrolled() {
		runCtx, runCancel := context.WithCancel(context.Background())
		defer runCancel()
		return runPendingEnrollment(runCtx, cfg, logger, enrollMgr, hubEnroller)
	}
	thingID := enrollMgr.ThingID()
	slog.Info("device enrolled", "thing_id", thingID)

	cfgMgr := config.NewManager(cfg)
	defer cfgMgr.Close()

	// forward-declared for the agent_settings shadow adapter.
	var statusCollector *status.Collector

	// Hub thingclient (WebSocket primary; nil keeps HTTP audit upload only).
	tc := wiring.InitThingClientFromStore(wiring.ThingClientConfig{
		HubURL:           cfg.HubURL,
		HubHTTPURL:       cfg.HubHTTPURL,
		ThingID:          thingID,
		Version:          version,
		Logger:           logger,
		ProcessStart:     processStartTime,
		OpsReg:           opsReg,
		ComposeVersionFn: platformshim.ComposeThingVersion,
	}, cDir)

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
	// the UpstreamTransport request injector. Reads the Ed25519 private
	// key from the platform keystore (macOS Keychain / Windows
	// DPAPI / Linux 0600 file, written there by enrollment), not a
	// plaintext PEM on disk. If the key is absent, InjectInto returns a nil
	// header (fail-open) and the request flows unattested.
	attestationSigner := attestation.NewSigner(
		platformKeystore,
		keystore.AttestationKeyName,
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
	wiring.RegisterConfigIntrospection(introspectReg, appliers.killSwitchObj, comp.PayloadCaptureStore)

	// Wire Hub config-changed callback onto thingclient.
	wiring.WireConfigChanged(tc, cfgLoader, logger)

	// Config reload goroutine (must start before statusCollector is used).
	configCh := cfgMgr.Subscribe()
	go startConfigReloadGoroutine(configCh)

	// Audit queue (SQLCipher-encrypted).
	auditQueue, err := wiring.InitAuditQueue(cfg.AuditDBPath, platformKeystore, logger)
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

	// Offline config cache: opened on the audit queue's SQLCipher DB and
	// replayed now so enforcement comes up on last-known policy immediately.
	// Placed after statusCollector so a restored agent_settings entry can
	// reach every subsystem it touches (the applier reads statusCollector).
	openAndRestoreConfigCache(ctx, auditQueue.DB(), &configCache, cfgRestoreMap, logger)

	staticInfo := metricsplatform.CaptureStaticInfo(metricsplatform.BuildInfo{
		ServiceVersion: "nexus-agent/" + version,
		StartTime:      processStartTime.Format(time.RFC3339),
	})

	// Thingclient callbacks (disconnect/reconnect/heartbeat).
	wiring.WireThingClientCallbacks(tc, staticInfo, statusCollector, diagBundle, logger)

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
	spillUploader, spillReader := wiring.InitSpillTransport(hubClient, platformKeystore, logger)

	// Drain upload logic. comp.PayloadCaptureStore (Hub-pushed) is the body
	// UPLOAD gate: when its StoreRequest/ResponseBody flags are off the drain
	// ships metadata only and the body stays local.
	drainUpload := buildDrainUpload(ctx, tc, hubClient, auditQueue, statusCollector, cfgMgr, thingID, comp.PayloadCaptureStore, spillReader, spillUploader, logger)

	// Audit drain supervisor.
	var drainWg sync.WaitGroup
	drainWg.Add(1)
	go startAuditDrainSupervisor(ctx, auditQueue, &drainWg, drainUpload, recoveryCfg, cfg)

	// Audit prune goroutine.
	go startAuditPruneGoroutine(ctx, auditQueue, recoveryCfg, cfg)

	// Local rollup goroutine.
	localRollup := wiring.InitLocalRollup(auditQueue, logger)
	go startLocalRollupGoroutine(ctx, localRollup, recoveryCfg)

	// Exemption cleanup + upload goroutines.
	go startExemptionCleanupGoroutine(ctx, comp.ExemptionStore, recoveryCfg)
	go startExemptionUploadGoroutine(ctx, comp.ExemptionStore, hubClient, thingID, recoveryCfg)

	// Device-token renewal goroutine: rotates the bearer token before
	// its bounded TTL lapses so a healthy agent never runs an expired token, and
	// each rotation invalidates the prior token server-side.
	go startDeviceTokenRenewalGoroutine(ctx, enrollMgr, recoveryCfg)

	// Auto-updater.
	// dataDir is the agent's persistent state directory (AuditDBPath parent),
	// used to persist the version floor file (updater-floor.json) across restarts.
	updaterDataDir := filepath.Dir(cfg.AuditDBPath)
	up := wiring.InitUpdater(hubClient, cfg.UpdaterEnabled, cfg.UpdaterCheckSec, version, runtime.GOOS, os.Args[0], updaterDataDir)
	wiring.StartUpdater(ctx, up, recoveryCfg, statusCollector.SetUpdateAvailable)

	// Drain pending crash DiagEvents to Hub before opening the WebSocket.
	drainPendingDiagEvents(ctx, diagBundle, hubClient, cDir, cfg, thingID, logger)

	// Start thingclient (nil after a failed start → HTTP audit upload fallback).
	var tcClose func()
	tc, tcClose = wiring.StartThingClient(ctx, tc, thingID, staticInfo, cfgLoader, recoveryCfg, logger)
	if tcClose != nil {
		defer tcClose()
	}

	// Lifecycle emitter + kill switch + pauser.
	lifecycleEmitter := wiring.InitLifecycleEmitter(tc, auditQueue, wiring.LifecycleEmitterConfig{
		ThingID:      thingID,
		AgentVersion: version,
		Logger:       logger,
	})

	// SSO auth state for IPC.
	authState := wiring.InitSSOAuth(wiring.SSOAuthConfig{
		HubEnroller:  hubEnroller,
		Manager:      enrollMgr,
		Bootstrap:    bootstrapClient,
		OSVersion:    osVersion(),
		AgentVersion: version,
	})

	// Status server.
	statusSocketPath := guiSocketPath()
	statusServer := wiring.InitStatusServer(wiring.StatusServerDeps{
		SocketPath:  statusSocketPath,
		Collector:   statusCollector,
		HubClient:   hubClient,
		Ctx:         ctx,
		Cancel:      cancel,
		Version:     version,
		Emitter:     lifecycleEmitter,
		AuditQueue:  auditQueue,
		ConfigMgr:   cfgMgr,
		Auth:        authState,
		SpillReader: spillReader,
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
	wiring.StartStatusAPI(statusServer, bootstrapClient, introspectReg, recoveryCfg)
	defer statusServer.Stop()

	slog.Info("agent running", "thing_id", thingID, "heartbeat", cfg.HeartbeatIntervalSec,
		"drain", cfg.AuditDrainIntervalSec, "statusAPI", statusSocketPath)

	// Self-intercept guard: write daemon PID so the NE filter can pass through own-process traffic.
	pidPath := "/var/run/nexus-agent/daemon.pid"
	writeDaemonPID(pidPath, logger)
	lifecycleEmitter.Startup()

	// Platform + connection bridge.
	plat := wiring.InitPlatform(cfg.PlatformBridgeAddress)

	// Interception-mode publication + darwin backpressure + health +
	// diagnostics IPC.
	wirePlatformReporting(plat, backpressureStore, statusCollector, statusServer, cfg)

	// Build the shared Tier 1+2+3 normalize Registry once (shared with
	// Hub agent_audit / ai-gateway / compliance-proxy).
	normalizeRegistry := wiring.InitNormalizeRegistry()

	// Linux/Windows: wire the shared/tlsbump bridge deps onto the platform
	// BEFORE Start launches the accept loop (no-op on macOS, which wires its
	// own deps in WireDarwinBridge below).
	wiring.WireInspectBridge(plat, wiring.BridgeDepsArgs{
		Keystore:      platformKeystore,
		Logger:        logger,
		AgentPipeline: comp.AgentPipeline,
		// Local capture store (always-on by default) gates body capture in
		// tlsbump — independent of the Hub upload config.
		PayloadCaptureStore:  comp.LocalCaptureStore,
		AuditQueue:           auditQueue,
		StreamingPolicyStore: comp.StreamingPolicyStore,
		NormalizeRegistry:    normalizeRegistry,
		AttestationSigner:    attestationSigner,
		UpstreamProxy:        cfg.UpstreamProxy,
	})

	wiring.StartPlatformInterception(ctx, plat, connHandler, recoveryCfg)
	defer plat.Stop() //nolint:errcheck

	bridgeCloser := platformshim.WireDarwinBridge(context.Background(), plat, platformshim.DarwinBridgeArgs{
		Keystore:          platformKeystore,
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

	// User-quit flag watcher goroutine. Gated on the live quitAllowed policy so a
	// quitAllowed=false fleet ignores a flag any local user could plant.
	go startQuitFlagWatcher(ctx, lifecycleEmitter, cancel,
		func() bool { q := cfgMgr.Get().QuitAllowed; return q == nil || *q },
		recoveryCfg, logger)

	// Wait for shutdown.
	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig)
		wiring.EmitShutdownGracefully(lifecycleEmitter, fmt.Sprintf("signal:%v", sig))
		cancel()
	case <-ctx.Done():
		if _, err := os.Stat(userQuitFlagPath()); err != nil {
			wiring.EmitShutdownGracefully(lifecycleEmitter, "ctx_done")
		}
	}

	// Graceful shutdown: wait for audit drain (max 10s).
	wiring.WaitForAuditDrain(&drainWg, recoveryCfg)

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

// wirePlatformReporting publishes the platform's interception mode, wires the
// darwin NE backpressure shim, surfaces interception health on the status
// collector, and installs the diagnostics IPC command. Lives in package main
// (not wiring) because the darwin backpressure shim comes from platformshim,
// which itself imports the wiring package.
func wirePlatformReporting(
	plat api.Platform,
	backpressureStore *backpressure.Store,
	statusCollector *status.Collector,
	statusServer *status.Server,
	cfg *config.AgentConfig,
) {
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
				SelfReported:     h.SelfReported,
				DegradedReason:   h.DegradedReason,
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
}

// runPendingEnrollment is the cold-boot path: the daemon's `run` command
// starts here when no device certificate exists on disk.
func runPendingEnrollment(
	ctx context.Context,
	cfg *config.AgentConfig,
	logger *slog.Logger,
	enrollMgr *enrollment.Manager,
	hubEnroller *enrollment.HubEnrollClient,
) int {
	logger.Info("agent starting in pending-enrollment mode (no device cert on disk)")
	bootstrapClient := bootstrap.New(cfg.HubHTTPURL, bootstrap.DefaultHTTPClient(), cfg.CpURL)
	warmBootstrap(ctx, bootstrapClient, logger)
	hostname, _ := os.Hostname()

	enrolledCh := make(chan struct{})
	var enrolledOnce sync.Once
	signalEnrolled := func() { enrolledOnce.Do(func() { close(enrolledCh) }) }

	authState := wiring.InitSSOAuth(wiring.SSOAuthConfig{
		HubEnroller:  hubEnroller,
		Manager:      enrollMgr,
		Bootstrap:    bootstrapClient,
		OSVersion:    osVersion(),
		AgentVersion: version,
		OnSuccess:    signalEnrolled,
	})
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
		authState.Authenticate,
	)
	statusServer.SetConfirmAuthFn(authState.Confirm)
	statusServer.SetCancelAuthFn(authState.Cancel)
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

// userQuitFlagShouldExit reports whether the presence of the user-quit flag
// must stop the daemon. The flag is a console-user convenience for quitting the
// agent; on a quitAllowed=false (locked / compliance) fleet it MUST be ignored.
// Without this gate any local user who can create the flag file — the flag
// directory is world-writable so the menu-bar app can write it across UIDs —
// would defeat the no-quit policy, exactly the bypass the IPC SHUTDOWN gate
// prevents. When quitAllowed is false the flag is not even stat'd.
func userQuitFlagShouldExit(flagPath string, quitAllowed bool) bool {
	if !quitAllowed {
		return false
	}
	_, err := os.Stat(flagPath)
	return err == nil
}

func startQuitFlagWatcher(ctx context.Context, e *lifecycle.Emitter, cancel context.CancelFunc, quitAllowedFn func() bool, recoveryCfg shareddiag.RecoveryConfig, logger *slog.Logger) {
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
			if userQuitFlagShouldExit(flagPath, quitAllowedFn()) {
				slog.Info("user-quit flag detected at runtime, initiating graceful shutdown", "path", flagPath)
				wiring.EmitShutdownGracefully(e, "user_quit_flag")
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

// startDeviceTokenRenewalGoroutine periodically checks whether the device bearer
// token is within its renewal window and, if so, rotates it. The check
// is a cheap on-disk read; the hourly cadence against a multi-day renewal window
// gives ample retries before the token's TTL could lapse. Rotation goes through
// the HTTP path (hubClient), which reads the token fresh from disk on every
// call, so all subsequent HTTP calls transparently use the rotated token.
func startDeviceTokenRenewalGoroutine(ctx context.Context, enrollMgr *enrollment.Manager, recoveryCfg shareddiag.RecoveryConfig) {
	rcfg := recoveryCfg
	rcfg.Source = "device-token-renewal"
	defer shareddiag.Recover(rcfg, nil)
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	renewIfNeeded := func() {
		if !enrollMgr.DeviceTokenNeedsRenewal(time.Now()) {
			return
		}
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := enrollMgr.RenewDeviceToken(rctx); err != nil {
			slog.Warn("device token renewal failed", "error", err)
			return
		}
		slog.Info("device token renewed")
	}
	// Renew opportunistically at startup so a token that lapsed while the agent
	// was off rotates immediately rather than waiting a full tick.
	renewIfNeeded()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewIfNeeded()
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
) {
	rcfg := recoveryCfg
	rcfg.Source = "audit-drain"
	defer shareddiag.Recover(rcfg, nil)
	defer drainWg.Done()
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
