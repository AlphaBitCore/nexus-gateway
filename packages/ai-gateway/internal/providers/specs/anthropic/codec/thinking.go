package codec

import "github.com/tidwall/gjson"

// reconstructThinkingBlocks rebuilds the Anthropic `thinking` /
// `redacted_thinking` content blocks that must lead an assistant turn's
// content array. Two sources, preferred in order:
//   - nexus_thinking: the per-block array the Anthropic ingress sets from
//     the client's own history — each block keeps its OWN signature
//     (Anthropic validates a signature against the exact content it
//     signed, so blocks must not be merged) and redacted blocks survive.
//   - reasoning_content: the L2 universal fallback a cross-format upstream
//     (DeepSeek/OpenAI) produced — one unsigned thinking block.
//
// Returns nil when the message carried no reasoning.
func reconstructThinkingBlocks(msg gjson.Result) []map[string]any {
	if nt := msg.Get("nexus_thinking"); nt.IsArray() {
		var blocks []map[string]any
		nt.ForEach(func(_, b gjson.Result) bool {
			if rd := b.Get("redacted_data").String(); rd != "" {
				blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": rd})
				return true
			}
			block := map[string]any{"type": "thinking", "thinking": b.Get("thinking").String()}
			if sig := b.Get("signature").String(); sig != "" {
				block["signature"] = sig
			}
			blocks = append(blocks, block)
			return true
		})
		if len(blocks) > 0 {
			return blocks
		}
	}
	if r := msg.Get("reasoning_content").String(); r != "" {
		return []map[string]any{{"type": "thinking", "thinking": r}}
	}
	return nil
}
