package estimator

import (
	"bytes"

	"github.com/tidwall/gjson"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// reasoningMarker / thinkingMarker are the substrings every reasoning-intent key
// shares: the OpenAI keys carry "reasoning" (reasoning_effort / reasoning.effort)
// and the Anthropic + Gemini keys carry "thinking" (thinking.budget_tokens /
// thinking_config.thinking_budget). A body containing NEITHER cannot carry any
// reasoning signal, so the parse below is skipped — see ReadReasoningSignal.
var (
	reasoningMarker = []byte("reasoning")
	thinkingMarker  = []byte("thinking")
)

// ReasoningSignal is the normalized reasoning-effort signal extracted
// from the canonical request body. Estimator reads it once and uses it
// to size the output budget anchor.
type ReasoningSignal struct {
	Effort       string // "minimal" / "low" / "medium" / "high" / ""
	BudgetTokens int    // Anthropic thinking.budget_tokens / Gemini thinkingConfig.thinkingBudget
	Source       string // for assumptions
}

// ReadReasoningSignal extracts reasoning intent from a canonical request body.
//
// The level and the amount are not alternatives — they are one statement
// recorded in two places. Canonicalization writes the level, because that is
// the only spelling every wire reads; the caller's own figure stays in their
// provider's extension. So both are gathered: reading the level and stopping
// would zero the exact budget on every body that carries one, and reading only
// the budget would miss the level a caller stated directly.
//
// The effort uses the canonical vocabulary. When only an amount was given, the
// level is derived from it — through the same function canonicalization uses,
// so the estimate is anchored to the level the request will actually carry.
func ReadReasoningSignal(rawBody []byte) ReasoningSignal {
	// Fast prefilter: every reasoning-intent key carries "reasoning" or "thinking"
	// as a substring (see the marker vars). A body with neither cannot carry a
	// signal, so skip the gjson parse entirely — gjson.ParseBytes copies the whole
	// ~50 KB body to a string (the dominant per-request parse cost), and the common
	// chat request has no reasoning intent, so this avoids that copy on the hot path.
	// A false positive (the word appears in user content) merely falls through to the
	// authoritative key lookups below, which return empty when the keys are absent.
	if !bytes.Contains(rawBody, reasoningMarker) && !bytes.Contains(rawBody, thinkingMarker) {
		return ReasoningSignal{}
	}
	root := gjson.ParseBytes(rawBody)

	// The amount, from wherever this body records one. The two extension keys
	// are what canonicalization writes; the two bare keys are what a caller can
	// hand the estimate endpoint directly, which takes whatever they post.
	//
	// The Gemini extension holds the caller's own object, so the key inside it
	// keeps Gemini's camelCase spelling rather than this file's snake_case one.
	var sig ReasoningSignal
	for _, key := range []string{
		"thinking.budget_tokens",
		"nexus.ext.anthropic.thinking.budget_tokens",
		"thinking_config.thinking_budget",
		"nexus.ext.gemini.thinking_config.thinkingBudget",
	} {
		if v := root.Get(key); v.Exists() {
			sig.BudgetTokens = int(v.Int())
			sig.Source = key
			break
		}
	}

	// The level, which outranks the amount as the SOURCE of the effort: it is
	// what the request will actually carry to the wire, and for an amount no
	// level can express — Gemini's "you decide" — it is the only statement
	// there is.
	for _, key := range []string{"reasoning_effort", "reasoning.effort"} {
		if v := root.Get(key); v.Exists() {
			sig.Effort = v.String()
			sig.Source = key
			return sig
		}
	}
	if sig.BudgetTokens != 0 {
		sig.Effort = normcore.EffortForBudget(sig.BudgetTokens)
	}
	return sig
}
