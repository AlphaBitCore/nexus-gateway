package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/telemetry"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// InitOpsMetrics creates the shared opsmetrics registry backed by
// prometheus.DefaultRegisterer and registers the tlsbump counters.
// Returns the registry and the process start time stamp.
func InitOpsMetrics() (*opsmetrics.Registry, time.Time) {
	opsReg := opsmetrics.NewRegistry(prometheus.DefaultRegisterer)
	tlsbump.RegisterMetrics(opsReg)
	return opsReg, time.Now().UTC()
}

// InitTelemetry initialises OpenTelemetry (no-op if disabled).
// The returned provider may be nil when cfg.OtelEnabled is false.
// The caller must defer tp.Shutdown(ctx) when tp != nil.
func InitTelemetry(cfg TelemetryConfig, logger *slog.Logger) (*telemetry.Provider, error) {
	tp, err := telemetry.Init(context.Background(), telemetry.Config{
		Enabled:      cfg.OtelEnabled,
		Endpoint:     cfg.OtelEndpoint,
		ServiceName:  cfg.OtelServiceName,
		SamplingRate: cfg.OtelSamplingRate,
	}, logger)
	if err != nil {
		logger.Warn("OpenTelemetry init failed", "error", err)
	}
	return tp, nil
}

// TelemetryConfig is the subset of agent config needed for OTel init.
type TelemetryConfig struct {
	OtelEnabled      bool
	OtelEndpoint     string
	OtelServiceName  string
	OtelSamplingRate float64
}
