package wiring

import (
	"context"
	"log/slog"
	"os"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
)

// InitRuntimeIntrospect registers /debug/runtime on e with sources for
// config flags, db pool stats, and Hub-pushed config keys.
func InitRuntimeIntrospect(
	e *echo.Echo,
	cfg *config.Config,
	db *store.DB,
	cpID string,
	buildVersion string,
	configKeyRecorder *runtimeintrospect.KeyStateRecorder,
	logger *slog.Logger,
) {
	cpIntrospect := runtimeintrospect.New("control-plane", cpID, buildVersion)

	cpHostname, _ := os.Hostname()
	cpIntrospect.Register(runtimeintrospect.SourceFunc{
		SourceName: "config.flags",
		Fn: func(_ context.Context) (any, error) {
			return map[string]any{
				"cp_id":          cpID,
				"hostname":       cpHostname,
				"server_port":    cfg.Server.Port,
				"hub_url":        cfg.Registry.NexusHubURL,
				"ai_gateway_url": cfg.BFF.AIGatewayURL,
			}, nil
		},
	})
	cpIntrospect.Register(configKeyRecorder.Source("log_level"))
	cpIntrospect.Register(configKeyRecorder.Source("observability"))

	if db != nil && db.Pool != nil {
		cpIntrospect.Register(runtimeintrospect.SourceFunc{
			SourceName: "runtime.db_pool",
			Fn: func(_ context.Context) (any, error) {
				stat := db.Pool.Stat()
				return map[string]any{
					"acquired_conns": stat.AcquiredConns(),
					"idle_conns":     stat.IdleConns(),
					"total_conns":    stat.TotalConns(),
					"max_conns":      stat.MaxConns(),
				}, nil
			},
		})
	}

	e.GET("/debug/runtime", echo.WrapHandler(cpIntrospect.Handler(runtimeintrospect.HandlerOptions{
		Token:  cfg.Auth.InternalServiceToken,
		Logger: logger,
	})))
}
