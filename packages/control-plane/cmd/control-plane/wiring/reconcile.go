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
	cachehandler "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/cache/handler"
	passthrough "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/passthrough/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/thingstats/thingstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/configreconcile"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
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
//   - semantic_cache.config (ai-gateway): fleet-wide L2 embedding config; a
//     lost push leaves L2 disabled or serving the previous fingerprint.
//   - response_cache.extract_config (ai-gateway): fleet-wide L1 extract cache
//     toggle/TTL; a lost push serves the previous exact-match cache config.
//   - response_cache.time_sensitive_patterns (ai-gateway): cluster-wide
//     freshness rule list; a lost push serves a stale freshness gate.
//   - agent_settings (agent): heartbeat / runtime tuning drift is benign
//     long-term but creates noisy "unhealthy" alerts.
//   - killswitch (compliance-proxy + agent): the emergency brake; if a
//     WS reconnect happens mid-toggle, drift reconciliation re-pushes so
//     the fleet doesn't silently apply the pre-toggle state.
//   - gateway_passthrough (ai-gateway): emergency passthrough toggle,
//     same operational class as killswitch.
//
// The three response/semantic cache keys each push their own Category-A blob
// under their own configKey (the unified `cache` watch cannot heal them). Each
// reconcile SourceLoader projects the source of truth through the EXACT same
// transform the admin handler's push uses — SemanticCacheConfigRow.WireState,
// ExtractCacheConfigRow.WireState, and cachehandler.BlobToPatterns respectively
// — so the content diff is apples-to-apples and never thrashes.
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
	meta := systemmetastore.New(pool)
	semStore := configstore.NewSemanticCacheStoreWithPgxPool(pool)
	extStore := configstore.NewExtractCacheStoreWithPgxPool(pool)
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
		// Semantic cache (L2) config — projected via the same WireState the
		// SemanticCacheHandler.PutConfig push uses (drops the wall-clock
		// UpdatedAt/UpdatedBy so the content diff never thrashes on save).
		{
			ConfigKey: configkey.SemanticCacheConfig,
			ThingType: "ai-gateway",
			SourceLoader: func(ctx context.Context) (json.RawMessage, error) {
				row, err := semStore.Get(ctx)
				if err != nil {
					return nil, err
				}
				return json.Marshal(row.WireState())
			},
		},
		// Extract cache (L1) config — the three behavioral fields, projected via
		// the same WireState the ExtractCacheHandler.PutConfig push uses.
		{
			ConfigKey: configkey.ResponseCacheExtractConfig,
			ThingType: "ai-gateway",
			SourceLoader: func(ctx context.Context) (json.RawMessage, error) {
				row, err := extStore.Get(ctx)
				if err != nil {
					return nil, err
				}
				return json.Marshal(row.WireState())
			},
		},
		// Time-sensitive freshness patterns — the DB override blob converted via
		// the same BlobToPatterns transform the time-sensitive push uses.
		{
			ConfigKey: configkey.ResponseCacheTimeSensitivePatterns,
			ThingType: "ai-gateway",
			SourceLoader: func(ctx context.Context) (json.RawMessage, error) {
				blob, err := semStore.GetOverrides(ctx)
				if err != nil {
					return nil, err
				}
				return json.Marshal(cachehandler.BlobToPatterns(blob))
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
		// must not silently revert to normal routing. The source of truth is
		// the three gateway_passthrough_config_* tables assembled into the
		// same blob the admin push sends (passthrough.SourceState), NOT a
		// thing_config_template row — Hub writes that template from the last
		// push, so reading it would compare Hub-against-Hub and never detect a
		// dropped push.
		{
			ConfigKey: configkey.GatewayPassthrough,
			ThingType: "ai-gateway",
			SourceLoader: func(ctx context.Context) (json.RawMessage, error) {
				return passthrough.SourceState(ctx, pool)
			},
		},
	}
	reconciler := configreconcile.New(thingstore.New(pool), hubClient, logger, 60*time.Second, watches, prometheus.DefaultRegisterer)

	// Attach the durable Category-B propagation backstop. The same
	// hubClient handlers already call via InvalidateConfigE now records intent
	// vs ack per (type, key); the reconcile loop's Pending arm re-pushes any key
	// whose last push CP never confirmed. SetLedger runs here — before the HTTP
	// server accepts requests in RunUntilSignal — so no live request observes a
	// half-wired client.
	if hubClient != nil {
		hubClient.SetLedger(hub.NewLedger(pool))
		reconciler.Pending = hubClient
	}

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
