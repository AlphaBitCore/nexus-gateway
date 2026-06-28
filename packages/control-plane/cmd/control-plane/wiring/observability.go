package wiring

import (
	"context"
	"github.com/goccy/go-json"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	metricsreg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
)

// InitOpsMetrics creates the ops metrics registry, registers CP-specific
// instruments, and returns the registry for use by the Hub thingclient sampler.
func InitOpsMetrics() *metricsreg.Registry {
	opsReg := metricsreg.NewRegistry(prometheus.DefaultRegisterer)
	cpmetrics.Register(opsReg)
	return opsReg
}

// InitObservability initialises the OpenTelemetry tracer provider.
// Merges file-based config (endpoint, service name) with DB-based
// system_metadata (otelEnabled, samplingRate).
// A failure is non-fatal: callers should log a warning and continue.
// The returned closer shuts down the tracer provider on process exit.
func InitObservability(ctx context.Context, db *store.DB, cfg *config.Config, logger *slog.Logger) (*telemetry.SwappableTracerProvider, func(), error) {
	otelCfg := LoadOtelConfig(ctx, db, cfg)
	tp, err := telemetry.Init(ctx, otelCfg, logger)
	if err != nil {
		return nil, func() {}, err
	}
	closer := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(shutdownCtx)
	}
	return tp, closer, nil
}

// LoadOtelConfig builds a telemetry.Config by merging file-based config with
// DB-based system_metadata overrides.  Exported so configdispatch.go (package
// main) can call it from the "observability" shadow-key handler.
func LoadOtelConfig(ctx context.Context, db *store.DB, cfg *config.Config) telemetry.Config {
	result := telemetry.Config{
		ServiceName: "nexus-control-plane",
	}

	if cfg.Otel.Endpoint != "" {
		result.Endpoint = cfg.Otel.Endpoint
	}
	if cfg.Otel.ServiceName != "" {
		result.ServiceName = cfg.Otel.ServiceName
	}

	if db != nil {
		raw, err := db.GetSystemMetadata(ctx, "observability.config")
		if err == nil && raw != nil {
			var dbCfg struct {
				OtelEnabled  *bool    `json:"otelEnabled"`
				SamplingRate *float64 `json:"samplingRate"`
				Endpoint     *string  `json:"endpoint"`
			}
			if err := json.Unmarshal(raw, &dbCfg); err == nil {
				if dbCfg.OtelEnabled != nil {
					result.Enabled = *dbCfg.OtelEnabled
				}
				if dbCfg.SamplingRate != nil {
					result.SamplingRate = *dbCfg.SamplingRate
				}
				if dbCfg.Endpoint != nil && *dbCfg.Endpoint != "" {
					result.Endpoint = *dbCfg.Endpoint
				}
			}
		}
	}

	return result
}
