package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/cache/cachestore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/thingstats/thingstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/configreconcile"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// InitReconciler creates and starts the config-drift reconcile loop.
// The loop compares CP source-of-truth against Hub's thing.desired every 60s
// for the known emergency-grade Category-A keys and re-emits
// Hub.NotifyConfigChange on drift. Does nothing when db is nil.
//
// The watch set tracks emergency / safety-critical configs whose drift
// would silently leave the fleet in an unsafe state:
//   - cache (ai-gateway): incorrect cache config can over-cache PII or
//     under-cache cost-sensitive flows.
//   - agent_settings (agent): heartbeat / runtime tuning drift is benign
//     long-term but creates noisy "unhealthy" alerts.
//   - killswitch (compliance-proxy + agent): the emergency brake; if a
//     WS reconnect happens mid-toggle, drift reconciliation re-pushes so
//     the fleet doesn't silently apply the pre-toggle state.
//   - gateway_passthrough (ai-gateway): emergency passthrough toggle,
//     same operational class as killswitch.
func InitReconciler(
	ctx context.Context,
	db *store.DB,
	hubClient *hub.Client,
	logger *slog.Logger,
) {
	if db == nil {
		return
	}
	pool := db.InternalPool()
	cs := cachestore.New(pool)
	meta := systemmetastore.NewFromPool(pool)
	watches := []configreconcile.Watch{
		{
			ConfigKey: configkey.Cache,
			ThingType: "ai-gateway",
			SourceLoader: func(ctx context.Context) (json.RawMessage, error) {
				blob, err := cs.AssembleCacheConfigBlob(ctx)
				if err != nil {
					return nil, err
				}
				return json.Marshal(blob)
			},
		},
		{
			ConfigKey: configkey.AgentSettings,
			ThingType: "agent",
			SourceLoader: func(ctx context.Context) (json.RawMessage, error) {
				raw, err := meta.GetSystemMetadata(ctx, "agent.settings")
				if err != nil {
					return nil, err
				}
				if raw == nil {
					return []byte("{}"), nil
				}
				return raw, nil
			},
		},
		// Killswitch on compliance-proxy — emergency-grade. A WS reconnect
		// between toggle and apply would leave the fleet in the previous
		// state; drift reconciliation re-pushes to recover within 60s.
		{
			ConfigKey: configkey.Killswitch,
			ThingType: "compliance-proxy",
			SourceLoader: func(ctx context.Context) (json.RawMessage, error) {
				return loadKillswitchTemplate(ctx, pool, "compliance-proxy")
			},
		},
		// Killswitch on agent — same safety justification as the compliance-proxy leg.
		{
			ConfigKey: configkey.Killswitch,
			ThingType: "agent",
			SourceLoader: func(ctx context.Context) (json.RawMessage, error) {
				return loadKillswitchTemplate(ctx, pool, "agent")
			},
		},
		// Gateway emergency passthrough — same operational class as
		// killswitch: a WS reconnect during an active passthrough window
		// must not silently revert to normal routing.
		{
			ConfigKey: configkey.GatewayPassthrough,
			ThingType: "ai-gateway",
			SourceLoader: func(ctx context.Context) (json.RawMessage, error) {
				return loadConfigTemplate(ctx, pool, "ai-gateway", configkey.GatewayPassthrough)
			},
		},
	}
	reconciler := configreconcile.New(thingstore.New(pool), hubClient, logger, 60*time.Second, watches, prometheus.DefaultRegisterer)
	go reconciler.Run(ctx)
}

// loadKillswitchTemplate reads the canonical {enabled: bool} state from
// thing_config_template for the supplied thing type. Returns "{}" for an
// unseeded template so the reconciler treats "no template" as the safe
// default rather than panicking.
func loadKillswitchTemplate(ctx context.Context, pool store.PgxPool, thingType string) (json.RawMessage, error) {
	return loadConfigTemplate(ctx, pool, thingType, configkey.Killswitch)
}

// loadConfigTemplate reads the JSON state of a thing_config_template
// row directly via pgx. Hand-rolled to avoid pulling thingstore's
// richer ThingConfigTemplate type just for the reconciler's "what's
// the canonical desired state" question. ErrNoRows maps to "{}" so the
// reconciler treats an unseeded template as the safe empty state.
func loadConfigTemplate(ctx context.Context, pool store.PgxPool, thingType, configKey string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := pool.QueryRow(ctx,
		`SELECT state FROM thing_config_template WHERE type = $1 AND config_key = $2`,
		thingType, configKey,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return json.RawMessage("{}"), nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	return raw, nil
}
