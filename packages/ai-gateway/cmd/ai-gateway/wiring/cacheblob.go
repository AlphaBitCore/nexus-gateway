// cacheblob.go — cache blob → wirerewrite.Config projection.
package wiring

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// ProjectCacheBlobToNormaliserConfig converts the cache blob into the
// wirerewrite.Config the L0/L3 strip pipeline consumes: the Tier-2 per-adapter
// rule overrides, and nothing else. The engine derives its own hasWork gate
// from them, so enabling a rule IS the demand and there is no global switch.
//
// It projects no per-provider state. The blob's Anthropic marker settings are
// read per request by the codec that writes the marker, and the Gemini-family
// fields drive the separate geminicache.ManagerSet. An earlier version
// resolved the marker settings here against a snapshot of the provider list,
// which silently disabled markers for any provider created after the last
// cache push — and for the whole process when a cold start applied `cache`
// before `providers`, an order the config loader does not guarantee.
func ProjectCacheBlobToNormaliserConfig(blob cacheconfig.CacheConfigBlob) wirerewrite.Config {
	out := wirerewrite.Config{Rules: map[string]map[string]wirerewrite.RuleOverride{}}
	for adapter, ac := range blob.Adapters {
		if len(ac.Rules) == 0 {
			continue
		}
		dst := make(map[string]wirerewrite.RuleOverride, len(ac.Rules))
		for ruleID, ro := range ac.Rules {
			dst[ruleID] = wirerewrite.RuleOverride{Enabled: ro.Enabled, DryRunAlways: ro.DryRunAlways}
		}
		out.Rules[adapter] = dst
	}
	return out
}
