// Package handler — traffic_adapter.go bridges the ai-gateway
// [provcore.Format] enum to the `shared/traffic/adapters` registry ID
// space. Hook content extraction and rewrite are implemented in the
// shared traffic adapters; the AI Gateway picks the correct adapter
// per request based on the detected ingress body format stamped on the
// request context by [Handler.ServeProxy].
//
// The mapping is one-to-one with every [provcore.Format] enum value;
// only `openai → openai-compat` is a non-identity rename (the traffic
// registry uses the longer suffix to disambiguate from third-party
// "openai" naming). The pair MUST stay synchronised with
// `shared/traffic/adapters/builtins.go`; the exhaustiveness test in
// `traffic_adapter_test.go` enforces this.
package proxy

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// formatToTrafficAdapterID maps a provider wire format to its
// traffic-adapter registry ID. The mapping is total over
// [provcore.Format]; the default branch exists as a runtime safety
// net so an unknown Format does not crash the hook pipeline — the
// caller falls back to `generic-jsonpath`.
func formatToTrafficAdapterID(f provcore.Format) string {
	switch f {
	case provcore.FormatOpenAI:
		return "openai-compat"
	case provcore.FormatDeepSeek:
		return "deepseek"
	case provcore.FormatGLM:
		return "glm"
	case provcore.FormatAzureOpenAI:
		return "azure-openai"
	case provcore.FormatAnthropic:
		return "anthropic"
	case provcore.FormatGemini:
		return "gemini"
	case provcore.FormatMiniMax:
		return "minimax"
	case provcore.FormatBedrock:
		return "bedrock"
	case provcore.FormatVertex:
		return "vertex"
	case provcore.FormatCohere:
		return "cohere"
	case provcore.FormatHuggingFace:
		return "huggingface"
	case provcore.FormatReplicate:
		return "replicate"
	case provcore.FormatMistral:
		return "mistral"
	case provcore.FormatXai:
		return "xai"
	case provcore.FormatGroq:
		return "groq"
	case provcore.FormatPerplexity:
		return "perplexity"
	case provcore.FormatTogether:
		return "together"
	case provcore.FormatFireworks:
		return "fireworks"
	case provcore.FormatMoonshot:
		return "moonshot"
	case provcore.FormatVoyage:
		return "voyage"
	default:
		return "generic-jsonpath"
	}
}

// trafficAdapterFor returns a `shared/traffic.Adapter` instance for the
// given ingress format. It looks up the factory in the registry wired
// into [Deps.TrafficAdapters] and instantiates a fresh adapter — the
// factory contract is "new instance per call" so that adapters with
// per-request state are safe.
//
// Production code must wire [Deps.TrafficAdapters] in `main.go`; unit
// tests that only exercise a single format can set [Deps.TrafficAdapter]
// directly instead. When neither is configured the function returns nil
// and callers are expected to skip hook extraction/rewrite for that
// request — this is a programming error in production and is caught by
// wiring tests.
//
// If the registry is wired but a known format maps to an unregistered
// adapter ID (unreachable in practice thanks to the exhaustive switch
// above plus the builtins contract in `shared/traffic/adapters`), the
// function logs a warning and falls back to `generic-jsonpath`.
func (h *Handler) trafficAdapterFor(format provcore.Format) traffic.Adapter {
	if h == nil || h.deps == nil {
		return nil
	}
	reg := h.deps.TrafficAdapters
	if reg == nil {
		// Test escape hatch: a single-adapter Handler can be built by
		// setting Deps.TrafficAdapter instead of constructing a full
		// registry. Not used in production.
		return h.deps.TrafficAdapter
	}
	id := formatToTrafficAdapterID(format)
	if factory := reg.Get(id); factory != nil {
		return factory()
	}
	if h.deps.Logger != nil {
		h.deps.Logger.Warn("traffic adapter not registered; falling back to generic-jsonpath",
			"format", string(format), "adapter_id", id)
	}
	if factory := reg.Get("generic-jsonpath"); factory != nil {
		return factory()
	}
	// Registry wired but missing both the format-specific adapter and
	// the generic fallback. Return nil; callers handle this explicitly.
	return nil
}
