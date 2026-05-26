package wiring

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
	selfreg "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/self/reg"
	selfshadow "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/self/shadow"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// InitSelfReg creates and executes Hub self-registration (inserts/updates the
// Hub's own row in the thing table). Returns the *SelfRegistrar so the caller
// can Deregister on shutdown.
func InitSelfReg(
	ctx context.Context,
	cfg *config.HubConfig,
	buildVersion string,
	st *store.Store,
	logger *slog.Logger,
) (*selfreg.SelfRegistrar, error) {
	sr := selfreg.New(selfreg.Config{
		InstanceID:       cfg.Hub.ID,
		Address:          cfg.Hub.AdvertiseAddr,
		MetricsURL:       fmt.Sprintf("http://%s/metrics", cfg.Hub.AdvertiseAddr),
		Version:          buildVersion,
		SchedulerEnabled: cfg.Scheduler.Enabled,
		PhysicalID:       cfg.Hub.ID,
	}, st, logger)

	if err := sr.Register(ctx); err != nil {
		return nil, err
	}
	return sr, nil
}

// SelfShadowResult holds the manager and the config key recorder.
type SelfShadowResult struct {
	Manager           *selfshadow.Manager
	ConfigKeyRecorder *runtimeintrospect.KeyStateRecorder
}

// InitSelfShadow wires the Hub self-shadow manager. Subscribes to PostgreSQL
// LISTEN config_changed, filters by Hub's own instance ID, and dispatches
// per-key reload handlers. Registers:
//   - "observability" → reconfigures the SwappableTracerProvider
//   - "log_level"     → live log level swap via logging.SetLevel
//
// Caller must defer ssMgr.Stop(ctx).
func InitSelfShadow(
	ctx context.Context,
	cfg *config.HubConfig,
	pool *pgxpool.Pool,
	st *store.Store,
	otel OTELResult,
	logger *slog.Logger,
) (SelfShadowResult, error) {
	configKeyRecorder := runtimeintrospect.NewKeyStateRecorder()
	ssMgr := selfshadow.New(cfg.Hub.ID, pool, st, logger)

	ssMgr.Register(configkey.Observability, selfshadow.HandlerFunc(func(_ context.Context, state json.RawMessage) error {
		configKeyRecorder.Record(configkey.Observability, state)
		if otel.Provider == nil {
			return nil
		}
		// Hub-side observability shape is a subset of telemetry.Config.
		// Missing fields fall back to YAML defaults so a partial UI edit
		// doesn't wipe the endpoint.
		var raw struct {
			Enabled      *bool    `json:"enabled"`
			Endpoint     *string  `json:"endpoint"`
			ServiceName  *string  `json:"serviceName"`
			SamplingRate *float64 `json:"samplingRate"`
		}
		if err := json.Unmarshal(state, &raw); err != nil {
			return fmt.Errorf("decode observability state: %w", err)
		}
		merged := otel.InitialCfg
		if raw.Enabled != nil {
			merged.Enabled = *raw.Enabled
		}
		if raw.Endpoint != nil {
			merged.Endpoint = *raw.Endpoint
		}
		if raw.ServiceName != nil && *raw.ServiceName != "" {
			merged.ServiceName = *raw.ServiceName
		}
		if raw.SamplingRate != nil {
			merged.SamplingRate = *raw.SamplingRate
		}
		return otel.Provider.Reconfigure(merged)
	}))

	ssMgr.Register(configkey.LogLevel, selfshadow.HandlerFunc(func(_ context.Context, state json.RawMessage) error {
		// Hub log level swap. shared/logging.SetLevel mutates the
		// package-level slog.LevelVar that NewLogger installed; the handler
		// chain reads it per record so the change takes effect immediately
		// without rebuilding any logger.
		configKeyRecorder.Record(configkey.LogLevel, state)
		var lv struct {
			Level string `json:"level"`
		}
		if err := json.Unmarshal(state, &lv); err != nil {
			return fmt.Errorf("decode log_level state: %w", err)
		}
		applied := logging.SetLevel(lv.Level)
		logger.Info("hub log level updated via shadow",
			"requested", lv.Level,
			"applied", applied.String(),
		)
		return nil
	}))

	if err := ssMgr.Start(ctx); err != nil {
		// Start currently doesn't return an error; defensive log if that
		// changes. The listener loop is best-effort.
		logger.Warn("selfshadow start failed", "error", err)
	}

	return SelfShadowResult{
		Manager:           ssMgr,
		ConfigKeyRecorder: configKeyRecorder,
	}, nil
}

