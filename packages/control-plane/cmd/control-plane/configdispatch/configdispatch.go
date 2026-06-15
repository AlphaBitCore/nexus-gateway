// Package configdispatch wires every Hub shadow config key the Control Plane
// consumes onto a shared/transport/configloader.Loader. The Loader handles
// outcome tracking, per-key error wrapping, reported-map assembly, and
// structured logging; each registerX() declares only the parse + apply logic.
package configdispatch

import (
	"context"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/wiring"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// configDispatchDeps carries every subsystem the per-key handlers
// touch. Control Plane currently consumes only two shadow keys, so
// the surface is intentionally small.
type configDispatchDeps struct {
	Logger            *slog.Logger
	ThingID           string
	Outcomes          *thingclient.OutcomeTracker
	TelemetryProvider *telemetry.SwappableTracerProvider // may be nil
	DB                *store.DB
	BootstrapConfig   *config.Config
}

// buildConfigLoader returns a Loader pre-populated with every shadow
// key Control Plane consumes.
func buildConfigLoader(d configDispatchDeps) *cfgloader.Loader {
	l := cfgloader.New(d.Logger, d.Outcomes, d.ThingID, "control-plane")

	registerCPObservability(l, d)
	registerCPLogLevel(l, d)

	return l
}

// registerCPObservability wires Hub-pushed observability deltas onto
// the live telemetry provider. Every apply re-reads the DB row via
// wiring.LoadOtelConfig because the Hub delta carries only the trigger,
// not the full config.
func registerCPObservability(l *cfgloader.Loader, d configDispatchDeps) {
	cfgloader.RegisterRaw(l, "observability", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.TelemetryProvider == nil {
			return nil, nil
		}
		newCfg := wiring.LoadOtelConfig(ctx, d.DB, d.BootstrapConfig)
		if err := d.TelemetryProvider.Reconfigure(newCfg); err != nil {
			return nil, err
		}
		return nil, nil
	})
}

// BuildConfigChangedCallback returns the thingclient OnConfigChanged callback
// for the Control Plane. It records incoming key states in the recorder,
// runs all registered parse+apply handlers via the Loader, and default-echoes
// unknown keys so Hub does not see spurious drift.
func BuildConfigChangedCallback(
	logger *slog.Logger,
	thingID string,
	tc *thingclient.Client,
	tp *telemetry.SwappableTracerProvider,
	db *store.DB,
	cfg *config.Config,
	rec *runtimeintrospect.KeyStateRecorder,
) func(map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
	return func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
		for k, cs := range desired {
			rec.Record(k, cs.State)
		}
		cfgLoader := buildConfigLoader(configDispatchDeps{
			Logger:            logger,
			ThingID:           thingID,
			Outcomes:          tc.Outcomes(),
			TelemetryProvider: tp,
			DB:                db,
			BootstrapConfig:   cfg,
		})
		reported, applyErr := cfgLoader.Apply(context.Background(), desired)
		for k, cs := range desired {
			if !cfgLoader.Has(k) {
				reported[k] = cs
			}
		}
		return reported, applyErr
	}
}

type cpLogLevelState struct {
	Level string `json:"level"`
}

// registerCPLogLevel wires Hub-pushed log_level deltas onto the
// process-wide slog.LevelVar via logging.SetLevel.
func registerCPLogLevel(l *cfgloader.Loader, d configDispatchDeps) {
	cfgloader.Register(l, cfgloader.Handler[cpLogLevelState]{
		Key:   "log_level",
		Parse: cfgloader.ParseJSON[cpLogLevelState](),
		Apply: func(ctx context.Context, v cpLogLevelState, ver int64) ([]byte, error) {
			// An empty level (blank/null/{} tick, or an explicit empty
			// string) is a no-op — matching ai-gateway. SetLevel("") parses
			// to LevelInfo and would silently reset the live level, clobbering
			// a boot-time debug.
			if v.Level == "" {
				return nil, nil
			}
			applied := logging.SetLevel(v.Level)
			d.Logger.Info("log level updated via shadow",
				"requested", v.Level,
				"applied", applied.String(),
			)
			return nil, nil
		},
	})
}
