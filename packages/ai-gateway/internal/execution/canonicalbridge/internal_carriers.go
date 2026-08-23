package canonicalbridge

import (
	"bytes"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// StripInternalCarriersForTarget removes provider-private carrier fields that
// must not egress to a target whose codec does not consume them. Anthropic and
// Bedrock consume nexus_thinking; Gemini and Vertex consume
// tool_calls[].function.thought_signature. Every other target strips both.
// reasoning_content remains universal.
//
// The gate is a DENYLIST by consumer, not an allowlist by wire shape: only the
// Anthropic-family codecs read nexus_thinking to rebuild signed thinking blocks
// (Anthropic natively, Bedrock by delegating to the Anthropic codec — Claude on
// AWS), so ONLY those two preserve it. Every other target either forwards the
// canonical messages VERBATIM (the OpenAI identity codec AND the Cohere v2
// passthrough, which both accept the canonical `messages` array as-is — either
// would leak the carrier) or field-maps them ignoring it (Gemini/Vertex); for
// all of those, stripping is both safe and required. Keying on "consumes" not
// "wire == OpenAIChat" means a future verbatim codec defaults to strip, closing
// the class instead of re-opening it per adapter.
//
// Both canonical→wire egress legs MUST call this: IngressChatToWire (the
// cross-format failover leg) invokes it internally; the cache-prep primary leg
// (stage_cache_body's prepareUpstreamBody, whose prepared bytes attempt-0 sends
// and which never reaches IngressChatToWire) calls it explicitly.
func (b *Bridge) StripInternalCarriersForTarget(canon []byte, target provcore.Format) []byte {
	if !targetConsumesNexusThinking(target) {
		canon = stripNexusThinking(canon)
	}
	if !targetConsumesThoughtSignature(target) {
		canon = stripThoughtSignatures(canon)
	}
	return canon
}

// targetConsumesNexusThinking reports whether a target's codec reads the
// nexus_thinking carrier to reconstruct Anthropic signed thinking blocks — the
// ONLY targets for which it must survive canonical→wire egress. Anthropic reads
// it natively; Bedrock delegates its EncodeRequest to the Anthropic codec, so a
// Claude-on-Bedrock target reconstructs the same signed blocks.
func targetConsumesNexusThinking(target provcore.Format) bool {
	switch target {
	case provcore.FormatAnthropic, provcore.FormatBedrock:
		return true
	default:
		return false
	}
}

func targetConsumesThoughtSignature(target provcore.Format) bool {
	switch target {
	case provcore.FormatGemini, provcore.FormatVertex:
		return true
	default:
		return false
	}
}

// stripNexusThinking removes the nexus_thinking field from every message that
// carries it. Gated on a substring pre-check so a body with no replayed
// thinking pays only a linear scan; on a body that has it, only the messages
// that actually carry the field pay a delete (the ForEach value guards against
// a full-body sjson walk per thinking-free message).
func stripNexusThinking(canon []byte) []byte {
	if !bytes.Contains(canon, []byte(`"nexus_thinking"`)) &&
		!bytes.Contains(canon, []byte(`\u`)) {
		return canon
	}
	msgs := gjson.GetBytes(canon, "messages")
	if !msgs.IsArray() {
		return canon
	}
	out := canon
	msgs.ForEach(func(idx, msg gjson.Result) bool {
		if !msg.Get("nexus_thinking").Exists() {
			return true
		}
		if deleted, derr := sjson.DeleteBytes(out, "messages."+idx.String()+".nexus_thinking"); derr == nil {
			out = deleted
		}
		return true
	})
	return out
}

func stripThoughtSignatures(canon []byte) []byte {
	if !bytes.Contains(canon, []byte(`"thought_signature"`)) &&
		!bytes.Contains(canon, []byte(`\u`)) {
		return canon
	}
	msgs := gjson.GetBytes(canon, "messages")
	if !msgs.IsArray() {
		return canon
	}
	out := canon
	msgs.ForEach(func(msgIdx, msg gjson.Result) bool {
		calls := msg.Get("tool_calls")
		if !calls.IsArray() {
			return true
		}
		calls.ForEach(func(callIdx, call gjson.Result) bool {
			if !call.Get("function.thought_signature").Exists() {
				return true
			}
			path := "messages." + msgIdx.String() + ".tool_calls." + callIdx.String() + ".function.thought_signature"
			if deleted, err := sjson.DeleteBytes(out, path); err == nil {
				out = deleted
			}
			return true
		})
		return true
	})
	return out
}
