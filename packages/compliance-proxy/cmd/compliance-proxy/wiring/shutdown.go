package wiring

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	runtimeserver "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/server"
)

// ShutdownDeps bundles all components that need orderly teardown.
type ShutdownDeps struct {
	Readiness     *atomic.Bool
	ShutdownCoord *conn.ShutdownCoordinator
	RuntimeServer *runtimeserver.Server
	HealthServer  *http.Server
	AuditWriter   audit.Writer          // may be nil
	RedisClient   redis.UniversalClient // may be nil
}

// RunShutdown executes the orderly shutdown sequence and logs each step.
func RunShutdown(d ShutdownDeps) {
	d.Readiness.Store(false)

	if err := d.ShutdownCoord.Shutdown(); err != nil {
		slog.Warn("shutdown coordinator error", "error", err)
	}

	runtimeCtx, runtimeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer runtimeCancel()
	if err := d.RuntimeServer.Shutdown(runtimeCtx); err != nil {
		slog.Warn("runtime API shutdown error", "error", err)
	}

	healthCtx, healthCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer healthCancel()
	if err := d.HealthServer.Shutdown(healthCtx); err != nil {
		slog.Warn("health server shutdown error", "error", err)
	}

	if d.AuditWriter != nil {
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := d.AuditWriter.Close(flushCtx); err != nil {
			slog.Warn("audit writer close error", "error", err)
		}
		flushCancel()
		slog.Info("audit writer flushed and closed")
	}

	if d.RedisClient != nil {
		if err := d.RedisClient.Close(); err != nil {
			slog.Warn("redis close error", "error", err)
		}
	}
}
