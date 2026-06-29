// Package configdispatch wires every shadow config key the AI Gateway
// consumes onto a single shared/transport/configloader.Loader.
package configdispatch

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/cmd/ai-gateway/wiring"
	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/freshness"
	geminicache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/gemini"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// HookConfigReloader narrows the shared HookConfigCache surface to Reload-only.
type HookConfigReloader interface {
	Reload(ctx context.Context) error
}

// CredCacheInvalidator narrows *credmanager.Manager to the two
// decrypted-credential cache operations the credentials applier needs:
// per-id eviction (granular) and a full flush (fallback). Keeping it
// as an interface decouples configdispatch from the Manager's construction
// surface and lets the credentials applier be unit-tested with a fake.
type CredCacheInvalidator interface {
	Invalidate(credentialID string)
	ClearCache()
}

// ReliabilityReloader narrows *wiring.ReliabilityConfig to Reload-only.
// Aliased from wiring to avoid re-declaring the same interface.
type ReliabilityReloader = wiring.ReliabilityReloader

// Deps carries every subsystem the per-key handlers touch. Each field may be
// nil when the corresponding wiring branch is disabled at startup; every
// handler tolerates nil and short-circuits to a no-op.
type Deps struct {
	Logger          *slog.Logger
	ThingID         string
	Outcomes        *thingclient.OutcomeTracker
	BootstrapConfig *config.Config

	DB                  *store.DB
	CacheLayer          *cachelayer.Layer
	CredManager         CredCacheInvalidator
	GeminiCacheMgrSet   *geminicache.ManagerSet
	HookConfigCache     HookConfigReloader                 // may be nil
	TelemetryProvider   *telemetry.SwappableTracerProvider // may be nil
	ObservabilityState  *atomic.Pointer[telemetry.Config]
	PayloadCaptureStore *payloadcapture.Store
	Reliability         ReliabilityReloader // may be nil
	PolicyCache         *quota.PolicyCache  // may be nil
	// AIGuardConfigCache is fetched via a getter because the singleton is
	// assigned after OnConfigChanged registration in main.go. Reading at
	// apply-time (not construction time) ensures the late assignment is
	// visible when shadow ticks arrive.
	AIGuardConfigCache func() *aiguard.ConfigCache
	NormEngine         *wirerewrite.Engine
	PassthroughCache   *passthrough.Cache

	// SemanticIndexLifecycle receives the latest ConfigSnapshot from the Hub
	// shadow and ensures the Valkey index is up to date. May be nil when the
	// semantic cache module is disabled at startup (e.g. Sentinel/Cluster Redis,
	// where the *redis.Client index management is unavailable).
	SemanticIndexLifecycle *semantic.IndexLifecycle
	// SemanticConfigCache is the in-process fleet semantic config snapshot. The
	// handler Sets it directly when SemanticIndexLifecycle is nil so the L1 hot
	// path still learns vary_by + the loaded signal on a degraded Redis topology.
	// Always non-nil in production (constructed even on Sentinel/Cluster).
	SemanticConfigCache *semantic.ConfigCache
	// FreshnessDetector holds the atomically-swappable time-sensitive rule set.
	// May be nil when freshness detection is disabled.
	FreshnessDetector *freshness.Detector

	// ResponseCache is the L1 (exact-match) response cache. The handler
	// atomically swaps enabled / TTL / applyFreshnessRules on every
	// response_cache.extract_config push without a service restart.
	// May be nil when the cache module is disabled at startup.
	ResponseCache *cache.Cache

	// OnModelsReloaded, when non-nil, is called after every successful model
	// snapshot reload. Wiring uses this to rebuild the capability pre-filter
	// snapshot without coupling configdispatch to the routing/capability package.
	OnModelsReloaded func(models []store.Model)
}

// errActiveKeyDepNil reports a wiring regression: a config key that IS
// registered in ValidByThingType["ai-gateway"] (so Hub actively pushes it) was
// applied while a dependency the gateway structurally cannot run without was
// nil. Returning this error (instead of the (nil,nil) "applied" no-op) makes
// the Loader record a FAILED outcome and withhold the reportedVer advance, so
// the Thing never falsely reports convergence on a half-wired gateway.
// This is reserved for deps that are NEVER legitimately nil in a
// healthy gateway (DB, CacheLayer, CredManager) — optional subsystems that are
// disabled at boot (response cache, semantic cache, freshness detector, etc.)
// keep their (nil,nil) no-op because nil there is a valid disabled state.
func errActiveKeyDepNil(key, dep string) error {
	return fmt.Errorf("configdispatch: %s applier: required dependency %s is nil "+
		"(wiring regression — the key is pushed but the subsystem is unwired)", key, dep)
}

