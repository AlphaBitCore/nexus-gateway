package wiring

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
)

// RunUntilSignal starts the Echo HTTP server and blocks until it receives
// SIGINT or SIGTERM, or until the server itself errors.
// On signal it performs a graceful shutdown within cfg.Server.ShutdownTimeout
// seconds and returns 0; on server error it returns 1.
func RunUntilSignal(ctx context.Context, ctxCancel context.CancelFunc, e *echo.Echo, cfg *config.Config, logger *slog.Logger) int {
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	slog.Info("starting control-plane",
		slog.String("addr", addr),
		slog.String("log_level", cfg.Log.Level),
	)

	serverErr := make(chan error, 1)
	go func() {
		if err := e.Start(addr); err != nil {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		slog.Error("server error", "error", err)
		ctxCancel()
		return 1
	case sig := <-quit:
		slog.Info("shutting down", slog.String("signal", sig.String()))
	}
	ctxCancel()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
	slog.Info("control-plane stopped")
	return 0
}
