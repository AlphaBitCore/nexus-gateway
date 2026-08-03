// Per-model wire-quirk rules owned by deepseek.
//
// Per provider-adapter-architecture.md §3a Rule 3, DeepSeek's per-model
// wire quirks live with the DeepSeek adapter, not in the generic dispatch
// layer. Both rules are STRUCTURAL (a value-conditional removal and a
// message-array back-fill — shapes a body-root FieldRule cannot express),
// so they ride the identity codec's ChatStructural list and execute on
// the decode door for thinking models — from BOTH codec entry points.
// The transitional dispatch callback covered only the native chat leg:
// the same history bridged from /v1/messages (or any non-OpenAI ingress)
// reached DeepSeek unfixed and 400'd.
package deepseek

import (
	"fmt"
	"strings"

	openaicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
)

// IsThinkingModel reports whether the DeepSeek model id belongs to a
// thinking/reasoning family that imposes the two thinking-mode wire quirks:
// it rejects a FORCED tool_choice with HTTP 400 `{"error":{"type":
// "invalid_request_error","message":"Thinking mode does not support this
// tool_choice"}}`, and it rejects a history whose assistant tool_calls turns
// dropped reasoning_content with HTTP 400 "The `reasoning_content` in the
// thinking mode must be passed back to the API."
//
// Matched: the whole deepseek-v4 family (deepseek-v4-pro AND deepseek-v4-flash
// both observed on api.taskforce10x.com) and deepseek-reasoner. The match is
// the family prefix, not per-suffix, because the quirk is a property of the v4
// thinking architecture, not of the pro/flash tier — a denylist of exact
// suffixes went stale exactly once already: deepseek-v4-flash 400'd every
// tool-loop client with a dropped reasoning_content because only -pro and
// -reasoner were listed (§3a Rule 7: both v4 tiers are evidenced; the family
// match generalizes across the shared architecture rather than re-chasing each
// new suffix). Both quirk fixes are behavior-preserving where they may not be
// needed — a stripped forced tool_choice ≡ auto, an empty reasoning_content is
// accepted as a plain presence marker — so widening the gate cannot regress a
// v4 model that happened to accept the original body.
func IsThinkingModel(modelID string) bool {
	return strings.HasPrefix(modelID, "deepseek-reasoner") ||
		strings.HasPrefix(modelID, "deepseek-v4")
}

// Contract assembles the DeepSeek wire contract: one structural rule
// carrying both thinking-model quirks (they share the gate and the
// decode-door execution).
func Contract() openaicodec.Contract {
	return openaicodec.Contract{
		ChatStructural: []openaicodec.StructuralRule{{
			Applies: IsThinkingModel,
			Apply:   applyThinkingRules,
		}},
	}
}

// applyThinkingRules applies the thinking-model quirks DeepSeek's wire
// imposes. Returns the rewrites applied; idempotent (a second pass finds
// nothing left to remove or fill).
func applyThinkingRules(payload map[string]any, _ string) []string {
	var rewrites []string
	if r := stripForcedToolChoice(payload); r != "" {
		rewrites = append(rewrites, r)
	}
	if n := fillMissingReasoningContent(payload); n > 0 {
		rewrites = append(rewrites, fmt.Sprintf("reasoning_content→filled_on_%d_assistant_tool_calls", n))
	}
	return rewrites
}

// stripForcedToolChoice removes a FORCED tool_choice ("required", or a named
// {"type":"function",...} selection) — the upstream hard-rejects it while
// still CALLING tools fine under the default behavior, so removal (≡ auto)
// preserves the caller's intent where forcing is impossible. "auto"/"none"
// pass through untouched (both accepted upstream).
func stripForcedToolChoice(payload map[string]any) string {
	tc, ok := payload["tool_choice"]
	if !ok {
		return ""
	}
	forced := false
	switch v := tc.(type) {
	case string:
		forced = v == "required"
	case map[string]any:
		forced = true // a named function selection is a forced choice
	}
	if !forced {
		return ""
	}
	delete(payload, "tool_choice")
	return "tool_choice→removed"
}

// fillMissingReasoningContent adds an empty reasoning_content to every
// assistant message that carries tool_calls but lacks the key. In thinking
// mode DeepSeek rejects such a history with HTTP 400 {"error":{"message":
// "The `reasoning_content` in the thinking mode must be passed back to the
// API."}} — observed in prod on deepseek-v4-pro from an agent loop that
// dropped the field when rebuilding its history.
//
// Probed against api.deepseek.com to bound the rule exactly: a plain
// assistant turn without reasoning_content is accepted (so this is not a
// blanket requirement — only tool_calls turns are checked), and an EMPTY
// string satisfies the check. The upstream tests for the key's presence, not
// its content. Filling "" is therefore lossless: the caller never sent the
// real reasoning, so it is gone either way, and the choice is between a
// rejected request and one the model can answer without prior reasoning.
// (When the ingress DID carry real reasoning — an Anthropic thinking
// history — the canonical converter now maps it to reasoning_content
// before this back-fill runs, so the "" fill no longer masks real text.)
// Returns the number of messages filled.
func fillMissingReasoningContent(payload map[string]any) int {
	msgs, ok := payload["messages"].([]any)
	if !ok {
		return 0
	}
	filled := 0
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		if _, has := msg["reasoning_content"]; has {
			continue
		}
		if calls, ok := msg["tool_calls"].([]any); !ok || len(calls) == 0 {
			continue
		}
		msg["reasoning_content"] = ""
		filled++
	}
	return filled
}