func BuildConfigLoader(d Deps) *cfgloader.Loader {
	l := cfgloader.New(d.Logger, d.Outcomes, d.ThingID, "ai-gateway")

	registerAGRoutingRules(l, d)
	registerAGCredentials(l, d)
	registerAGProviders(l, d)
	registerAGModels(l, d)
	registerAGHookConfig(l, d)
	registerAGObservability(l, d)
	registerAGPayloadCapture(l, d)
	registerAGCredentialReliability(l, d)
	registerAGQuotaTriad(l, d)
	registerAGVirtualKeys(l, d)
	registerAGAIGuardConfig(l, d)
	registerAGCacheConfig(l, d)
	registerAGGatewayPassthroughConfig(l, d)
	registerAGLogLevel(l, d)
	registerAGSemanticCacheConfig(l, d)
	registerAGTimeSensitivePatterns(l, d)
	registerAGExtractCacheConfig(l, d)

	return l
}

func registerAGRoutingRules(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, "routing_rules", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// DB is structurally required — routing rules live in the DB and a
		// gateway with no DB cannot serve traffic.
		if d.DB == nil {
			return nil, errActiveKeyDepNil("routing_rules", "DB")
		}
		d.DB.InvalidateRuleCache()
		return nil, nil
	})
}

func registerAGCredentials(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, "credentials", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// CredManager + CacheLayer are structurally required — the gateway
		// cannot decrypt or route without them.
		if d.CredManager == nil {
			return nil, errActiveKeyDepNil("credentials", "CredManager")
		}
		if d.CacheLayer == nil {
			return nil, errActiveKeyDepNil("credentials", "CacheLayer")
		}
		// Granular invalidation: when Hub pushes a targeted
		// invalidate-by-id envelope, evict only the changed credentials'
		// decrypted entries instead of wiping the whole cache. A full
		// ClearCache on every credential change triggers a re-decrypt
		// storm for unrelated credentials. Fall back to ClearCache only
		// when the payload carries no specific IDs (full reload signal).
		ids := wiring.ParseInvalidateIDs(raw)
		if len(ids) > 0 {
			for _, id := range ids {
				d.CredManager.Invalidate(id)
			}
		} else {
			d.CredManager.ClearCache()
		}
		reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := d.CacheLayer.ReloadCredentials(reloadCtx); err != nil {
			return nil, err
		}
		return nil, nil
	})
}

func registerAGProviders(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, "providers", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// CacheLayer is structurally required — the provider snapshot is the
		// gateway's routing source of truth.
		if d.CacheLayer == nil {
			return nil, errActiveKeyDepNil("providers", "CacheLayer")
		}
		reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := d.CacheLayer.ReloadProviders(reloadCtx); err != nil {
			return nil, err
		}
		// Provider list changed: ManagerSet rebuilds per-provider Gemini managers
		// using the last cache blob already cached inside the ManagerSet.
		if d.GeminiCacheMgrSet != nil {
			d.GeminiCacheMgrSet.ReloadProviders()
		}
		return nil, nil
	})
}

func registerAGModels(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, "models", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// CacheLayer is structurally required — the model snapshot drives
		// routing, pricing, and capability resolution.
		if d.CacheLayer == nil {
			return nil, errActiveKeyDepNil("models", "CacheLayer")
		}
		reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := d.CacheLayer.ReloadModels(reloadCtx); err != nil {
			return nil, err
		}
		// Notify the capability cache so the embedding pre-filter stays in
		// sync with the updated Model rows.
		if d.OnModelsReloaded != nil {
			d.OnModelsReloaded(d.CacheLayer.AllModels())
		}
		return nil, nil
	})
}

func registerAGHookConfig(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, configkey.Hooks, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// best-effort: reload errors are already logged inside the cache;
		// the next request just falls back to the previous snapshot.
		if d.HookConfigCache == nil {
			return nil, nil
		}
		return nil, d.HookConfigCache.Reload(ctx)
	})
}

func registerAGObservability(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, "observability", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.TelemetryProvider == nil || d.DB == nil {
			return nil, nil
		}
		newCfg := wiring.InitOtelConfig(ctx, d.DB, d.BootstrapConfig)
		if err := d.TelemetryProvider.Reconfigure(newCfg); err != nil {
			return nil, err
		}
		if d.ObservabilityState != nil {
			d.ObservabilityState.Store(&newCfg)
		}
		return nil, nil
	})
}

