// Command nexus-hub starts the Nexus Hub — the platform operations center
// responsible for Thing lifecycle, config sync, scheduled jobs, MQ consumers,
// and the Hub HTTP/WebSocket API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/cmd/nexus-hub/wiring"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/bootenv"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
	sharedops "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/profiling"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/runtimemem"
)

var buildVersion = "dev"

func main() { os.Exit(run()) }

func run() int {
	_, _ = bootenv.LoadFromRepoRoot(slog.Default())
	runtimemem.AutoSetMemoryLimit(slog.Default())
	configPath := flag.String("config", "nexus-hub.config.yaml", "path to config file")
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
	// Opt-in profiler: SIGUSR1 dumps to NEXUS_PPROF_DIR / HTTP on NEXUS_PPROF_ADDR. No-op unless set.
	profiling.Start("nexus-hub")
	logger.Info("starting nexus-hub", "id", cfg.Hub.ID, "port", cfg.Server.Port,
		"version", buildVersion, "scheduler", cfg.Scheduler.Enabled)

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()

	dbPool, err := wiring.InitDB(ctx, cfg, logger)
	if err != nil {
		logger.Error("database init failed", "error", err)
		return 1
	}
	defer dbPool.Close()

	redisClient, err := wiring.InitRedis(ctx, cfg, logger)
	if err != nil {
		logger.Error("invalid redis URL", "error", err)
		return 1
	}
	mqRes, err := wiring.InitMQ(ctx, cfg, logger)
	if err != nil {
		logger.Error("MQ init failed", "error", err)
		return 1
	}
	defer wiring.CloseMQAndRedis(mqRes, redisClient)

	opsReg := sharedops.NewRegistry(prometheus.DefaultRegisterer)

	storageRes, err := wiring.InitStorage(ctx, cfg, dbPool, redisClient, logger)
	if err != nil {
		logger.Error("storage init failed", "error", err)
		return 1
	}

	// One-shot startup audit: WARN on any thing_config_template row whose
	// (type, key) isn't registered in configkey.ValidByThingType. Does
	// not fail boot — orphans may exist transiently during multi-PR
	// configuration refactors.
	wiring.RunConfigKeyAudit(ctx, dbPool, logger)
	consumerMgr := wiring.InitConsumerManager(cfg, dbPool, mqRes.Consumer, opsReg, logger)
	if consumerMgr != nil {
		consumerMgr.Start(ctx)
		defer consumerMgr.Stop()
	}
	identityRes, err := wiring.InitIdentity(ctx, cfg, storageRes.Store, logger)
	if err != nil {
		logger.Error("identity init failed", "error", err)
		return 1
	}
	if identityRes.JWKSCache != nil {
		defer identityRes.JWKSCache.Close()
	}

	tmRes := wiring.InitFleet(cfg, storageRes.Store, redisClient, mqRes.Producer, opsReg, logger)
	opsRes := wiring.InitOpsMetrics(dbPool, opsReg, tmRes.WSServer, logger)
	logger = wiring.InitDiagSink(cfg, opsRes, opsReg, buildVersion, logger)
	wiring.StartWSSignalSubscriber(ctx, cfg.Hub.ID, mqRes.Consumer, tmRes.WSPool, cfg.Auth.SignalSecret, logger)

	selfReg, err := wiring.InitSelfReg(ctx, cfg, buildVersion, storageRes.Store, logger)
	if err != nil {
		logger.Error("hub self-registration failed", "error", err)
		return 1
	}
	otelRes := wiring.InitOTEL(ctx, cfg, logger)
	if otelRes.Provider != nil {
		defer otelRes.Provider.Shutdown(context.Background()) //nolint:errcheck
	}
	selfShadowRes, _ := wiring.InitSelfShadow(ctx, cfg, dbPool, storageRes.Store, otelRes, logger)
	defer func() { _ = selfShadowRes.Manager.Stop(context.Background()) }()
	wiring.InitSelfInstrumentation(ctx, cfg, buildVersion, time.Now().UTC(), dbPool, opsReg, opsRes, logger)

	alertsRes, err := wiring.InitAlerts(dbPool, cfg.Auth.CredentialMasterKey, logger)
	if err != nil {
		logger.Error("alerts init failed", "error", err)
		return 1
	}
	siemBridge := wiring.InitSIEMBridge(ctx, dbPool, logger)
	sched, err := wiring.InitScheduler(ctx, cfg, dbPool, redisClient, mqRes.Consumer, mqRes.Producer,
		storageRes.Store, tmRes.Mgr, storageRes.SpillStore, opsReg, alertsRes.Store, alertsRes.Raiser, siemBridge, logger)
	if err != nil {
		logger.Error("scheduler init failed", "error", err)
		return 1
	}
	if sched != nil {
		defer sched.Stop()
	}

	ec := wiring.BuildEchoConfig(cfg, buildVersion, dbPool, redisClient, mqRes, storageRes,
		consumerMgr, identityRes, tmRes, alertsRes, selfShadowRes, wiring.InitNormalizeRegistry(), sched, logger)
	e := wiring.InitEcho(ec)
	enrollAPI := wiring.MountRoutes(e, ec)
	go func() {
		addr := cfg.Server.BindAddr()
		logger.Info("http server starting", "addr", addr)
		if err := e.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "error", err)
			ctxCancel()
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logger.Info("shutdown signal received", "signal", sig.String())
	ctxCancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer shutdownCancel()
	wiring.GracefulShutdown(shutdownCtx, e, selfReg, tmRes.WSServer, consumerMgr, enrollAPI, opsRes, logger)
	return 0
}
