package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/handler/enroll"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/consumer"
	selfreg "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/self/reg"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/ws"
)

// GracefulShutdown runs the ordered shutdown sequence for Hub components.
// It is called after the signal is received and the main context is cancelled.
func GracefulShutdown(
	shutdownCtx context.Context,
	e *echo.Echo,
	selfRegistrar *selfreg.SelfRegistrar,
	wsServer *ws.Server,
	consumerMgr *consumer.Manager,
	enrollAPI *enroll.EnrollmentAPI,
	opsRes OpsMetricsResult,
	logger *slog.Logger,
) {
	// Deregister the Hub thing row so CP UI shows it as offline immediately.
	if selfRegistrar != nil {
		if err := selfRegistrar.Deregister(shutdownCtx); err != nil {
			logger.Warn("hub self-deregistration failed", "error", err)
		}
	}

	// Close WebSocket connections before draining the HTTP server so in-flight
	// handleMessage calls complete or error cleanly.
	if wsServer != nil {
		wsServer.Close()
	}

	// Stop MQ consumer manager.
	if consumerMgr != nil {
		consumerMgr.Stop()
		logger.Info("MQ consumer manager stopped")
	}

	// Close enroll API connections (gRPC or long-poll).
	if enrollAPI != nil {
		enrollAPI.Close()
	}

	// Flush opsmetrics writers — must run AFTER wsServer.Close so any in-flight
	// handleMessage that already enqueued can still be committed.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if opsRes.Writer != nil {
		_ = opsRes.Writer.Stop(stopCtx)
	}
	if opsRes.DiagWriter != nil {
		_ = opsRes.DiagWriter.Stop(stopCtx)
	}

	// Shut down the HTTP server gracefully.
	if err := e.Shutdown(shutdownCtx); err != nil {
		logger.Error("http server shutdown error", "error", err)
	}

	logger.Info("nexus-hub stopped")
}
