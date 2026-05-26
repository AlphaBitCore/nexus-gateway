// runtimeapi.go — runtime shadow API server + introspection registry wiring.
package wiring

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/runtimeapi"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// IntrospectDeps groups the optional subsystems the runtime introspection
// handler reads from. Each field may be nil; the handler guards accordingly.
type IntrospectDeps struct {
	AgID                string
	BuildVersion        string
	CacheLayer          *cachelayer.Layer
	PolicyCache         *quota.PolicyCache
	PayloadCaptureStore *payloadcapture.Store
	HookConfigCache     *pipeline.HookConfigCache
	AIGuardConfigCache  func() *aiguard.ConfigCache
	ObservabilityGet    func() *telemetry.Config
	ConfigKeyRecorder   *runtimeintrospect.KeyStateRecorder
	AuthToken           string
}

// InitIntrospectRegistry builds the runtime introspection registry and mounts
// /debug/runtime on mux.
func InitIntrospectRegistry(deps IntrospectDeps, mux *http.ServeMux) *runtimeintrospect.Registry {
	introspectReg := runtimeintrospect.New("ai-gateway", deps.AgID, deps.BuildVersion)

	if deps.PayloadCaptureStore != nil {
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "config.payload_capture",
			Fn: func(_ context.Context) (any, error) {
				return deps.PayloadCaptureStore.Get(), nil
			},
		})
	}
	if deps.CacheLayer != nil {
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "cache.cachelayer.stats",
			Fn: func(_ context.Context) (any, error) {
				return deps.CacheLayer.Stats(), nil
			},
		})
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "cache.routing_rules",
			Fn: func(ctx context.Context) (any, error) {
				return deps.CacheLayer.GetEnabledRoutingRules(ctx)
			},
		})
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "cache.models",
			Fn: func(ctx context.Context) (any, error) {
				return deps.CacheLayer.ListEnabledModels(ctx)
			},
		})
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "cache.providers",
			Fn: func(_ context.Context) (any, error) {
				all := deps.CacheLayer.ProvidersAll()
				out := make([]map[string]any, 0, len(all))
				for _, p := range all {
					out = append(out, map[string]any{
						"id": p.ID, "name": p.Name, "adapterType": p.AdapterType,
						"baseUrl": p.BaseURL, "enabled": p.Enabled,
					})
				}
				return out, nil
			},
		})
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "cache.credentials",
			Fn: func(_ context.Context) (any, error) {
				all := deps.CacheLayer.CredentialsAll()
				out := make([]map[string]any, 0, len(all))
				for _, c := range all {
					out = append(out, map[string]any{
						"id": c.ID, "name": c.Name, "providerId": c.ProviderID,
						"enabled": c.Enabled, "encryptionKeyId": c.EncryptionKeyID,
						"encryptedKeyLength": len(c.EncryptedKey),
					})
				}
				return out, nil
			},
		})
	}
	if deps.ObservabilityGet != nil {
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "config.observability",
			Fn: func(_ context.Context) (any, error) {
				snap := deps.ObservabilityGet()
				if snap == nil {
					return nil, nil
				}
				return *snap, nil
			},
		})
	}
	if deps.PolicyCache != nil {
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "cache.policy_cache.org_parents",
			Fn: func(_ context.Context) (any, error) {
				return map[string]any{"org_parents": deps.PolicyCache.OrgParents()}, nil
			},
		})
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "cache.quota_policies",
			Fn: func(_ context.Context) (any, error) {
				return deps.PolicyCache.PolicySnapshot(), nil
			},
		})
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "cache.quota_overrides",
			Fn: func(_ context.Context) (any, error) {
				return deps.PolicyCache.OverrideSnapshot(), nil
			},
		})
	}
	if deps.HookConfigCache != nil {
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "config.hooks",
			Fn: func(_ context.Context) (any, error) {
				return deps.HookConfigCache.Snapshot(), nil
			},
		})
	}
	if deps.AIGuardConfigCache != nil {
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "config.ai_guard",
			Fn: func(ctx context.Context) (any, error) {
				cache := deps.AIGuardConfigCache()
				if cache == nil {
					return nil, nil
				}
				cfg, err := cache.Get(ctx)
				if err != nil || cfg == nil {
					return nil, err
				}
				headerKeys := make([]string, 0, len(cfg.CustomHeaders))
				for k := range cfg.CustomHeaders {
					headerKeys = append(headerKeys, k)
				}
				return map[string]any{
					"id": cfg.ID, "backendMode": cfg.BackendMode,
					"providerId": cfg.ProviderID, "modelId": cfg.ModelID,
					"externalUrl": cfg.ExternalURL, "externalCredentialId": cfg.ExternalCredentialID,
					"customHeaderKeys": headerKeys, "promptTemplate": cfg.PromptTemplate,
					"timeoutMs": cfg.TimeoutMs, "cacheTtlSeconds": cfg.CacheTTLSeconds,
					"backendFingerprint": cfg.BackendFingerprint,
				}, nil
			},
		})
	}
	if deps.ConfigKeyRecorder != nil {
		deps.ConfigKeyRecorder.RegisterAll(introspectReg, []string{
			configkey.LogLevel,
			configkey.Cache,
			configkey.GatewayPassthrough,
			configkey.CredentialReliability,
			configkey.VirtualKeys,
		})
	}
	mux.Handle("GET /debug/runtime", introspectReg.Handler(runtimeintrospect.HandlerOptions{
		Token:  deps.AuthToken,
		Logger: slog.Default(),
	}))
	return introspectReg
}

// MountRuntimeAPI mounts the /runtime/* API surface when thingClient is available.
func MountRuntimeAPI(thingClient *thingclient.Client, mux *http.ServeMux) {
	if thingClient == nil {
		return
	}
	apiToken := os.Getenv("AI_GATEWAY_API_TOKEN")
	if apiToken == "" {
		slog.Warn("AI_GATEWAY_API_TOKEN not set; /runtime/* will reject all requests")
	}
	rtServer := runtimeapi.New(runtimeapi.Config{
		APIToken: apiToken,
		Thing:    thingClient,
		Logger:   slog.Default(),
	})
	rtServer.Mount(mux)
}
