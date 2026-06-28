package estimator

import (
	"bytes"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
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

// ReadReasoningSignal extracts reasoning intent from a canonical
// request body. The canonical body is OpenAI shape, but some clients
// also include the Anthropic-shape thinking object or Gemini-shape
// thinkingConfig — we honor whichever is present.
//
// Returned effort uses the OpenAI canonical vocabulary (minimal/low/
// medium/high). When a numeric budget is given (Anthropic /
// Gemini), it's bucketed: <2000=low, 2000-7999=medium, ≥8000=high.
func ReadReasoningSignal(rawBody []byte, ingressFormat provcore.Format) ReasoningSignal {
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

	// 1. OpenAI canonical reasoning_effort string.
	if v := root.Get("reasoning_effort"); v.Exists() {
		return ReasoningSignal{Effort: v.String(), Source: "reasoning_effort"}
	}
	// OpenAI Responses-API places it under reasoning.effort.
	if v := root.Get("reasoning.effort"); v.Exists() {
		return ReasoningSignal{Effort: v.String(), Source: "reasoning.effort"}
	}

	// 2. Anthropic thinking.budget_tokens (lifted into canonical body as
	//    a nexus.ext.* round-trip when the canonicalbridge processed the
	//    ingress).
	if v := root.Get("thinking.budget_tokens"); v.Exists() {
		b := int(v.Int())
		return ReasoningSignal{Effort: bucketBudget(b), BudgetTokens: b, Source: "thinking.budget_tokens"}
	}
	if v := root.Get("nexus.ext.anthropic.thinking.budget_tokens"); v.Exists() {
		b := int(v.Int())
		return ReasoningSignal{Effort: bucketBudget(b), BudgetTokens: b, Source: "nexus.ext.anthropic.thinking.budget_tokens"}
	}

	// 3. Gemini thinking_config.thinking_budget.
	if v := root.Get("thinking_config.thinking_budget"); v.Exists() {
		b := int(v.Int())
		return ReasoningSignal{Effort: bucketBudget(b), BudgetTokens: b, Source: "thinking_config.thinking_budget"}
	}
	if v := root.Get("nexus.ext.gemini.thinking_config.thinking_budget"); v.Exists() {
		b := int(v.Int())
		return ReasoningSignal{Effort: bucketBudget(b), BudgetTokens: b, Source: "nexus.ext.gemini.thinking_config.thinking_budget"}
	}

	return ReasoningSignal{}
}

// bucketBudget maps a numeric budget to the OpenAI effort vocabulary.
func bucketBudget(b int) string {
	switch {
	case b <= 0:
		return ""
	case b < 2000:
		return "low"
	case b < 8000:
		return "medium"
	default:
		return "high"
	}
}
