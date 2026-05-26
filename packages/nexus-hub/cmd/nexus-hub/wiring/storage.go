package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/compliance/catbagent"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillupload"
)

// StorageResult holds the store handles produced by InitStorage.
type StorageResult struct {
	Store        *store.Store
	CatBRegistry *store.CatBRegistry
	SpillStore   spillstore.SpillStore
	SpillSecrets *spillupload.SecretStore
	SpillDedup   spillupload.Dedup
}

// InitStorage wires the central store, Cat B registry, spill store, and spill
// upload secret/dedup handles. Pool must already be open.
func InitStorage(
	ctx context.Context,
	cfg *config.HubConfig,
	pool *pgxpool.Pool,
	redisClient redis.UniversalClient,
	logger *slog.Logger,
) (StorageResult, error) {
	st := store.New(pool)

	// Cat B loader registry. Hub aggregates CP-owned state for Cat B config
	// keys. Adding more (thingType, configKey) loaders is a one-line Register
	// call. Rule-pack store backs the hooks enricher so agent-bound hooks
	// ship to the agent with their effective rule sets baked into Config.
	hubRulePackStore := rulepack.NewStore(pool)
	catBRegistry := store.NewCatBRegistry()
	catBRegistry.Register("agent", configkey.Hooks,
		catbagent.NewAgentHookConfigLoader(pool, hubRulePackStore, logger))
	catBRegistry.Register("agent", configkey.InterceptionDomains,
		catbagent.NewAgentInterceptionDomainsLoader(pool, logger))
	catBRegistry.Register("agent", configkey.PayloadCapture,
		catbagent.NewAgentPayloadCaptureLoader(pool, logger))
	catBRegistry.Register("agent", configkey.StreamingCompliance,
		catbagent.NewAgentStreamingComplianceLoader(pool, logger))
	catBRegistry.Register("agent", configkey.InstalledRulePacks,
		catbagent.NewAgentInstalledRulePacksLoader(pool, logger))
	catBRegistry.Register("agent", configkey.UserContext,
		catbagent.NewAgentUserContextLoader(pool, logger))
	catBRegistry.Register("agent", configkey.Exemptions,
		catbagent.NewAgentExemptionsLoader(pool, logger))

	// Spill store. Wire spillstore for Control Plane reads (spill ref → bytes)
	// and for the dev localfs blob upload endpoint. Returns (nil, nil) when
	// cfg.Spill.Enabled is false. The audit ingestion path NEVER calls
	// SpillStore.Put — agents upload via the pre-signed URL flow before
	// submitting the audit envelope.
	hubSpillStore, err := spillfactory.New(cfg.Spill, logger)
	if err != nil {
		return StorageResult{}, err
	}

	// Spill upload secrets. LoadOrInit auto-generates an epoch-1 secret on
	// first boot. Failures are non-fatal: Hub keeps running but the mint
	// endpoint returns 503 until the operator fixes the issue.
	var spillSecrets *spillupload.SecretStore
	if hubSpillStore != nil && pool != nil {
		secretInitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		spillSecrets, err = spillupload.LoadOrInit(secretInitCtx, newHubMetadataAdapter(pool))
		cancel()
		if err != nil {
			logger.Warn("spill upload secrets unavailable; mint endpoint will return 503", "error", err)
			spillSecrets = nil
		}
	}

	var spillDedup spillupload.Dedup
	if redisClient != nil {
		spillDedup = spillupload.NewRedisDedup(redisClient)
	}

	return StorageResult{
		Store:        st,
		CatBRegistry: catBRegistry,
		SpillStore:   hubSpillStore,
		SpillSecrets: spillSecrets,
		SpillDedup:   spillDedup,
	}, nil
}
