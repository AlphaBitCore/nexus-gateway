package wiring

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/loaders"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// InitPayloadCaptureStore creates and seeds the payload capture store from DB.
// When configDB is nil the default config is returned without a DB read.
func InitPayloadCaptureStore(configDB *sql.DB, emitter *compliance.AuditEmitter, logger *slog.Logger) *payloadcapture.Store {
	store := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	if configDB != nil {
		initCtx, initCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer initCancel()
		if pcCfg, err := loaders.LoadPayloadCaptureConfig(initCtx, configDB); err != nil {
			logger.Warn("payload capture config initial load failed; using defaults", "error", err)
		} else {
			store.Set(pcCfg)
			logger.Info("payload capture config loaded",
				"storeRequestBody", pcCfg.StoreRequestBody,
				"storeResponseBody", pcCfg.StoreResponseBody,
				"maxInlineBodyBytes", pcCfg.MaxInlineBodyBytes,
			)
		}
	}
	if emitter != nil {
		emitter.WithPayloadCaptureStore(store)
	}
	return store
}

// InitStreamingPolicyStore returns the hot-swappable streaming policy
// Store seeded with the admin-configured Policy (loaded from
// system_metadata['streaming_compliance.config']) or DefaultPolicy()
// when DB is unavailable / the row is missing. Three-service alignment
// (#115): agent / compliance-proxy / ai-gateway all wire a *Store
// through the shared streampolicy.BootStore helper; CP's
// configdispatch handler reloads the Store via Store.ApplyShadowState
// on every Hub shadow push of the streaming_compliance key.
//
// Delegates the boot boilerplate (load + decode + log + install) to
// streampolicy.BootStore so warn/info log shapes match across all
// three services.
func InitStreamingPolicyStore(configDB *sql.DB, logger *slog.Logger) *streampolicy.Store {
	var loader streampolicy.RawConfigLoader
	if configDB != nil {
		loader = func(ctx context.Context) (json.RawMessage, error) {
			var raw json.RawMessage
			err := configDB.QueryRowContext(ctx,
				`SELECT value FROM system_metadata WHERE key = $1`,
				streampolicy.SystemMetadataKey,
			).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return raw, err
		}
	}
	return streampolicy.BootStore(context.Background(), loader, logger)
}
