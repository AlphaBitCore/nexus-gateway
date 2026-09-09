package gemini

import "github.com/tidwall/gjson"

// The two decisions the extract and rewrite walks MUST agree on, in one place.
//
// traffic.Adapter binds ExtractRequest and RewriteRequestBody to walk the schema
// in the same order: content.Segments[i] pairs with the i-th extractable slot.
// A disagreement here is not a cosmetic drift — the rewrite returns no error, so
// the pipeline records the request as redacted while plaintext crosses the wire.
//
// Both decisions had drifted, and each cost a real leak:
//
//   - THINKING PARTS. Extract routes thought=true text to ReasoningSegments and
//     off Segments; the rewrite wrote into every text part. For parts
//     [A, thought, B] the rewrite put redact(A) in A, redact(B) into the THINKING
//     part, then ran out of segments and returned — leaving B unredacted on its
//     way to the provider, and corrupting the thought text Gemini requires echoed
//     back verbatim in a multi-turn conversation. The response side had the same
//     shape, returning unredacted assistant text to the client.
//
//   - SYSTEM INSTRUCTION. Extract reads both systemInstruction.parts and
//     system_instruction.parts (the protobuf spelling, which Google's JSON
//     surface accepts); the rewrite read only the camelCase one. So a snake_case
//     request started its segment index at the first user message: the system
//     prompt's redaction went into that message, and the last message was never
//     rewritten at all.
//
// Every walk on both sides reads these, so the two halves cannot disagree again.

// systemInstructionKeys are the spellings a Gemini request may use for the
// system prompt, in the order both walks visit them. Google's JSON surface
// accepts the protobuf snake_case form alongside the documented camelCase one,
// and a caller that uses it must still be redacted.
var systemInstructionKeys = []string{"systemInstruction.parts", "system_instruction.parts"}

// isReasoningPart reports whether a parts[] entry carries thinking text rather
// than user-visible text. Gemini 2.5+ marks reasoning with thought=true.
//
// Reasoning is kept OFF Segments (mirroring Anthropic thinking and OpenAI
// reasoning_content), so the rewrite must skip exactly the same parts or every
// slot after the first thought is off by one.
func isReasoningPart(part gjson.Result) bool {
	return part.Get("thought").Bool()
}
