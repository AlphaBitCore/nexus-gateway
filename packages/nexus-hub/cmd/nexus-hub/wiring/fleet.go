package wiring

import (
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/opsmetrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/ws"
	sharedops "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// FleetResult holds the WebSocket pool/server and fleet Manager.
type FleetResult struct {
	WSPool   *ws.Pool
	WSServer *ws.Server
	Mgr      *manager.Manager
}

// InitFleet creates the ops-metrics registry, WebSocket pool, fleet Manager,
// and WebSocket Server. The opsReg is threaded through so the same Prometheus
// DefaultRegisterer feeds both the per-tick self-sampler and the WS connection
// counters.
func InitFleet(
	cfg *config.HubConfig,
	st *store.Store,
	redisClient redis.UniversalClient,
	mqProducer mq.Producer,
	opsReg *sharedops.Registry,
	logger *slog.Logger,
) FleetResult {
	wsPool := ws.NewPool(opsReg, logger)
	mgr := manager.New(st, redisClient, mqProducer, wsPool, cfg.Hub.ID, logger)
	wsServer := ws.NewServer(wsPool, mgr, cfg.Hub.ID, cfg.Auth.InternalServiceToken, cfg.Hub.AllowedOrigins, logger)

	return FleetResult{
		WSPool:   wsPool,
		WSServer: wsServer,
		Mgr:      mgr,
	}
}

// InitOpsMetrics sets up the Hub opsmetrics writer trio and handler, then wires
// the opsmetrics handler into wsServer. The caller must defer the Stop calls
// returned below.
//
// Returned stopFn should be called after wsServer.Close() so any in-flight
// handleMessage that already enqueued can still be flushed.
// OpsMetricsResult holds the writer handles produced by InitOpsMetrics.
type OpsMetricsResult struct {
	Writer       *opsmetrics.Writer
	DiagWriter   *opsmetrics.DiagWriterImpl
	StaticWriter *opsmetrics.StaticInfoWriter
}

// InitOpsMetrics sets up the Hub opsmetrics writer trio and handler, then wires
// the handler into wsServer. Stop both writers after wsServer.Close so in-flight
// handleMessage calls can flush.
func InitOpsMetrics(
	pool *pgxpool.Pool,
	opsReg *sharedops.Registry,
	wsServer *ws.Server,
	logger *slog.Logger,
) OpsMetricsResult {
	w := opsmetrics.NewWriter(pool, logger, 10000, 200*time.Millisecond)
	w.SetDropCounter(opsReg.NewCounter("metrics.dropped_total", []string{"reason"}))

	dw := opsmetrics.NewDiagWriter(pool, logger, 10000, 100*time.Millisecond)
	dw.SetDropCounter(opsReg.NewCounter("diag.dropped_total", []string{"reason"}))

	sw := opsmetrics.NewStaticInfoWriter(pool)
	wsServer.SetOpsMetricsHandler(opsmetrics.NewHandler(w, dw, sw, logger))

	return OpsMetricsResult{Writer: w, DiagWriter: dw, StaticWriter: sw}
}
