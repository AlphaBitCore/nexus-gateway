// hooks.go — compliance pipeline + hook config cache + payload capture + wirerewrite wiring.
package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	hookwh "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/webhook"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// InitHookRegistry clones the shared hook registry and replaces
// webhook-forward with a variant using the ai-gateway shared http.Client pool.
func InitHookRegistry(webhookCfg config.HTTPClientPoolConfig) (*hookcore.HookRegistry, error) {
	webhookClient := nexushttp.New(nexushttp.Config{
		Timeout:             time.Duration(webhookCfg.TimeoutSec) * time.Second,
		MaxIdleConns:        webhookCfg.MaxIdleConns,
		MaxIdleConnsPerHost: webhookCfg.MaxIdleConnsPerHost,
		IdleConnTimeout:     time.Duration(webhookCfg.IdleConnTimeoutSec) * time.Second,
		Caller:              "webhook-shared",
		PropagateReqID:      true,
	})
	gwHookRegistry := builtins.Registry.Clone()
	gwHookRegistry.Replace("webhook-forward", func(cfg *hookcore.HookConfig) (hookcore.Hook, error) {
		return hookwh.NewWebhookForwardWithClient(cfg, webhookClient)
	})
	gwHookRegistry.Freeze()
	return gwHookRegistry, nil
}

// InitHookConfigCache constructs the shared HookConfigCache backed by the DB.
func InitHookConfigCache(
	db *store.DB,
	gwHookRegistry *hookcore.HookRegistry,
	logger *slog.Logger,
) *pipeline.HookConfigCache {
	var rulePackStore *rulepack.Store
	if db != nil {
		rulePackStore = rulepack.NewStore(db.Pool)
	}
	return pipeline.NewHookConfigCache(
		func(ctx context.Context) ([]hookcore.HookConfig, error) {
			if db == nil {
				return nil, nil
			}
			cfgs, err := LoadHookConfigsFromDB(ctx, db.Pool)
			if err != nil {
				return nil, err
			}
			if rulePackStore != nil {
				if cfgs, err = rulepack.Enrich(ctx, rulePackStore, cfgs); err != nil {
					return nil, fmt.Errorf("hooks: enrich rule packs: %w", err)
				}
			}
			return cfgs, nil
		},
		gwHookRegistry,
		2*time.Minute,
		logger,
	)
}

// LoadHookConfigsFromDB queries enabled hook configs from the database.
//
// The top-level `endpoint` column MUST be selected: webhook-style hooks
// (webhook-forward, AI-Guard compliance webhook) read their remote URL
// from cfg.Config["endpoint"], and the admin API persists that URL into
// the dedicated `endpoint` column — not into the config JSON. Skipping
// the column here would cause BuildHookConfig to leave cfg.Config
// without "endpoint", and every webhook hook would fail at construction
// with `webhook-forward: endpoint is required`.
func LoadHookConfigsFromDB(ctx context.Context, pool *pgxpool.Pool) ([]hookcore.HookConfig, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, name, "implementationId", stage, enabled, priority,
		       config, "timeoutMs", "failBehavior", "applicableIngress",
		       endpoint
		FROM "HookConfig"
		WHERE enabled = true
		ORDER BY priority ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("hooks: query configs: %w", err)
	}
	defer rows.Close()

	var configs []hookcore.HookConfig
	for rows.Next() {
		var hc hookcore.HookConfig
		var configJSON []byte
		var endpoint *string
		if err := rows.Scan(
			&hc.ID, &hc.Name, &hc.ImplementationID, &hc.Stage,
			&hc.Enabled, &hc.Priority, &configJSON,
			&hc.TimeoutMs, &hc.FailBehavior, &hc.ApplicableIngress,
			&endpoint,
		); err != nil {
			return nil, fmt.Errorf("hooks: scan config: %w", err)
		}
		row := hookcore.HookConfigRow{
			ID:                hc.ID,
			Name:              hc.Name,
			ImplementationID:  hc.ImplementationID,
			Stage:             hc.Stage,
			Enabled:           hc.Enabled,
			Priority:          hc.Priority,
			TimeoutMs:         hc.TimeoutMs,
			FailBehavior:      hc.FailBehavior,
			ConfigJSON:        string(configJSON),
			ApplicableIngress: hc.ApplicableIngress,
		}
		if endpoint != nil {
			row.Endpoint = *endpoint
		}
		built, err := hookcore.BuildHookConfig(row)
		if err != nil {
			return nil, fmt.Errorf("hooks: build config %q: %w", hc.ID, err)
		}
		configs = append(configs, built)
	}
	return configs, nil
}

// InitPayloadCaptureStore seeds the store from system_metadata.
// LoadPayloadCaptureConfig is in observability.go (same package).
func InitPayloadCaptureStore(ctx context.Context, db *store.DB) *payloadcapture.Store {
	pcs := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	if db != nil {
		initCtx, initCancel := context.WithTimeout(ctx, 10*time.Second)
		defer initCancel()
		if pcCfg, err := LoadPayloadCaptureConfig(initCtx, db); err != nil {
			slog.Warn("payload capture initial load failed; using defaults", "error", err)
		} else {
			pcs.Set(pcCfg)
			slog.Info("payload capture config loaded",
				"storeRequestBody", pcCfg.StoreRequestBody,
				"storeResponseBody", pcCfg.StoreResponseBody,
				"maxInlineBodyBytes", pcCfg.MaxInlineBodyBytes,
				"maxRequestBytes", pcCfg.MaxRequestBytes,
				"maxResponseBytes", pcCfg.MaxResponseBytes,
			)
		}
	}
	return pcs
}

// InitStreamingPolicyStore seeds the streaming compliance policy Store
// from system_metadata['streaming_compliance.config']. #115
// three-service alignment — agent, compliance-proxy, and ai-gateway
// all wire their Store through the shared streampolicy.BootStore
// helper. ai-gateway uses pgxpool (not the stdlib *sql.DB that
// streampolicy.LoadGlobalDefault expects) so the loader closure routes
// the query through store.DB.GetSystemMetadata and hands the raw JSON
// off to BootStore for decode + Set.
//
// configdispatch's registerStreamingCompliance handler reloads on
// every Hub shadow push of the streaming_compliance key by calling
// Store.ApplyShadowState — no per-server setter wrapper.
//
// Nil DB → BootStore with nil loader → Store seeded with DefaultPolicy()
// only. Downstream consumers (proxy_cache.go SSE handler) read
// Store.Get() per-request.
func InitStreamingPolicyStore(ctx context.Context, db *store.DB) *streampolicy.Store {
	var loader streampolicy.RawConfigLoader
	if db != nil {
		loader = func(loadCtx context.Context) (json.RawMessage, error) {
			raw, err := db.GetSystemMetadata(loadCtx, streampolicy.SystemMetadataKey)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, nil
				}
				return nil, err
			}
			return raw, nil
		}
	}
	return streampolicy.BootStore(ctx, loader, slog.Default())
}

// InitNormEngine creates and returns a wirerewrite engine.
func InitNormEngine(logger *slog.Logger) *wirerewrite.Engine {
	return wirerewrite.New(logger)
}
