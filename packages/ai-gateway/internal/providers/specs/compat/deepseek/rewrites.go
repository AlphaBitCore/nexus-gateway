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
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// IsThinkingModel reports whether the DeepSeek model id belongs to a
// thinking/reasoning family that imposes the two thinking-mode wire quirks:
// it rejects a FORCED tool_choice with HTTP 400 `{"error":{"type":
// "invalid_request_error","message":"Thinking mode does not support this
// tool_choice"}}`, and it rejects a history whose assistant turns dropped
// reasoning_content with HTTP 400 "The `reasoning_content` in the thinking
// mode must be passed back to the API."
//
// The trigger for that second rejection is the REQUEST carrying `tools`, not
// the individual turn carrying `tool_calls`. Probed on 2026-08-19, one
// variable at a time: plain assistant turns with no tools → 200; the same
// turns with a tools array → 400; with the empty presence marker restored →
// 200 again. A tool-calls-only fill therefore covered one shape of the
// requirement and left staging failing on a 17-tool request whose assistant
// turns were plain text and had never called anything.
//
// Matched: deepseek-reasoner, and generation 4 or later of the deepseek-v
// line (deepseek-v4-pro AND deepseek-v4-flash both observed on
// api.taskforce10x.com). Neither half of that is an exact list, and both
// widenings have the same cause — an exact list goes stale in the direction
// that hurts. Exact SUFFIXES went stale once: deepseek-v4-flash 400'd every
// tool-loop client with a dropped reasoning_content because only -pro and
// -reasoner were named. An exact GENERATION would go stale the same way the
// day a v5 ships, and an unreleased generation is precisely the one nobody
// has probed.
//
// Widening is safe because both fixes are behaviour-preserving on a model
// that would have accepted the original body: a stripped forced tool_choice
// ≡ auto, and an empty reasoning_content is accepted as a plain presence
// marker. §3a Rule 7 is satisfied — both v4 tiers are evidenced, and the
// generalization follows the shared thinking architecture rather than
// re-chasing each new suffix.
func IsThinkingModel(modelID string) bool {
	return strings.HasPrefix(modelID, "deepseek-reasoner") ||
		specutil.GenerationAtLeast(modelID, "deepseek-v", 4)
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
		rewrites = append(rewrites, fmt.Sprintf("reasoning_content→filled_on_%d_assistant_turns", n))
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
	// A non-empty tools array puts the whole history under the requirement; an
	// empty one is the no-tools case and must not widen it.
	tools, _ := payload["tools"].([]any)
	toolsPresent := len(tools) > 0
	filled := 0
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		if _, has := msg["reasoning_content"]; has {
			continue
		}
		if !toolsPresent && !hasToolCalls(msg) {
			// No tools on the request and none on this turn: the upstream
			// accepts the turn as-is, so filling it would rewrite a body it
			// never objected to.
			continue
		}
		msg["reasoning_content"] = ""
		filled++
	}
	return filled
}

// hasToolCalls reports whether an assistant message carries a non-empty
// tool_calls array. Kept separate from the tools-present test above because
// the two are different facts: one is about this turn, the other about the
// request the turn is replayed in.
func hasToolCalls(msg map[string]any) bool {
	calls, ok := msg["tool_calls"].([]any)
	return ok && len(calls) > 0
}
