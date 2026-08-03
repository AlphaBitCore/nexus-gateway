package ingress

import "strings"

// addReasoning attaches collected assistant thinking history to a canonical
// message. Two carriers, by design:
//   - reasoning_content (joined text) is the L2 universal field — it
//     legitimately flows to any reasoning target (DeepSeek consumes it),
//     and is harmless elsewhere.
//   - nexus_thinking (per-block {thinking, signature} / {redacted_data})
//     is Anthropic-round-trip-only: the Anthropic codec rebuilds every
//     block with its OWN signature (Anthropic validates a signature
//     against the exact content it signed), and the bridge STRIPS this
//     nexus-namespaced field before any non-Anthropic upstream sees it.
//
// No-op when the message carried no thinking blocks.
func addReasoning(entry map[string]any, lines []string, blocks []map[string]any) {
	if len(blocks) == 0 {
		return
	}
	if len(lines) > 0 {
		entry["reasoning_content"] = strings.Join(lines, "\n")
	}
	entry["nexus_thinking"] = blocks
}
