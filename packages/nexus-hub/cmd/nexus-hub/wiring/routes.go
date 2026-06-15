package wiring

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	emw "github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
	fleetmgr "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/handler/enroll"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/scheduler"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jwks"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/consumer"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/ws"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillupload"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// EchoConfig carries all the wired subsystems the Echo setup needs.
type EchoConfig struct {
	Cfg          *config.HubConfig
	BuildVersion string
	DBPool       *pgxpool.Pool
	RedisClient  redis.UniversalClient
	ConsumerMgr  *consumer.Manager
	Mgr          *fleetmgr.Manager
	WSServer     *ws.Server
	Sched        *scheduler.Scheduler
	EnrollSvc    *enrollment.Service
	MQProducer   mq.Producer
	Store        *store.Store
	AgentCA      *agentca.CA
	JWKSCache    *jwks.Cache
	AlertStore   *alerting.Store
	AlertRaiser  *alerting.Raiser
	AlertsRes    AlertsResult
	CatBRegistry *store.CatBRegistry
	SpillStore   spillstore.SpillStore
	SpillSecrets *spillupload.SecretStore
	SpillDedup   spillupload.Dedup
	NormalizeFn  normalizecore.AuditFn
	SelfShadow   SelfShadowResult
	Logger       *slog.Logger
}

// InitEcho constructs the Echo instance, mounts middleware, health endpoints,
// metrics, and the runtime introspect handler. Returns the *echo.Echo instance.
// The caller is responsible for calling e.Shutdown on graceful stop.
func InitEcho(ec EchoConfig) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// Trust-proxy config: only honor X-Forwarded-For / X-Real-IP when the
	// immediate peer is a loopback / private-net address (nginx on 127.0.0.1,
	// or an in-cluster k8s ingress). Without this, a compromised client could
	// spoof X-Forwarded-For to poison user-identity attribution.
	// ExtractIPFromXFFHeader walks XFF right-to-left, skipping trusted-proxy
	// hops, so the returned IP is the leftmost untrusted peer.
	e.IPExtractor = echo.ExtractIPFromXFFHeader(
		echo.TrustLoopback(true),
		echo.TrustLinkLocal(false),
		echo.TrustPrivateNet(true),
	)

	e.Use(emw.Recover())
	// NexusRequestID seeds the request context with the inbound x-nexus-request-id
	// (or a freshly minted UUID), so httpclient.Client propagates it forward when
	// PropagateReqID=true. Replaces echo's emw.RequestID() which only set the header.
	e.Use(handler.NexusRequestID())

	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	e.GET("/readyz", readyzHandler(ec.DBPool, ec.RedisClient, ec.ConsumerMgr))
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))

	// Runtime introspection (e31-s7): Hub self-hosts /debug/runtime.
	introspectReg := buildIntrospectReg(ec)
	e.GET("/debug/runtime", echo.WrapHandler(introspectReg.Handler(runtimeintrospect.HandlerOptions{
		Token:  ec.Cfg.Auth.InternalServiceToken,
		Logger: ec.Logger,
	})))

	return e
}

// MountRoutes calls handler.SetupRoutes and mounts all Hub API routes onto the
// Echo instance. Returns the enroll API handle (may be nil) so main.go can close it.
func MountRoutes(e *echo.Echo, ec EchoConfig) *enroll.EnrollmentAPI {
	return handler.SetupRoutes(handler.RouteConfig{
		Echo:                e,
		Mgr:                 ec.Mgr,
		WSServer:            ec.WSServer,
		Scheduler:           ec.Sched,
		Enrollment:          ec.EnrollSvc,
		MQProducer:          ec.MQProducer,
		ServiceToken:        ec.Cfg.Auth.InternalServiceToken,
		HubConfigToken:      ec.Cfg.Auth.HubConfigToken,
		Store:               ec.Store,
		AgentCA:             ec.AgentCA,
		JWKSCache:           ec.JWKSCache,
		CpIssuer:            ec.Cfg.AuthServer.Issuer,
		CpURL:               ec.Cfg.AuthServer.URL,
		DBPool:              ec.DBPool,
		Raiser:              ec.AlertRaiser,
		AlertStore:          ec.AlertStore,
		AlertRules:          ec.AlertsRes.RulesReg,
		AlertSenders:        ec.AlertsRes.SenderReg,
		CatB:                ec.CatBRegistry,
		SpillStore:          ec.SpillStore,
		SpillBackend:        ec.Cfg.Spill.Backend,
		SpillPerObjectCap:   ec.Cfg.Spill.PerObjectCap(),
		SpillSecrets:        ec.SpillSecrets,
		SpillDedup:          ec.SpillDedup,
		OpsDiagPool:         ec.DBPool,
		OpsLogger:           ec.Logger,
		HubID:               ec.Cfg.Hub.ID,
		HubLocalURL:         fmt.Sprintf("http://localhost:%d", ec.Cfg.Server.Port),
		NormalizeAgentAudit: ec.NormalizeFn,
	})
}

