package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/breakglass"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/configdispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	proxyserver "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/server"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/bootenv"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/profiling"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/runtimemem"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/wiring"
)

var buildVersion = "dev"

func main() { os.Exit(run()) }

// composeMetricsURL / composeManagementBaseURL are retained as package-level
// shims so main_test.go (package main) can continue to test the logic without
// importing the wiring package.
func composeMetricsURL(advertiseHost, bindAddr string) string {
	return wiring.ComposeMetricsURL(advertiseHost, bindAddr)
}
func composeManagementBaseURL(advertiseHost, bindAddr string) string {
	return wiring.ComposeManagementBaseURL(advertiseHost, bindAddr)
}

func run() int {
	_, _ = bootenv.LoadFromRepoRoot(slog.Default())
	runtimemem.AutoSetMemoryLimit(slog.Default())

	configPath := flag.String("config", "compliance-proxy.config.yaml", "path to configuration file")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
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
	// Opt-in profiler: SIGUSR1 dumps to NEXUS_PPROF_DIR / HTTP on NEXUS_PPROF_ADDR. No-op unless set.
	profiling.Start("nexus-compliance-proxy")
	slog.Info("nexus compliance-proxy starting", "config", *configPath,
		"listenAddr", cfg.Listener.Address, "metricsAddr", cfg.Metrics.Address)

	opsResult := wiring.InitOpsRegistry()
	redisClient := wiring.InitRedis(cfg, logger)
	certResult, err := wiring.InitCertIssuer(cfg, redisClient, logger)
	if err != nil {
		slog.Error("failed to initialize cert issuer", "error", err)
		return 1
	}

	infra, err := wiring.InitInfra(cfg, logger)
	if err != nil {
		slog.Error("failed to initialize infra", "error", err)
		return 1
	}
	accessChecker := infra.AccessChecker
	connManager := infra.ConnManager
	shutdownCoord := infra.ShutdownCoord
	upstreamTransport := infra.UpstreamTransport

	mqProducer, err := wiring.InitMQProducer(cfg, logger)
	if err != nil {
		slog.Error("MQ producer init failed", "error", err)
		return 1
	}
	if mqProducer != nil {
		defer mqProducer.Close() //nolint:errcheck
	}

	auditRes, err := wiring.InitAudit(cfg, mqProducer, logger)
	if err != nil {
		slog.Error("failed to initialize audit subsystem", "error", err)
		return 1
	}

	cacheManager := cache.NewManager(5*time.Minute, logger)
	compRes, err := wiring.InitCompliance(cfg, cacheManager, auditRes.Writer, logger)
	if err != nil {
		slog.Error("failed to initialize compliance kernel", "error", err)
		return 1
	}

	domainEngine := domain.NewEngine()
	wiring.RegisterCacheLoaders(compRes.ConfigDB, cacheManager, domainEngine, accessChecker, logger)

	payloadCaptureStore := wiring.InitPayloadCaptureStore(compRes.ConfigDB, compRes.Emitter, logger)
	streamingPolicyStore := wiring.InitStreamingPolicyStore(compRes.ConfigDB, logger)

	killSwitch := killswitch.NewKillSwitch(logger)
	killSwitch.SetForceCloseFunc(func() int {
		n := int(connManager.ActiveCount())
		shutdownCoord.Shutdown() //nolint:errcheck
		return n
	})
	exemptionStore := wiring.InitExemptionStore(logger)

	hostname, _ := os.Hostname()
	proxyID := fmt.Sprintf("proxy-%s", hostname)
	if cfg.ID != "" {
		proxyID = cfg.ID
	}
	normalizeRegistry := wiring.WireNormalizer(auditRes.Writer, proxyID, hostname)

	proxyServer := wiring.InitProxyServerFull(wiring.ProxyServerDeps{
		Cfg: cfg, Logger: logger, AccessChecker: accessChecker,
		ConnManager: connManager, ShutdownCoord: shutdownCoord,
		UpstreamTransport: upstreamTransport, CertResult: certResult,
		CompRes: compRes, DomainEngine: domainEngine,
		AdapterRegistry:   infra.AdapterRegistry,
		NormalizeRegistry: normalizeRegistry,
		KillSwitch:        killSwitch, ExemptionStore: exemptionStore,
		PayloadCaptureStore: payloadCaptureStore, StreamingPolicyStore: streamingPolicyStore,
	})

	otelCfg := wiring.LoadOtelConfig(context.Background(), compRes.ConfigDB)
	tp, err := telemetry.Init(context.Background(), otelCfg, slog.Default())
	if err != nil {
		slog.Warn("OpenTelemetry init failed", "error", err)
	} else {
		defer tp.Shutdown(context.Background()) //nolint:errcheck
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Arm the hook-config TTL-backstop ticker (wiring already did the
	// fail-hard initial load; this re-load is harmless and the ticker
	// covers a degraded Hub push channel for the process lifetime).
	if err := compRes.HookConfigCache.Start(ctx); err != nil {
		logger.Warn("hook config cache start failed", "error", err)
	}

	configKeyRecorder := runtimeintrospect.NewKeyStateRecorder()
	readiness := &atomic.Bool{}
	readiness.Store(true)

	baseDeps := configdispatch.Deps{
		Logger: logger, ThingID: proxyID, KillSwitch: killSwitch,
		ExemptionStore: exemptionStore, HookConfigCache: compRes.HookConfigCache,
		ConfigDB: compRes.ConfigDB, CacheManager: cacheManager,
		AccessChecker: accessChecker, TelemetryProvider: tp,
		PayloadCaptureStore:  payloadCaptureStore,
		StreamingPolicyStore: streamingPolicyStore,
		ProxyServer:          proxyServer,
	}
	hubRes := configdispatch.InitHubAndCfgLoader(ctx, baseDeps,
		func(apply func(map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error)) *thingclient.Client {
			return wiring.InitThingClientSimple(ctx, wiring.ThingClientDeps{
				Cfg: cfg, ProxyID: proxyID, BuildVersion: buildVersion, Logger: logger,
				MQProducer: mqProducer, OpsRegistry: opsResult.Registry,
				ProcessStartTime: opsResult.ProcessStartTime, ConfigKeyRecorder: configKeyRecorder,
				OnConfigChanged: apply,
			})
		},
	)
	tcRes := wiring.ThingClientResult{}
	var thingClient *thingclient.Client
	if hubRes.ThingClient != nil {
		thingClient = hubRes.ThingClient
		tcRes = wiring.CaptureThingClientResult(thingClient, cfg, opsResult.ProcessStartTime, logger)
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = thingClient.Close(shutdownCtx)
		}()
	}

	exemptionStore.StartCleanup(ctx, 60*time.Second)

	srvs := wiring.InitServers(wiring.ServersDeps{
		Cfg: cfg, Logger: logger, Readiness: readiness, KillSwitch: killSwitch,
		ConnManager: connManager, StartTime: time.Now().UTC(), RedisClient: redisClient,
		ExemptionStore: exemptionStore, ThingClient: thingClient,
		ProxyID: proxyID, BuildVersion: buildVersion, CertResult: certResult,
		CacheManager: cacheManager, DomainEngine: domainEngine, CompRes: compRes,
		PayloadCapture: payloadCaptureStore, ConfigKeyRecorder: configKeyRecorder,
		ServiceToken: cfg.Auth.InternalServiceToken,
	})

	go func() {
		slog.Info("health/metrics server starting", "addr", cfg.Metrics.Address)
		if err := srvs.HealthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("health server error", "error", err)
		}
	}()
	go func() {
		if err := srvs.RuntimeSrv.Start(ctx); err != nil {
			slog.Error("runtime API server error", "error", err)
		}
	}()
	go breakglass.RunReplay(ctx, srvs.RuntimeSrv, logger)

	diagRes := wiring.InitDiagSink(logger, thingClient, proxyID, opsResult.Registry)
	wiring.WireOnReconnect(wiring.ReconnectDeps{
		ThingClient: thingClient, StaticInfo: tcRes.StaticInfo,
		StaticInfoReady: tcRes.StaticInfoReady, RuntimeServer: srvs.RuntimeSrv,
		ReconnectBuffer: diagRes.ReconnectBuffer, Logger: diagRes.Logger,
	})
	wiring.PushStartupDiagEvent(thingClient, proxyID, buildVersion)

	proxyErrCh := make(chan error, 1)
	go func() {
		slog.Info("proxy server starting", "addr", cfg.Listener.Address)
		if err := proxyserver.Start(ctx, cfg.Listener.Address, proxyServer); err != nil {
			proxyErrCh <- err
		}
		close(proxyErrCh)
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections...")
	case err := <-proxyErrCh:
		if err != nil {
			slog.Error("proxy server failed", "error", err)
			return 1
		}
	}

	wiring.RunShutdown(wiring.ShutdownDeps{
		Readiness: readiness, ShutdownCoord: shutdownCoord,
		RuntimeServer: srvs.RuntimeSrv, HealthServer: srvs.HealthServer,
		AuditWriter: auditRes.Writer,
		RedisClient: redisClient,
	})
	slog.Info("nexus compliance-proxy stopped")
	return 0
}