func registerAGPayloadCapture(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, "payload_capture", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.DB == nil {
			return nil, nil
		}
		reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		pcCfg, err := wiring.LoadPayloadCaptureConfig(reloadCtx, d.DB)
		if err != nil {
			return nil, err
		}
		d.PayloadCaptureStore.Set(pcCfg)
		d.Logger.Info("payload capture config reloaded",
			"storeRequestBody", pcCfg.StoreRequestBody,
			"storeResponseBody", pcCfg.StoreResponseBody,
			"maxInlineBodyBytes", pcCfg.MaxInlineBodyBytes,
			"maxRequestBytes", pcCfg.MaxRequestBytes,
			"maxResponseBytes", pcCfg.MaxResponseBytes,
		)
		return nil, nil
	})
}

// NOTE: ai-gateway intentionally has NO streaming_compliance applier.
// `streaming_compliance` is absent from ValidByThingType["ai-gateway"]
// (packages/shared/schemas/configkey/validation.go), so the CP write path
// never fans the key out to ai-gateway — a registered applier would be dead
// code that can never run. The gateway's streaming compliance policy is
// seeded once at boot from system_metadata via streampolicy.BootStore
// (wiring.InitStreamingPolicyStore) and read by the proxy SSE handler; live
// admin changes to that policy reach the interception nodes (compliance-proxy
// + agent), which DO carry the key, not ai-gateway. The previous "three-service
// alignment" applier here was a propagation no-op (deleted).

func registerAGCredentialReliability(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, "credential_reliability", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// Global circuit / health thresholds reloaded from system_metadata.
		// Per-credential overrides ride on the cachelayer Credential snapshot
		// and need no separate reload.
		if d.Reliability == nil || d.DB == nil {
			return nil, nil
		}
		reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return nil, d.Reliability.Reload(reloadCtx, d.DB)
	})
}

// registerAGQuotaTriad wires quota_policies, quota_overrides, and organizations
// to the same applier — all three invalidate the policy cache. Wiring
// "organizations" ensures admin renames and re-parents reach OrgParents within
// one Hub broadcast instead of waiting for an ai-gateway restart.
func registerAGQuotaTriad(l *cfgloader.Loader, d Deps) {
	reload := func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.PolicyCache == nil {
			return nil, nil
		}
		return nil, d.PolicyCache.Load(ctx)
	}
	cfgloader.RegisterRaw(l, "quota_policies", reload)
	cfgloader.RegisterRaw(l, "quota_overrides", reload)
	cfgloader.RegisterRaw(l, "organizations", reload)
}

func registerAGVirtualKeys(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, "virtual_keys", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// CacheLayer is structurally required — VK auth resolves against the
		// VK snapshot it owns.
		if d.CacheLayer == nil {
			return nil, errActiveKeyDepNil("virtual_keys", "CacheLayer")
		}
		// Targeted invalidate by hash: payload may carry a list of
		// affected hashes via the Hub invalidate-by-id form; fall
		// back to a full purge if none provided.
		hashes := wiring.ParseInvalidateIDs(raw)
		if len(hashes) > 0 {
			d.CacheLayer.InvalidateVirtualKeys(hashes...)
		} else {
			d.CacheLayer.PurgeVirtualKeys()
		}
		return nil, nil
	})
}

func registerAGAIGuardConfig(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, configkey.AIGuard, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// Read via getter: the singleton is assigned after loader construction in main.go.
		if d.AIGuardConfigCache == nil {
			return nil, nil
		}
		cache := d.AIGuardConfigCache()
		if cache != nil {
			cache.Invalidate()
		}
		return nil, nil
	})
}

func registerAGCacheConfig(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, configkey.Cache, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// Single shadow key carrying the full 3-tier prompt cache config
		// (global / adapter / per-provider). Drives both the normaliser engine
		// (per-adapter rules + per-provider effective markers) and the
		// geminicache ManagerSet (per-provider effective Gemini knobs).
		if len(raw) == 0 {
			return nil, nil
		}
		var blob cacheconfig.CacheConfigBlob
		if err := json.Unmarshal(raw, &blob); err != nil {
			return nil, fmt.Errorf("cache parse: %w", err)
		}
		// Emergency master kill switch (Tier-1 global). Wire it into the L1
		// Cache object, which the proxy cache stage reads to gate BOTH L1 and
		// L2 response caching. Without this the switch was parsed but never
		// consulted by the data plane. Provider-side caching (L3 markers /
		// Gemini context cache) is intentionally NOT gated here — it makes the
		// upstream cache (cost/latency only), never serves a stored response,
		// so it is not "gateway response caching" the emergency switch targets.
		if d.ResponseCache != nil {
			d.ResponseCache.SetMasterKill(blob.Global.CacheMasterKillSwitch)
		}
		if d.GeminiCacheMgrSet != nil {
			d.GeminiCacheMgrSet.SetConfig(blob)
		}
		if d.NormEngine != nil {
			d.NormEngine.Reload(wiring.ProjectCacheBlobToNormaliserConfig(blob, d.CacheLayer))
		}
		return nil, nil
	})
}

