package wirerewrite

import (
	"regexp"
)

// Canonical bundled rule IDs.
const (
	RuleAnthropicCchStrip = "claude-code-cch-strip"
	RuleBedrockCchStrip   = "bedrock-claude-cch-strip"
)

// bundledRules returns the factory-default rule definitions. These are
// cloned and merged with operator config overrides on every Engine reload.
//
// Only surgical, opt-in strip rules ship here: each removes a single known
// volatile token (a billing nonce) from a precise body path via regex, so the
// forwarded request stays byte-identical to the client's intent apart from that
// one token. Whole-body re-serialisation (e.g. JSON field-order canonicalisation)
// is intentionally NOT offered: re-encoding the client's body to stabilise a
// cache key rewrites bytes the client chose (number formatting, escaping, key
// order) and changes what is sent upstream — an unacceptable risk for a
// passthrough gateway, and the dominant per-request allocation when enabled.
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
	}
}
