package codec

import "github.com/tidwall/gjson"

// reconstructThinkingBlocks rebuilds only signed Anthropic thinking and
// redacted_thinking blocks from the nexus_thinking exact-replay carrier.
// reasoning_content is universal text, not provider-native signed thinking,
// and must never be fabricated into an Anthropic block.
func reconstructThinkingBlocks(msg gjson.Result) []map[string]any {
	if nt := msg.Get("nexus_thinking"); nt.IsArray() {
		var blocks []map[string]any
		nt.ForEach(func(_, b gjson.Result) bool {
			if rd := b.Get("redacted_data").String(); rd != "" {
				blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": rd})
				return true
			}
			sig := b.Get("signature").String()
			if sig == "" {
				return true
			}
			block := map[string]any{"type": "thinking", "thinking": b.Get("thinking").String(), "signature": sig}
			blocks = append(blocks, block)
			return true
		})
		if len(blocks) > 0 {
			return blocks
		}
	}
	return nil
}