func registerAGGatewayPassthroughConfig(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, configkey.GatewayPassthrough, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// Cold-start contract: an empty snapshot disables every bypass (fail-closed).
		if d.PassthroughCache == nil {
			return nil, nil
		}
		if len(raw) == 0 {
			d.PassthroughCache.SetSnapshot(nil)
			return nil, nil
		}
		var snap passthrough.Snapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			d.Logger.Warn("gateway_passthrough parse failed; preserving prior snapshot", "error", err)
			return nil, fmt.Errorf("gateway_passthrough parse: %w", err)
		}
		d.PassthroughCache.SetSnapshot(&snap)
		d.Logger.Info("passthrough config updated",
			"global_enabled", snap.Global.Enabled,
			"adapters", len(snap.Adapters),
			"providers", len(snap.Providers),
		)
		return nil, nil
	})
}

// semanticCacheConfigBlob is the JSON shape pushed by Hub for the
// semantic_cache.config shadow key.  It mirrors configstore.SemanticCacheConfigRow
// exactly; a local copy avoids an import cycle between this package and
// packages/shared/storage/configstore.
type semanticCacheConfigBlob struct {
	EmbeddingProviderID  *string `json:"embeddingProviderId"`
	EmbeddingModelID     *string `json:"embeddingModelId"`
	EmbeddingDimension   *int    `json:"embeddingDimension"`
	EmbeddingFingerprint string  `json:"embeddingFingerprint"`
	RedisIndexName       string  `json:"redisIndexName"`
	Enabled              bool    `json:"enabled"`
	Threshold            float32 `json:"threshold"`
	VaryBy               string  `json:"varyBy"`
	EmbedStrategy        string  `json:"embedStrategy"`
	AllowCrossModel      bool    `json:"allowCrossModel"`
	// Provider join — pushed by CP so the gateway doesn't have to look
	// these up per-request on the L2 hot path.
	EmbeddingProviderBaseURL      string  `json:"embeddingProviderBaseUrl,omitempty"`
	EmbeddingProviderModelID      string  `json:"embeddingProviderModelId,omitempty"`
	EmbeddingInputPricePerMillion float64 `json:"embeddingInputPricePerMillion,omitempty"`
	EmbeddingMaxInputTokens       int     `json:"embeddingMaxInputTokens,omitempty"`
}

// registerAGSemanticCacheConfig registers the semantic_cache.config handler.
// On each Hub push the handler:
//  1. Decodes the blob into a semantic.ConfigSnapshot (empty payload → disabled).
//  2. Applies it via applySemanticSnapshot.
//
// The in-process ConfigCache update is DECOUPLED from index lifecycle: the
// snapshot carries fleet config (vary_by, enabled, threshold) the L1 + L2 hot
// path needs even on a Sentinel/Cluster Redis where the *redis.Client index
// management (IndexLifecycle) is unavailable. Coupling the two previously left
// the ConfigCache un-Set on Sentinel, so L1's ScopeReady() never flipped and
// vary_by never reached the gateway.
func registerAGSemanticCacheConfig(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, configkey.SemanticCacheConfig, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.SemanticIndexLifecycle == nil && d.SemanticConfigCache == nil {
			return nil, nil
		}
		if len(raw) == 0 {
			// Empty payload: disable the semantic cache.
			applySemanticSnapshot(ctx, d, semantic.ConfigSnapshot{Enabled: false})
			return nil, nil
		}
		var blob semanticCacheConfigBlob
		if err := json.Unmarshal(raw, &blob); err != nil {
			return nil, fmt.Errorf("semantic_cache.config parse: %w", err)
		}

		snap := semantic.ConfigSnapshot{
			Enabled:                       blob.Enabled,
			Fingerprint:                   blob.EmbeddingFingerprint,
			RedisIndexName:                blob.RedisIndexName,
			Threshold:                     blob.Threshold,
			VaryBy:                        blob.VaryBy,
			EmbedStrategy:                 blob.EmbedStrategy,
			AllowCrossModel:               blob.AllowCrossModel,
			EmbeddingProviderBaseURL:      blob.EmbeddingProviderBaseURL,
			EmbeddingProviderModelID:      blob.EmbeddingProviderModelID,
			EmbeddingInputPricePerMillion: blob.EmbeddingInputPricePerMillion,
			EmbeddingMaxInputTokens:       blob.EmbeddingMaxInputTokens,
		}
		if blob.EmbeddingProviderID != nil {
			snap.EmbeddingProviderID = *blob.EmbeddingProviderID
		}
		if blob.EmbeddingModelID != nil {
			snap.EmbeddingModelID = *blob.EmbeddingModelID
		}
		if blob.EmbeddingDimension != nil {
			snap.EmbeddingDimension = *blob.EmbeddingDimension
		}

		applySemanticSnapshot(ctx, d, snap)
		return nil, nil
	})
}