// dbPinger is the narrow pool interface readyzHandler uses for health checks.
// *pgxpool.Pool satisfies it; tests may inject a pgxmock.
type dbPinger interface {
	Ping(ctx context.Context) error
}

// readyzHandler returns an Echo handler that checks DB, Redis, and consumer health.
func readyzHandler(db dbPinger, rdb redis.UniversalClient, cm *consumer.Manager) echo.HandlerFunc {
	return func(c echo.Context) error {
		rctx := c.Request().Context()
		checks := map[string]string{}
		status := http.StatusOK

		if db == nil {
			checks["database"] = "not configured"
		} else if err := db.Ping(rctx); err != nil {
			checks["database"] = "error: " + err.Error()
			status = http.StatusServiceUnavailable
		} else {
			checks["database"] = "ok"
		}

		if rdb != nil {
			if err := rdb.Ping(rctx).Err(); err != nil {
				checks["redis"] = "error: " + err.Error()
				status = http.StatusServiceUnavailable
			} else {
				checks["redis"] = "ok"
			}
		} else {
			checks["redis"] = "not configured"
		}

		if cm != nil {
			if err := cm.HealthCheck(); err != nil {
				checks["consumers"] = "error: " + err.Error()
				status = http.StatusServiceUnavailable
			} else {
				checks["consumers"] = "ok"
			}
		}

		return c.JSON(status, checks)
	}
}

// BuildEchoConfig assembles an EchoConfig from the results of all Init* calls.
// Centralises the field-by-field wiring so main.go stays ≤150 LOC.
func BuildEchoConfig(
	cfg *config.HubConfig,
	buildVersion string,
	dbPool *pgxpool.Pool,
	redisClient redis.UniversalClient,
	mqRes MQResult,
	storageRes StorageResult,
	consumerMgr *consumer.Manager,
	identityRes IdentityResult,
	tmRes FleetResult,
	alertsRes AlertsResult,
	selfShadowRes SelfShadowResult,
	normalizeFn normalizecore.AuditFn,
	sched *scheduler.Scheduler,
	logger *slog.Logger,
) EchoConfig {
	return EchoConfig{
		Cfg:          cfg,
		BuildVersion: buildVersion,
		DBPool:       dbPool,
		RedisClient:  redisClient,
		ConsumerMgr:  consumerMgr,
		Mgr:          tmRes.Mgr,
		WSServer:     tmRes.WSServer,
		Sched:        sched,
		EnrollSvc:    identityRes.EnrollSvc,
		MQProducer:   mqRes.Producer,
		Store:        storageRes.Store,
		AgentCA:      identityRes.AgentCA,
		JWKSCache:    identityRes.JWKSCache,
		AlertStore:   alertsRes.Store,
		AlertRaiser:  alertsRes.Raiser,
		AlertsRes:    alertsRes,
		CatBRegistry: storageRes.CatBRegistry,
		SpillStore:   storageRes.SpillStore,
		SpillSecrets: storageRes.SpillSecrets,
		SpillDedup:   storageRes.SpillDedup,
		NormalizeFn:  normalizeFn,
		SelfShadow:   selfShadowRes,
		Logger:       logger,
	}
}
