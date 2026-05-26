// Package telemetry provides OTEL tracing for the Nexus Agent.
// It delegates to the shared SwappableTracerProvider.
package telemetry

import (
	"context"
	"log/slog"

	sharedtel "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
)

// Config is an alias for the shared telemetry Config.
type Config = sharedtel.Config

// Provider is the swappable tracer provider.
type Provider = sharedtel.SwappableTracerProvider

// Init creates a SwappableTracerProvider via the shared package.
func Init(ctx context.Context, cfg Config, logger *slog.Logger) (*Provider, error) {
	return sharedtel.Init(ctx, cfg, logger)
}
