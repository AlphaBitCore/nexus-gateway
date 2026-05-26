// Per-model passthrough rewrites owned by openai.
//
// Per provider-adapter-architecture.md §3a Rule 3, OpenAI's own
// per-model wire quirks (gpt-5.x and o-series reasoning models reject
// classic chat-completions parameter names) live with the OpenAI
// adapter, not in the generic providers/spec_adapter.go layer. The
// passthrough dispatch reaches us via the AdapterSpec.PassthroughRewrite
// callback wired in NewSpec.
package rewrites

import "strings"

// IsReasoningModel reports whether modelID is an OpenAI reasoning
// model that rejects the classic chat-completions parameter set
// (`max_tokens`, `temperature`, `top_p`). Detection is by ID prefix
// because the upstream gates parameters on the model identifier, not
// on a discoverable capability flag.
//
// Covered families: o-series (o1, o3, o4, ...) and gpt-5*. The check
// matches "o" followed by a digit so future "o5"/"o6" snapshots
// inherit the rule without code changes; any non-digit (e.g. legacy
// "openai-..." names) is rejected so we don't accidentally rewrite
// non-reasoning calls.
//
// Observed (2026-05, direct calls to api.openai.com):
//   - gpt-5.4 / gpt-5.4-mini / gpt-5.4-nano / gpt-5.5 — 400
//     "Unsupported parameter: 'max_tokens' is not supported with this
//     model. Use 'max_completion_tokens' instead."
//   - o1 / o3 / o3-mini / o4-mini — same 400.
func IsReasoningModel(modelID string) bool {
	if modelID == "" {
		return false
	}
	if strings.HasPrefix(modelID, "gpt-5") {
		return true
	}
	if len(modelID) >= 2 && modelID[0] == 'o' && modelID[1] >= '1' && modelID[1] <= '9' {
		return true
	}
	return false
}

// ApplyReasoningRewrites rewrites a chat-completions payload in place
// so it matches the parameter contract of OpenAI reasoning models:
//
//   - max_tokens → max_completion_tokens (vendor rename; hard 400 without it)
//   - temperature removed (reasoning models reject it with HTTP 400)
//   - top_p removed (same rejection behaviour)
//
// Returns the list of rewrites applied, formatted as "<from>→<to>" or
// "<param>→removed". Returns nil when modelID is not a reasoning
// family or when no rewrite was needed.
//
// Wired into AdapterSpec.PassthroughRewrite in NewSpec; consumed by
// the generic spec_adapter.PrepareBody passthrough path.
func ApplyReasoningRewrites(payload map[string]any, modelID string) []string {
	if !IsReasoningModel(modelID) {
		return nil
	}
	var rewrites []string
	if v, ok := payload["max_tokens"]; ok {
		if _, has := payload["max_completion_tokens"]; !has {
			payload["max_completion_tokens"] = v
		}
		delete(payload, "max_tokens")
		rewrites = append(rewrites, "max_tokens→max_completion_tokens")
	}
	if _, ok := payload["temperature"]; ok {
		delete(payload, "temperature")
		rewrites = append(rewrites, "temperature→removed")
	}
	if _, ok := payload["top_p"]; ok {
		delete(payload, "top_p")
		rewrites = append(rewrites, "top_p→removed")
	}
	return rewrites
}