// applySemanticSnapshot pushes a semantic ConfigSnapshot into the data plane.
// When the index lifecycle is wired (normal *redis.Client topology) it owns the
// ConfigCache.Set plus EnsureIndex. When it is nil (Sentinel/Cluster degraded
// L2) the ConfigCache is Set directly so vary_by + enabled still reach the
// hot path — L1 exact-match runs on every Redis topology and folds vary_by into
// its key, so it must learn the fleet scope regardless of L2 index availability.
func applySemanticSnapshot(ctx context.Context, d Deps, snap semantic.ConfigSnapshot) {
	if d.SemanticIndexLifecycle != nil {
		d.SemanticIndexLifecycle.OnConfigSnapshot(ctx, snap)
		return
	}
	if d.SemanticConfigCache != nil {
		d.SemanticConfigCache.Set(snap)
	}
}

// extractCacheConfigBlob mirrors the JSON Hub-shadow payload for
// response_cache.extract_config. Field names match the singleton row's JSON
// tags so the blob serialises round-trip from CP to AI GW.
type extractCacheConfigBlob struct {
	Enabled             bool `json:"enabled"`
	TTLSeconds          int  `json:"ttlSeconds"`
	ApplyFreshnessRules bool `json:"applyFreshnessRules"`
}

// registerAGExtractCacheConfig registers the response_cache.extract_config handler.
// On each Hub push it atomically swaps the cache.Cache config (enabled, TTL,
// freshness-rule gate) without a service restart.
// Empty payload disables the cache (defensive default).
func registerAGExtractCacheConfig(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, configkey.ResponseCacheExtractConfig, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.ResponseCache == nil {
			return nil, nil
		}
		if len(raw) == 0 {
			d.ResponseCache.SetConfig(cache.ConfigSnapshot{Enabled: false})
			return nil, nil
		}
		var blob extractCacheConfigBlob
		if err := json.Unmarshal(raw, &blob); err != nil {
			return nil, fmt.Errorf("response_cache.extract_config parse: %w", err)
		}
		ttl := time.Duration(blob.TTLSeconds) * time.Second
		d.ResponseCache.SetConfig(cache.ConfigSnapshot{
			Enabled:             blob.Enabled,
			TTL:                 ttl,
			ApplyFreshnessRules: blob.ApplyFreshnessRules,
		})
		d.Logger.Info("extract-cache config reloaded",
			"enabled", blob.Enabled,
			"ttl_seconds", blob.TTLSeconds,
			"apply_freshness_rules", blob.ApplyFreshnessRules)
		return nil, nil
	})
}

// registerAGTimeSensitivePatterns registers the response_cache.time_sensitive_patterns
// handler.  On each Hub push the handler decodes the JSON array of freshness.Rule
// values and atomically reloads the Detector.
func registerAGTimeSensitivePatterns(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, configkey.ResponseCacheTimeSensitivePatterns, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.FreshnessDetector == nil {
			return nil, nil
		}
		var rules []freshness.Rule
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rules); err != nil {
				return nil, fmt.Errorf("response_cache.time_sensitive_patterns parse: %w", err)
			}
		}
		if err := d.FreshnessDetector.Reload(rules); err != nil {
			return nil, fmt.Errorf("response_cache.time_sensitive_patterns reload: %w", err)
		}
		d.Logger.Info("time-sensitive patterns reloaded", "count", len(rules))
		return nil, nil
	})
}

type agLogLevelState struct {
	Level string `json:"level"`
}

func registerAGLogLevel(l *cfgloader.Loader, d Deps) {
	cfgloader.Register(l, cfgloader.Handler[agLogLevelState]{
		Key:   "log_level",
		Parse: cfgloader.ParseJSON[agLogLevelState](),
		Apply: func(ctx context.Context, v agLogLevelState, ver int64) ([]byte, error) {
			// slog.LevelVar is set once at NewLogger and consulted on every record,
			// so the swap takes effect immediately without rebuilding any logger.
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
