// Per-model passthrough rewrites owned by moonshot.
//
// Per provider-adapter-architecture.md §3a Rule 3, Moonshot's per-model
// wire quirks (kimi-k2.5 / kimi-k2.6 require temperature=1 and reject
// any other value with HTTP 400) live with the Moonshot adapter, not
// in the generic providers/spec_adapter.go layer. The passthrough
// dispatch reaches us via the AdapterSpec.PassthroughRewrite callback
// wired in NewSpec.
package moonshot

import "strings"

// IsFixedTempModel reports whether the Moonshot model id belongs to a
// family that hardcodes temperature on the upstream side and rejects
// any caller-supplied value with HTTP 400 "invalid temperature: only
// 1 is allowed for this model." Older kimi-k2-thinking and
// moonshot-v1-* models accept arbitrary temperature.
//
// Observed (2026-05, direct calls to api.moonshot.cn): kimi-k2.5,
// kimi-k2.6.
func IsFixedTempModel(modelID string) bool {
	switch {
	case strings.HasPrefix(modelID, "kimi-k2.5"),
		strings.HasPrefix(modelID, "kimi-k2.6"):
		return true
	}
	return false
}

// ApplyRewrites strips the caller's temperature (and any companion
// top_p) on fixed-temp Moonshot models so the upstream applies its
// mandatory =1 default — sending any other value hard-fails the
// request. Returns the rewrites applied or nil when modelID is not
// in the fixed-temp family.
//
// Wired into AdapterSpec.PassthroughRewrite in NewSpec; consumed by
// the generic spec_adapter.PrepareBody passthrough path.
func ApplyRewrites(payload map[string]any, modelID string) []string {
	if !IsFixedTempModel(modelID) {
		return nil
	}
	var rewrites []string
	if _, ok := payload["temperature"]; ok {
		delete(payload, "temperature")
		rewrites = append(rewrites, "temperature→removed")
	}
	if _, ok := payload["top_p"]; ok {
		delete(payload, "top_p")
		rewrites = append(rewrites, "top_p→removed")
	}
	return rewrites
}
