package wirerewrite

import (
	"regexp"
)

// Canonical bundled rule IDs.
const (
	RuleAnthropicCchStrip         = "claude-code-cch-strip"
	RuleBedrockCchStrip           = "bedrock-claude-cch-strip"
	RuleOpenAIFieldOrderNormalize = "openai-field-order-normalize"
	RuleAzureOpenAIFieldOrder     = "azure-openai-field-order-normalize"
	RuleDeepSeekFieldOrder        = "deepseek-field-order-normalize"
	RuleGLMFieldOrder             = "glm-field-order-normalize"
	RuleMoonshotFieldOrder        = "moonshot-field-order-normalize"
	RuleMistralFieldOrder         = "mistral-field-order-normalize"
	RuleXaiFieldOrder             = "xai-field-order-normalize"
	RuleGroqFieldOrder            = "groq-field-order-normalize"
	RulePerplexityFieldOrder      = "perplexity-field-order-normalize"
	RuleTogetherFieldOrder        = "together-field-order-normalize"
	RuleFireworksFieldOrder       = "fireworks-field-order-normalize"
	RuleMiniMaxFieldOrder         = "minimax-field-order-normalize"
)

// bundledRules returns the factory-default rule definitions. These are
// cloned and merged with operator config overrides on every Engine reload.
func bundledRules() []Rule {
	cchRe := regexp.MustCompile(`cch=[0-9a-f]+;?\s*`)
	return []Rule{
		{
			// Strip Claude Code's billing nonce from Anthropic system prompts.
			// The token appears as "cch=<hex>; " or "cch=<hex>" within the
			// text field of system content blocks. Removing it makes cache
			// keys stable across consecutive Claude Code sessions that share
			// an identical system prompt.
			ID:               RuleAnthropicCchStrip,
			AdapterType:      AdapterAnthropic,
			Type:             RuleTypeStrip,
			EnabledByDefault: false,
			KeyNormalizeSafe: true,
			BodyPath:         "system.#.text",
			Regex:            cchRe,
		},
		{
			// Same as claude-code-cch-strip but for Bedrock-wire Claude requests.
			// Bedrock uses the Anthropic Messages format on the wire, so the
			// cch= nonce appears in the same location.
			ID:               RuleBedrockCchStrip,
			AdapterType:      AdapterBedrock,
			Type:             RuleTypeStrip,
			EnabledByDefault: false,
			KeyNormalizeSafe: true,
			BodyPath:         "system.#.text",
			Regex:            cchRe,
		},
		{
			// Canonicalise top-level JSON field order for OpenAI-wire bodies.
			// Go's encoding/json sorts map keys alphabetically; applying this
			// to PrepareBody output neutralises SDK-specific field orderings
			// (Python vs Go vs TS SDKs) before hashing.
			ID:               RuleOpenAIFieldOrderNormalize,
			AdapterType:      AdapterOpenAI,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
		{
			// Azure OpenAI uses identical wire format to OpenAI — same field
			// order normalisation guarantees stable cache keys across SDK versions.
			ID:               RuleAzureOpenAIFieldOrder,
			AdapterType:      AdapterAzureOpenAI,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
		{
			// DeepSeek OpenAI-compat wire — canonicalise field order.
			ID:               RuleDeepSeekFieldOrder,
			AdapterType:      AdapterDeepSeek,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
		{
			// GLM OpenAI-compat wire — canonicalise field order.
			ID:               RuleGLMFieldOrder,
			AdapterType:      AdapterGLM,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
		{
			// Moonshot OpenAI-compat wire — canonicalise field order.
			ID:               RuleMoonshotFieldOrder,
			AdapterType:      AdapterMoonshot,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
		{
			// Mistral OpenAI-compat wire — canonicalise field order.
			ID:               RuleMistralFieldOrder,
			AdapterType:      AdapterMistral,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
		{
			// xAI (Grok) OpenAI-compat wire — canonicalise field order.
			ID:               RuleXaiFieldOrder,
			AdapterType:      AdapterXai,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
		{
			// Groq OpenAI-compat wire — canonicalise field order.
			ID:               RuleGroqFieldOrder,
			AdapterType:      AdapterGroq,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
		{
			// Perplexity OpenAI-compat wire — canonicalise field order.
			ID:               RulePerplexityFieldOrder,
			AdapterType:      AdapterPerplexity,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
		{
			// Together AI OpenAI-compat wire — canonicalise field order.
			ID:               RuleTogetherFieldOrder,
			AdapterType:      AdapterTogether,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
		{
			// Fireworks AI OpenAI-compat wire — canonicalise field order.
			ID:               RuleFireworksFieldOrder,
			AdapterType:      AdapterFireworks,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
		{
			// MiniMax OpenAI-compat wire — canonicalise field order.
			ID:               RuleMiniMaxFieldOrder,
			AdapterType:      AdapterMiniMax,
			Type:             RuleTypeFieldOrder,
			EnabledByDefault: true,
			KeyNormalizeSafe: true,
		},
	}
}
