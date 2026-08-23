// reasoning.go — the canonical LEVEL translated onto this wire's BUDGET.
//
// Split out of codec.go along the same seam the Anthropic adapter used for its
// sampling policy: one concern, one audience, and the file-size ratchet.
package codec

import (
	"strings"

	"github.com/tidwall/gjson"
)

// thinkingConfigFromCanonicalEffort converts the canonical LEVEL into this
// wire's own shape, or nil when the caller expressed nothing usable.
//
// Every recognised level becomes `thinkingBudget: -1` — Gemini for "you
// decide" — rather than a number, and that is the honest translation rather
// than a lossy one. This wire has NO probed minimum or maximum budget anywhere
// in the repo, unlike Anthropic's 1024 floor, so a level-to-number mapping
// would be an invented range, and an invented range is an upstream 400 wearing
// a feature's clothes. `-1` delivers what the caller asked for — reasoning —
// and leaves the amount to the party that actually knows it.
//
// The gradation between "low" and "high" is genuinely lost, and the coerced
// self-report says so, so the caller can see we chose for them. Recovering it
// needs probed per-model ranges; until those exist, saying "think, you decide"
// is true, while saying "think exactly this much" would not be.
//
// "none" produces nothing. This wire's spelling for a disable is not
// established here — 0 is a guess, not a probed fact — and omitting the block
// is what the codec did before, so declining changes nothing rather than
// asserting something unproven.
func thinkingConfigFromCanonicalEffort(canonicalBody []byte) map[string]any {
	effort := gjson.GetBytes(canonicalBody, "reasoning_effort")
	if !effort.Exists() || effort.Type != gjson.String {
		return nil
	}
	switch strings.ToLower(effort.String()) {
	case "minimal", "low", "medium", "high", "max":
		return map[string]any{"thinkingBudget": -1}
	default:
		// "none", and any level this wire's vocabulary does not contain.
		return nil
	}
}
