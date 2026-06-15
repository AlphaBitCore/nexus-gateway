// Per-model passthrough rewrites owned by deepseek.
//
// Per provider-adapter-architecture.md §3a Rule 3, DeepSeek's per-model
// wire quirks live with the DeepSeek adapter, not in the generic
// providers/spec_adapter.go layer. The passthrough dispatch reaches us
// via the AdapterSpec.PassthroughRewrite callback wired in NewSpec.
package deepseek

import "strings"

// IsThinkingModel reports whether the DeepSeek model id belongs to a
// thinking/reasoning family that rejects a FORCED tool_choice with
// HTTP 400 `{"error":{"type":"invalid_request_error","message":
// "Thinking mode does not support this tool_choice"}}`.
//
// Observed (2026-06, api.deepseek.com): deepseek-v4-pro (400 above, on
// tool_choice:"required"); deepseek-reasoner (same rejection class —
// four consecutive structured-output calls lost to it on 2026-06-11).
// Only these two evidenced families are matched — removal flattens the
// caller's forcing, so a speculative rule would silently change behavior
// on models that might accept it (§3a Rule 7: every rule cites its 400).
func IsThinkingModel(modelID string) bool {
	return strings.HasPrefix(modelID, "deepseek-reasoner") ||
		strings.HasPrefix(modelID, "deepseek-v4-pro")
}

// ApplyRewrites strips a FORCED tool_choice ("required", or a named
// {"type":"function",...} selection) on thinking DeepSeek models — the
// upstream hard-rejects it while still CALLING tools fine under the
// default behavior, so removal (≡ auto) preserves the caller's intent
// where forcing is impossible. "auto"/"none" pass through untouched
// (both accepted upstream). Returns the rewrites applied, or nil when
// modelID is not a thinking model.
//
// Wired into AdapterSpec.PassthroughRewrite in NewSpec; consumed by
// the generic spec_adapter.PrepareBody passthrough path.
func ApplyRewrites(payload map[string]any, modelID string) []string {
	if !IsThinkingModel(modelID) {
		return nil
	}
	tc, ok := payload["tool_choice"]
	if !ok {
		return nil
	}
	forced := false
	switch v := tc.(type) {
	case string:
		forced = v == "required"
	case map[string]any:
		forced = true // a named function selection is a forced choice
	}
	if !forced {
		return nil
	}
	delete(payload, "tool_choice")
	return []string{"tool_choice→removed"}
}
