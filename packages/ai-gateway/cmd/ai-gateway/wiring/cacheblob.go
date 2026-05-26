// cacheblob.go — cache blob → wirerewrite.Config projection.
package wiring

import (
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// ProjectCacheBlobToNormaliserConfig converts the 3-tier cache blob into the
// wirerewrite.Config the L0/L3 pipeline consumes:
//
//  1. NormaliserEnabled  ← Tier 1.
//  2. Rules[adapterType] ← Tier 2 per-adapter rules sub-map.
//  3. Providers[providerID] ← effective Anthropic/Bedrock marker config.
//
// Gemini-family Tier 2/3 fields drive the separate geminicache.ManagerSet and
// are intentionally NOT projected here.
func ProjectCacheBlobToNormaliserConfig(blob cacheconfig.CacheConfigBlob, layer *cachelayer.Layer) wirerewrite.Config {
	out := wirerewrite.Config{
		NormaliserEnabled: blob.Global.NormaliserEnabled,
		Rules:             map[string]map[string]wirerewrite.RuleOverride{},
		Providers:         map[string]wirerewrite.ProviderCacheConfig{},
	}

	for adapter, ac := range blob.Adapters {
		if len(ac.Rules) == 0 {
			continue
		}
		dst := map[string]wirerewrite.RuleOverride{}
		for ruleID, ro := range ac.Rules {
			dst[ruleID] = wirerewrite.RuleOverride{
				Enabled:      ro.Enabled,
				DryRunAlways: ro.DryRunAlways,
			}
		}
		out.Rules[adapter] = dst
	}

	if layer != nil {
		all := layer.ProvidersAll()
		for pid, p := range all {
			if p.AdapterType != "anthropic" && p.AdapterType != "bedrock" {
				continue
			}
			eff := cacheconfig.Resolve(blob, pid, p.AdapterType)
			out.Providers[pid] = wirerewrite.ProviderCacheConfig{
				CacheMarkerInjectEnabled:    eff.MarkerInjectEnabled,
				CacheMarkerBoundary3Enabled: eff.MarkerBoundary3Enabled,
			}
		}
	}
	return out
}
