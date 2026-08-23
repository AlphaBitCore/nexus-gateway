package codec

import (
	"fmt"
	"strings"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Per-model thinking-contract policy for the Anthropic wire. The vendor
// replaced the thinking request shape generation by generation: older Claude
// families take `thinking: {type: "enabled", budget_tokens: N}`, while every
// family from Opus 4.7 onward takes `thinking: {type: "adaptive"}` with the
// level in a separate `output_config: {effort: ...}` — and rejects the old
// shape with 400 `"thinking.type.enabled" is not supported for this model.
// Use "thinking.type.adaptive" and "output_config.effort"`.
//
// All boundaries below are from direct probes against api.anthropic.com:
//   - claude-opus-5 / claude-fable-5 with adaptive + output_config.effort=low
//     → 200 with a thinking block and usage.output_tokens_details.thinking_tokens.
//   - adaptive + budget_tokens → 400 "thinking.adaptive.budget_tokens: Extra
//     inputs are not permitted" — the two vocabularies are exclusive.
//   - output_config.effort accepts exactly 'low', 'medium', 'high', 'xhigh',
//     'max' (the 400 for an unknown word enumerates them).
//   - claude-opus-4-6 with the adaptive shape → 200 but thinking_tokens=0 and
//     no thinking block: an older model SILENTLY IGNORES the new shape, so
//     keeping older families on `enabled` is what preserves their reasoning.

// setOutputConfig writes ONE key into the outgoing `output_config`, creating the
// map only when it is absent.
//
// Anthropic puts two unrelated things there — `format` (the caller's JSON
// Schema) and `effort` (reasoning depth) — and they are written by two different
// walks. Both used to assign the whole map, so whichever ran second erased the
// other. The reasoning walk runs second, which meant a request carrying both a
// schema and `reasoning_effort` reached Anthropic with no schema at all: HTTP
// 200, no JSON object, and an `x-nexus-coerced` header naming the effort while
// saying nothing about what it destroyed. Confirmed live on prod-20260819d
// against claude-opus-5 before this existed.
//
// `applyNativeThinkingContract` had already got this right with
// sjson.SetBytes(out, "output_config.effort", …), which merges. This is that
// same answer for the map-shaped path, so the file now has one answer instead
// of two.
func setOutputConfig(out map[string]any, key string, value any) {
	cfg, _ := out["output_config"].(map[string]any)
	if cfg == nil {
		cfg = map[string]any{}
		out["output_config"] = cfg
	}
	cfg[key] = value
}

// claudeModelsOnEnabledThinking lists the Claude family prefixes that speak
// the `enabled` + budget_tokens contract. Same allowlist direction as
// claudeModelsAcceptingSamplingParams, for the same reason: an unlisted
// model — which every future Claude release starts out as — gets the shape
// the vendor is moving toward, instead of a 400 in its first hour of traffic.
var claudeModelsOnEnabledThinking = []string{
	"claude-opus-4-6",
	"claude-opus-4-5",
	"claude-opus-4-1",
	"claude-sonnet-4-6",
	"claude-sonnet-4-5",
	"claude-haiku-4-5",
	// 3.x introduced extended thinking on the enabled contract; 2.x and
	// instant predate thinking entirely, and for them the enabled shape and
	// the adaptive shape fail the same way — keeping them here changes
	// nothing while keeping the allowlist aligned with the sampling one.
	"claude-3",
	"claude-2",
	"claude-instant",
}

// anthropicModelOnAdaptiveThinking reports whether reasoning intent must be
// spelled as adaptive + output_config.effort for this model.
//
// Only Claude models are judged: a non-Claude id behind the anthropic
// adapter is some other endpoint speaking the Messages shape whose thinking
// contract we have never probed.
func anthropicModelOnAdaptiveThinking(model string) bool {
	if !strings.HasPrefix(model, "claude-") {
		return false
	}
	for _, prefix := range claudeModelsOnEnabledThinking {
		if specutil.MatchesFamily(model, prefix) {
			return false
		}
	}
	return true
}

// adaptiveEffortFromCanonical maps the canonical reasoning_effort vocabulary
// onto the probed output_config.effort one. "none" and unknown words return
// ok=false: none means the caller asked NOT to think (spelled by omission on
// this wire), and guessing what an unknown word means is how a translation
// starts producing answers nobody asked for.
func adaptiveEffortFromCanonical(effort string) (string, bool) {
	switch strings.ToLower(effort) {
	case "minimal":
		return "low", true
	case "low", "medium", "high", "max":
		return strings.ToLower(effort), true
	default:
		return "", false
	}
}

// applyNativeThinkingContract coerces a native enabled+budget thinking block
// into the adaptive shape on models whose wire rejects the old one. The
// budget becomes output_config.effort via the shared budget→level table, so
// the caller's "how much" survives the vocabulary change; the budget field
// itself cannot ride along (probed: adaptive + budget_tokens is a 400,
// "Extra inputs are not permitted").
//
// Disabled blocks and bodies with no thinking are returned as the same
// slice. Models on the enabled contract are never touched — for them the
// caller's block is already the wire's own vocabulary.
func applyNativeThinkingContract(body []byte, model string) ([]byte, []string, error) {
	if !anthropicModelOnAdaptiveThinking(model) {
		return body, nil, nil
	}
	thinking := gjson.GetBytes(body, "thinking")
	if !thinking.Exists() || !thinking.IsObject() || thinking.Get("type").String() != "enabled" {
		return body, nil, nil
	}
	effort := normcore.EffortForBudget(int(thinking.Get("budget_tokens").Int()))
	if effort == "" {
		// enabled with no usable budget is malformed on the old contract
		// too; forwarding it verbatim keeps the upstream's answer the
		// caller's to see.
		return body, nil, nil
	}
	out, err := sjson.SetBytes(body, "thinking", map[string]any{"type": "adaptive"})
	if err != nil {
		return nil, nil, err
	}
	// Only fill the effort if the caller has not already set one — a body
	// carrying both vocabularies keeps its own output_config.
	if !gjson.GetBytes(out, "output_config.effort").Exists() {
		if out, err = sjson.SetBytes(out, "output_config.effort", effort); err != nil {
			return nil, nil, err
		}
	}
	return out, []string{"thinking.enabled→adaptive+output_config.effort=" + effort}, nil
}

// applyReasoningIntent writes the model's thinking configuration into the
// outgoing body from whichever spelling the caller used, in precedence order:
//
//  1. nexus.ext.anthropic.thinking — the caller spoke this wire's native
//     vocabulary and their block is forwarded, with one exception: an
//     enabled+budget block bound for an adaptive-contract model is coerced
//     exactly as the native door coerces it, because forwarding it is the
//     recorded 400, not a preference we can honour.
//  2. reasoning_effort, canonical — translated to the model's contract:
//     adaptive+output_config.effort for the newest generations, an
//     enabled+budget block derived from max_tokens for the older ones.
func applyReasoningIntent(canonicalBody []byte, out map[string]any, model string, rewrites *[]string) error {
	if ext := canonicalext.Get(canonicalBody, "anthropic", "thinking"); ext.Exists() {
		if !ext.IsObject() {
			canonicalext.WarnOnce("anthropic", "thinking_not_object")
			provdispatch.EmitReasoningPassthrough("anthropic", "skipped_malformed")
			return nil
		}
		var thinking map[string]any
		// The IsObject guard above guarantees a JSON object, so this Unmarshal
		// into a map cannot fail — an empty object is the only skip path. An
		// empty (or, defensively, unparseable) block is SKIPPED fail-open:
		// forwarding a block we cannot use is the recorded 400, not a preference
		// to honour. The discarded error is intentional; IsObject already
		// rejected every non-object shape a caller could send.
		_ = json.Unmarshal([]byte(ext.Raw), &thinking)
		if len(thinking) == 0 {
			canonicalext.WarnOnce("anthropic", "thinking_empty")
			provdispatch.EmitReasoningPassthrough("anthropic", "skipped_malformed")
			return nil
		}
		if err := reconcileThinkingBudget(out, thinking, rewrites); err != nil {
			return err
		}
		if t, _ := thinking["type"].(string); t == "enabled" && anthropicModelOnAdaptiveThinking(model) {
			budget, _ := numericField(thinking["budget_tokens"])
			if eff := normcore.EffortForBudget(int(budget)); eff != "" {
				thinking = map[string]any{"type": "adaptive"}
				setOutputConfig(out, "effort", eff)
				*rewrites = append(*rewrites, "thinking.enabled→adaptive+output_config.effort="+eff)
			}
		}
		out["thinking"] = thinking
		provdispatch.EmitReasoningPassthrough("anthropic", "injected")
		return nil
	}
	if anthropicModelOnAdaptiveThinking(model) {
		effort := gjson.GetBytes(canonicalBody, "reasoning_effort")
		if effort.Exists() && effort.Type == gjson.String {
			if eff, ok := adaptiveEffortFromCanonical(effort.String()); ok {
				out["thinking"] = map[string]any{"type": "adaptive"}
				setOutputConfig(out, "effort", eff)
				*rewrites = append(*rewrites, "reasoning_effort→thinking.adaptive+output_config.effort="+eff)
				provdispatch.EmitReasoningPassthrough("anthropic", "translated")
			}
		}
		return nil
	}
	if thinking := thinkingFromCanonicalEffort(canonicalBody, out); thinking != nil {
		// The caller asked to reason in the canonical spelling — a LEVEL —
		// and this generation's wire takes a BUDGET. Without the translation
		// an OpenAI-ingress caller routed here has their intent silently
		// dropped on a row that looks identical to one served properly.
		if err := reconcileThinkingBudget(out, thinking, rewrites); err != nil {
			return err
		}
		out["thinking"] = thinking
		*rewrites = append(*rewrites, fmt.Sprintf("reasoning_effort→thinking.budget_tokens=%v", thinking["budget_tokens"]))
		provdispatch.EmitReasoningPassthrough("anthropic", "translated")
	}
	return nil
}
