// Command control-plane starts the Nexus Gateway Control Plane — the Go
// admin API server. It serves admin CRUD, agent API, and BFF proxy to
// data-plane services.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/configdispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/wiring"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/infrastructure/infra"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/bootenv"
)

var buildVersion = "dev"

func main() { os.Exit(run()) }

func run() int {
	// Boot-time env: load repo-root .env so dev workflows don't have to
	// export each variable manually. systemd EnvironmentFile / docker
	// --env-file values still win (godotenv non-overload).
	_, _ = bootenv.LoadFromRepoRoot(slog.Default())
	configPath := flag.String("config", "control-plane.config.yaml", "config file path")
	flag.Parse()

	boot, err := wiring.InitBootstrap(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap failed: %v\n", err)
		return 1
	}
	cfg, logger := boot.Config, boot.Logger
	opsReg := wiring.InitOpsMetrics()

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()

	db, dbClose, err := wiring.InitDB(ctx, cfg, logger)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		return 1
	}
	defer dbClose()

	redisClient, redisClose, err := wiring.InitRedis(ctx, cfg, logger)
	if err != nil {
		slog.Error("failed to parse Redis URL", "error", err)
		return 1
	}
	defer redisClose()

	tp, tpClose, err := wiring.InitObservability(ctx, db, cfg, logger)
	if err != nil {
		slog.Warn("failed to initialize telemetry, continuing without tracing", "error", err)
	} else {
		defer tpClose()
	}

	cryptoResult, err := wiring.InitCrypto(cfg, logger)
	if err != nil {
		slog.Error("failed to initialize credential vault", "error", err)
		return 1
	}
	iamEngine := wiring.InitIAM(db, redisClient, logger)

	mqResult, err := wiring.InitMQ(cfg, logger)
	if err != nil {
		slog.Error("MQ init failed", "error", err)
		return 1
	}
	defer mqResult.Close()
	revResult := wiring.InitRevocation(db, mqResult.Producer, logger)
	cpThingID := wiring.DeriveThingID(cfg)
	adminJWTVerifier := wiring.InitJWT(ctx, cfg, mqResult.Consumer, cpThingID, logger)
	auditWriter := wiring.InitAuditWriter(mqResult.Producer, logger)

	// InitHub installs the diag sink and mutates slog.Default; re-assign
	// logger afterward so downstream subsystems get the wrapped logger.
	hubResult, err := wiring.InitHub(wiring.HubDeps{
		Cfg:              cfg,
		Logger:           logger,
		Ctx:              ctx,
		BuildVersion:     buildVersion,
		ProcessStartTime: boot.StartTime,
		OpsReg:           opsReg,
		MQProducer:       mqResult.Producer,
	})
	if err != nil {
		slog.Warn("hub init failed, continuing without Hub", "error", err)
	}
	logger = slog.Default()
	defer hubResult.Close() //nolint:errcheck
	if hubResult.ThingClient != nil {
		hubResult.ThingClient.OnConfigChanged(configdispatch.BuildConfigChangedCallback(
			logger, hubResult.ThingID, hubResult.ThingClient, tp, db, cfg, hubResult.ConfigKeyRecorder,
		))
	}

	e := wiring.InitEcho(logger)
	wiring.InitRuntimeIntrospect(e, cfg, db, hubResult.ThingID, buildVersion, hubResult.ConfigKeyRecorder, logger)

	_, err = wiring.InitRoutes(e, wiring.RoutesDeps{
		Cfg:               cfg,
		DB:                db,
		HubClient:         hubResult.HubClient,
		IAMEngine:         iamEngine,
		Vault:             cryptoResult.Vault,
		MultiVault:        cryptoResult.MultiVault,
		AuditWriter:       auditWriter,
		RevocationService: revResult.Service,
		RevocationStore:   revResult.Store,
		JWTVerifier:       adminJWTVerifier,
		RedisClient:       redisClient,
		Logger:            logger,
		Ctx:               ctx,
	})
	if err != nil {
		slog.Error("routes init failed", "error", err)
		return 1
	}
	readinessHandler := wiring.NewReadinessHandler(infra.Deps{
		DB:     db,
		Hub:    hubResult.HubClient,
		Audit:  auditWriter,
		Logger: logger,
	})
	wiring.InitReadiness(e, readinessHandler)

	authServerCloser, err := wiring.InitAuthServer(ctx, e, wiring.AuthServerDeps{
		Cfg:               cfg,
		DB:                db,
		HubClient:         hubResult.HubClient,
		IAMEngine:         iamEngine,
		RevocationService: revResult.Service,
		AuditWriter:       auditWriter,
		JWTVerifier:       adminJWTVerifier,
		Logger:            logger,
	})
	if err != nil {
		slog.Error("auth server init failed", "error", err)
		return 1
	}
	defer authServerCloser()

	wiring.InitReconciler(ctx, db, hubResult.HubClient, logger)
	return wiring.RunUntilSignal(ctx, ctxCancel, e, cfg, logger)
}

// defaultAdvertiseHost delegates to wiring.DefaultAdvertiseHost.
func defaultAdvertiseHost(configured string) string {
	return wiring.DefaultAdvertiseHost(configured)
}
