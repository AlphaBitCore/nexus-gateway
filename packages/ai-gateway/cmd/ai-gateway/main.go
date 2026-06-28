// Command ai-gateway is the Nexus AI Gateway entry point.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/cmd/ai-gateway/configdispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/cmd/ai-gateway/wiring"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/bootenv"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/profiling"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/runtimemem"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

var buildVersion = "dev"

func main() { os.Exit(run()) }

func run() int {
	_, _ = bootenv.LoadFromRepoRoot(slog.Default())
	runtimemem.AutoSetMemoryLimit(slog.Default())
	configPath := flag.String("config", "ai-gateway.config.yaml", "config file path")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	logger, err := logging.NewLogger(logging.Config{
		Level: cfg.Log.Level, Format: cfg.Log.Format,
		File: cfg.Log.File, StackOnError: cfg.Log.StackOnError,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	slog.SetDefault(logger)
	slog.Info("shared initialized", slog.Int("sharedHooks", len(builtins.Registry.All())))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d, cleanup, err := wiring.Boot(ctx, cfg, logger)
	if err != nil {
		slog.Error("boot failed", "error", err)
		return 1
	}
	defer cleanup()

	tcResult := wiring.InitThingClient(ctx, wiring.TCInitDeps{
		Cfg: cfg, DB: d.DB, CacheLayer: d.CacheLayer, CredManager: d.CredManager,
		GeminiMgrSet: d.GeminiMgrSet, HookConfigCache: d.HookConfigCache,
		Tp: d.Tp, ObsState: &d.ObsState,
		PayloadCapture: d.PayloadCapture, StreamingPolicy: d.StreamingPolicy,
		Reliability: d.Reliability, PolicyCache: d.PolicyCache,
		AiguardGetter: func() *aiguard.ConfigCache { return d.AiguardConfigCache },
		NormEngine:    d.NormEngine, PassthroughCache: d.PassthroughCache,
		AuditWriter: d.AuditWriter, ConfigKeyRecorder: d.ConfigKeyRecorder,
		OpsReg: d.OpsReg, ProcessStartTime: d.ProcessStartTime,
		MqProducer: d.MqProducer, Logger: logger, BuildVersion: buildVersion,
		CfgLoaderBuilder: func(agID string, outcomes *thingclient.OutcomeTracker) *cfgloader.Loader {
			return configdispatch.BuildConfigLoader(configdispatch.Deps{
				Logger: logger, ThingID: agID, Outcomes: outcomes,
				BootstrapConfig: cfg, DB: d.DB, CacheLayer: d.CacheLayer,
				CredManager: d.CredManager, GeminiCacheMgrSet: d.GeminiMgrSet,
				HookConfigCache: d.HookConfigCache, TelemetryProvider: d.Tp,
				ObservabilityState:  &d.ObsState,
				PayloadCaptureStore: d.PayloadCapture,
				Reliability:         d.Reliability, PolicyCache: d.PolicyCache,
				AIGuardConfigCache: func() *aiguard.ConfigCache { return d.AiguardConfigCache },
				NormEngine:         d.NormEngine, PassthroughCache: d.PassthroughCache,
				SemanticIndexLifecycle: d.Semantic.IndexLifecycle,
				FreshnessDetector:      d.Semantic.Detector,
				ResponseCache:          d.ResponseCache,
				OnModelsReloaded: func(models []store.Model) {
					if d.CapCache != nil {
						d.CapCache.Replace(capability.NewSnapshot(models))
					}
				},
			})
		},
	})
	agID, tcClient := tcResult.AgID, tcResult.Client
	if tcClient != nil {
		defer func() {
			sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tcClient.Close(sc)
		}()
	}

	logger = wiring.InitDiagSink(ctx, tcClient, tcResult, agID, buildVersion, logger, d.OpsReg)

	if err := d.HookConfigCache.Start(ctx); err != nil {
		slog.Warn("hook config cache start failed", "error", err)
	}

	aiguard.Register(prometheus.DefaultRegisterer)
	if d.DB != nil {
		d.AiguardConfigCache = aiguard.NewConfigCache(
			configstore.NewAIGuardStore(d.DB.Pool), 2*time.Minute, logger)
	} else {
		logger.Warn("database unavailable; AI Guard endpoints not mounted")
	}

	h := wiring.MountRoutes(http.NewServeMux(), tcClient, d, agID, buildVersion, cfg, logger)

	addr := cfg.Server.BindAddr()
	server := &http.Server{
		Addr: addr, Handler: h,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}
	// Opt-in profiler: SIGUSR1 dumps to NEXUS_PPROF_DIR / HTTP on NEXUS_PPROF_ADDR. No-op unless set.
	profiling.Start("nexus-ai-gateway")
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		slog.Info("ai-gateway starting", "addr", addr, "log_level", cfg.Log.Level)
		// The prior "chunked_async only" startup INFO log was
		// removed here. ai-gateway now honors all three streaming modes
		// (passthrough / chunked_async / buffer_full_block) via the
		// shared streampolicy.Store; the SSE handler dispatches per
		// request. Operators see the active mode via the
		// streaming_compliance shadow reload log line.
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		slog.Info("shutting down ai-gateway")
		sc, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(sc)
	})
	g.Go(func() error { return wiring.RunTicker(gctx, 5*time.Minute, d.RateLimiter.Cleanup) })

	if err := g.Wait(); err != nil {
		slog.Error("exit", "error", err)
	}
	slog.Info("ai-gateway stopped")
	return 0
}
