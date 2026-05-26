package providers

// ValidAdapterTypes is the authoritative set of Provider.adapter_type
// values accepted by the admin API. Each entry matches the AI Gateway
// providers.Format enum and the non-fallback IDs in
// shared/traffic/adapters. Keep this list in lockstep with
// packages/ai-gateway/internal/providers/types.go (Format / AllFormats);
// no DB CHECK enforces the enum, so the Control Plane handler is the
// validation gate (mirrored in docs/users/api/openapi/ai-gateway/e30-s2-provider-adapter-type.yaml).
var ValidAdapterTypes = []string{
	"openai",
	"anthropic",
	"gemini",
	"glm",
	"deepseek",
	"azure-openai",
	"minimax",
	"bedrock",
	"vertex",
	// OpenAI-compat re-users — distinct adapterType so per-vendor audit /
	// metrics / rate-limit policies can target them without name matching.
	"cohere",
	"huggingface",
	"replicate",
	"mistral",
	"xai",
	"groq",
	"perplexity",
	"together",
	"fireworks",
	"moonshot",
}

// IsValidAdapterType reports whether v is one of the canonical
// Provider.adapter_type values. Matching is case-sensitive — the
// handler must receive the exact canonical slug (letters / hyphens in
// lowercase).
func IsValidAdapterType(v string) bool {
	for _, candidate := range ValidAdapterTypes {
		if candidate == v {
			return true
		}
	}
	return false
}
