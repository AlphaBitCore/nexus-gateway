// Package estimator computes a pre-flight cost estimate for an AI
// request without sending it upstream. Powers the nexus.dry_run flag
// and the /v1/estimate endpoint.
//
// Public surface:
//
//   - Estimate(ctx, EstimateInput) (EstimateResult, error)
//   - ReadReasoningSignal(rawBody []byte, ingressFormat) ReasoningSignal
//
// Tokenization: a character-ratio heuristic is used for every adapter
// family (OpenAI/Azure/DeepSeek/...: chars/3.5; Anthropic: chars/3.5;
// Gemini: chars/4). The heuristic is documented in
// EstimateResult.Assumptions[] so callers see the ±10–15% expected error.
//
// Output budget: a small static table keyed by (modelCode,
// reasoning_effort) provides the expected token range. Models not in
// the table fall back to a generic budget anchored at max_tokens / 4
// with a 3× envelope.
//
// Cache lookup: the estimator does not inspect the response cache or
// upstream prefix cache. CacheBenefit fields stay zero.
//
// All estimator output uses the canonical metrics.Cost struct so
// estimate and reality reconcile at the database level.
package estimator

import (
	"context"
	"fmt"
	"math"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// EstimateInput is the input to Estimate. The canonical request body is
// the OpenAI-shape JSON that the gateway's canonicalbridge produced;
// caller has already canonicalized the ingress shape.
type EstimateInput struct {
	CanonicalRequest []byte
	IngressFormat    provcore.Format
	Target           ResolvedTarget
	Prices           metrics.ModelPrices
}

// ResolvedTarget describes the (Provider, Model) the request would route
// to. The caller resolves this via the routing engine before calling
// Estimate; the estimator does not invoke routing itself.
type ResolvedTarget struct {
	ProviderID  string
	ModelID     string
	ModelCode   string
	AdapterType string
	MaxOutput   int // model.maxOutputTokens for clamping the high envelope
}

// EstimateResult is the structured output. Tokens come in low/expected/
// high triples to communicate the inherent uncertainty of output-token
// prediction. Costs use the same metrics.Cost struct that real-request
// stamping uses.
type EstimateResult struct {
	Tokens      TokenBreakdown     `json:"tokens"`
	Cost        CostBreakdown      `json:"cost"`
	Cache       CacheBenefit       `json:"cache"`
	Reasoning   ReasoningBreakdown `json:"reasoning"`
	Assumptions []string           `json:"assumptions,omitempty"`
}

// TokenBreakdown carries the input/output/reasoning token estimates.
// UncachedInput + InputCached = total estimated PromptTokens.
type TokenBreakdown struct {
	UncachedInput int   `json:"uncachedInput"`
	InputCached   int   `json:"inputCached"`
	Output        Range `json:"output"`
	Reasoning     Range `json:"reasoning"`
}

// CostBreakdown carries the low/expected/high Cost triple.
type CostBreakdown struct {
	Currency string       `json:"currency"`
	Low      metrics.Cost `json:"low"`
	Expected metrics.Cost `json:"expected"`
	High     metrics.Cost `json:"high"`
}

// CacheBenefit communicates expected cache savings (best-effort; v1
// leaves fields at zero — cache lookup is a future enhancement).
type CacheBenefit struct {
	ResponseHitProbability float64 `json:"responseHitProbability"`
	PromptCacheReadTokens  int     `json:"promptCacheReadTokens"`
	SavingsExpected        float64 `json:"savingsExpected"`
}

// ReasoningBreakdown reflects the reasoning_effort signal read from the
// request and the model's support for it.
type ReasoningBreakdown struct {
	EffortRequested  string `json:"effortRequested,omitempty"` // "minimal" / "low" / "medium" / "high" / ""
	SupportedByModel bool   `json:"supportedByModel"`
	EstimatedTokens  int    `json:"estimatedTokens"`
}

// Range is the low/expected/high triple used for token + cost estimates.
type Range struct {
	Low      int `json:"low"`
	Expected int `json:"expected"`
	High     int `json:"high"`
}

// Estimate is the single entry point. Deterministic given inputs; no
// global state, no logger calls in the hot path. Errors are limited to
// context cancellation and malformed canonical body.
func Estimate(ctx context.Context, in EstimateInput) (EstimateResult, error) {
	if err := ctx.Err(); err != nil {
		return EstimateResult{}, err
	}
	if len(in.CanonicalRequest) == 0 {
		return EstimateResult{}, fmt.Errorf("estimator: empty canonical request body")
	}

	out := EstimateResult{Cost: CostBreakdown{Currency: "USD"}}

	// Step 1 — tokenize input.
	tk := pickTokenizer(in.Target.AdapterType)
	inputChars := countCanonicalInputChars(in.CanonicalRequest)
	estimatedInput := tk.CountTokens(inputChars)
	out.Tokens.UncachedInput = estimatedInput
	if tk.IsHeuristic() {
		out.Assumptions = append(out.Assumptions,
			fmt.Sprintf("%s token count is a character-ratio heuristic (chars/%.1f); ±10–15%% typical error",
				in.Target.AdapterType, tk.Divisor()))
	}

	// Step 2 — read reasoning effort signal.
	reasoning := ReadReasoningSignal(in.CanonicalRequest, in.IngressFormat)

	// Step 3 — output budget anchor.
	expectedOutput, supports := lookupOutputBudget(in.Target.ModelCode, reasoning.Effort)
	if expectedOutput == 0 {
		// Generic fallback: anchor at max_tokens / 4, with a 3× envelope.
		expectedOutput = pickMaxTokens(in.CanonicalRequest, in.Target.MaxOutput) / 4
		if expectedOutput < 100 {
			expectedOutput = 100
		}
	}
	out.Tokens.Output = expandRange(expectedOutput, in.Target.MaxOutput)
	out.Reasoning = ReasoningBreakdown{
		EffortRequested:  reasoning.Effort,
		SupportedByModel: supports,
	}
	if supports {
		// For reasoning-capable models, ~60% of the output anchor is the
		// reasoning subset for high effort, ~30% for medium, ~10% for
		// low/minimal. Heuristic only.
		share := reasoningShareFromEffort(reasoning.Effort)
		out.Reasoning.EstimatedTokens = int(float64(expectedOutput) * share)
		out.Tokens.Reasoning = expandRange(out.Reasoning.EstimatedTokens, in.Target.MaxOutput)
	} else if reasoning.Effort != "" {
		out.Assumptions = append(out.Assumptions,
			fmt.Sprintf("model %s does not support reasoning; reasoning_effort=%s ignored",
				in.Target.ModelCode, reasoning.Effort))
	}

	// Step 4 — compute cost triple via the shared CalculateCost function.
	// Reuses the exact same arithmetic that real-request stamping uses, so
	// estimate and reality reconcile at the cost.Total level.
	out.Cost.Low = costForTokens(out.Tokens.UncachedInput, out.Tokens.Output.Low, in.Prices)
	out.Cost.Expected = costForTokens(out.Tokens.UncachedInput, out.Tokens.Output.Expected, in.Prices)
	out.Cost.High = costForTokens(out.Tokens.UncachedInput, out.Tokens.Output.High, in.Prices)

	return out, nil
}

// costForTokens helper synthesises a provcore.Usage matching the
// estimate's token counts and calls metrics.CalculateCost. PromptTokens
// uses the OpenAI canonical convention (full input including any cached
// subset); CacheReadTokens / CacheCreationTokens are zero.
func costForTokens(input, output int, prices metrics.ModelPrices) metrics.Cost {
	prompt := input
	completion := output
	usage := provcore.Usage{
		PromptTokens:     &prompt,
		CompletionTokens: &completion,
	}
	return metrics.CalculateCost(usage, prices)
}

// expandRange returns a low/expected/high range based on the anchor.
// low = anchor/3, high = min(anchor*3, maxOutput) so the high envelope
// never exceeds the model's max_output_tokens.
func expandRange(anchor int, maxOutput int) Range {
	if anchor < 1 {
		anchor = 1
	}
	low := anchor / 3
	if low < 1 {
		low = 1
	}
	high := anchor * 3
	if maxOutput > 0 && high > maxOutput {
		high = maxOutput
	}
	if high < anchor {
		high = anchor
	}
	return Range{Low: low, Expected: anchor, High: high}
}

// reasoningShareFromEffort returns the fraction of total output tokens
// the reasoning subset typically occupies, per documented vendor
// guidance (OpenAI Reasoning Guide + Anthropic + Gemini 2.5 thinking).
func reasoningShareFromEffort(effort string) float64 {
	switch effort {
	case "high":
		return 0.60
	case "medium":
		return 0.30
	case "low", "minimal":
		return 0.10
	}
	return 0.0
}

// countCanonicalInputChars sums the character length of every input
// text part across the on-wire shapes the dry-run dispatcher may hand
// us (the caller passes whatever PrepareBody produced for the target
// adapter, which is wire-shape, NOT always canonical OpenAI):
//
//   - OpenAI / Anthropic chat: messages[].content (string OR
//     [{type:"text", text:"…"}] blocks). Anthropic top-level "system"
//     can be either a string or the same block array.
//   - OpenAI Responses-API: top-level `input` (string OR array of
//     structured items — message / input_text / function_call_output)
//     plus optional `instructions` (string).
//   - Gemini generateContent: contents[].parts[].text plus
//     systemInstruction.parts[].text.
//
// Reasonably accurate for text; multimodal parts are ignored (token
// cost dominated by text content). Returning 0 chars would zero out
// the whole estimate, so each branch is independent (we sum whichever
// keys exist in the body).
func countCanonicalInputChars(canonical []byte) int {
	total := 0

	// OpenAI / Anthropic chat shape.
	gjson.GetBytes(canonical, "messages").ForEach(func(_, msg gjson.Result) bool {
		total += textCharsFromContent(msg.Get("content"))
		return true
	})
	// Anthropic top-level system (string or block array).
	total += textCharsFromContent(gjson.GetBytes(canonical, "system"))

	// OpenAI Responses-API native shape: `input` can be a plain string,
	// an array of strings, or an array of structured items. The structured
	// items may be `{role,content:[{type:"input_text",text:"…"}]}` shaped
	// messages, plain `{type:"input_text",text:"…"}` parts, or tool
	// outputs / function_call_output entries with `output` text.
	if input := gjson.GetBytes(canonical, "input"); input.Exists() {
		if input.Type == gjson.String {
			total += len(input.String())
		} else {
			input.ForEach(func(_, item gjson.Result) bool {
				if item.Type == gjson.String {
					total += len(item.String())
					return true
				}
				total += textCharsFromContent(item.Get("content"))
				if t := item.Get("text"); t.Exists() {
					total += len(t.String())
				}
				if o := item.Get("output"); o.Exists() && o.Type == gjson.String {
					total += len(o.String())
				}
				return true
			})
		}
	}
	// Responses-API top-level instructions (system-message analogue).
	total += textCharsFromContent(gjson.GetBytes(canonical, "instructions"))

	// Gemini generateContent shape.
	gjson.GetBytes(canonical, "contents").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("parts").ForEach(func(_, part gjson.Result) bool {
			if t := part.Get("text"); t.Exists() {
				total += len(t.String())
			}
			return true
		})
		return true
	})
	gjson.GetBytes(canonical, "systemInstruction.parts").ForEach(func(_, part gjson.Result) bool {
		if t := part.Get("text"); t.Exists() {
			total += len(t.String())
		}
		return true
	})

	return total
}

// textCharsFromContent extracts character length from either a string
// content or an array of content blocks (OpenAI multimodal / Anthropic).
func textCharsFromContent(content gjson.Result) int {
	if !content.Exists() {
		return 0
	}
	if content.Type == gjson.String {
		return len(content.String())
	}
	total := 0
	content.ForEach(func(_, part gjson.Result) bool {
		if t := part.Get("text"); t.Exists() {
			total += len(t.String())
		}
		return true
	})
	return total
}

// pickMaxTokens reads max_tokens / max_completion_tokens from the
// canonical body. Falls back to the model's documented max_output when
// neither is set.
func pickMaxTokens(canonical []byte, modelMax int) int {
	if v := gjson.GetBytes(canonical, "max_completion_tokens"); v.Exists() {
		return int(v.Int())
	}
	if v := gjson.GetBytes(canonical, "max_tokens"); v.Exists() {
		return int(v.Int())
	}
	if modelMax > 0 {
		return modelMax
	}
	return 4096
}

// _ keeps math imported when only used by other estimator files.
var _ = math.Sqrt
