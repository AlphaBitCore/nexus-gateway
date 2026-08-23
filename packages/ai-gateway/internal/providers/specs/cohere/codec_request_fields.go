package cohere

// codec_request_fields.go — the Cohere v2 /chat request wire shape.
//
// Cohere rejects any field it does not know with a 422, so an adapter that
// forwards the canonical body verbatim ships a 422 for every canonical-only
// field it happens to carry. Subtracting known offenders one at a time does not
// converge, and production showed exactly that: the deploy that folded
// max_completion_tokens went out around 05:00 UTC on 2026-08-05, and by 12:00
// the same five Cohere models were failing on `nexus` — our own extension
// namespace, which the Responses ingress legitimately stamps onto the canonical
// body (provider-adapter-architecture §3a) and which every non-OpenAI adapter is
// responsible for translating away. One field fixed, the next one surfaced.
//
// So the encoder projects onto a DECLARED set instead. A canonical field that is
// not named here never reaches the wire, which makes the next one a no-op rather
// than the next incident.
//
// The set is established BOTH ways, because neither source alone is sufficient
// for a projection.
//
//   - Measured: each name below was sent to api.cohere.com/v2/chat on 2026-08-05
//     and its verdict recorded. This is what catches a wrong belief — the comment
//     this replaces asserted "top_p stays top_p (matches Cohere's `p` field via
//     OpenAI alias)", and top_p is in fact a 422, so every request carrying the
//     most common sampling parameter in the canonical shape was failing under a
//     comment saying it was handled.
//   - Enumerated from https://docs.cohere.com/reference/chat. Probing can only
//     falsify a name someone thought to probe; for a projection the risk runs the
//     other way — a field Cohere accepts and nobody listed is dropped SILENTLY,
//     which is a quieter defect than a 422 and a worse one. The first draft of
//     this set, built from probes alone, was missing `thinking` and `priority`.
//
// Cohere distinguishes the two failures precisely, which is what makes the set
// checkable: 422 "unknown field" / "unrecognized content type" means the wire
// does not know the name, while 400 means it knows the name and something about
// this request is wrong (thinking with too small a max_tokens, thinking on a
// model that does not support it, a thinking block outside an assistant message).
// Only the 422 class belongs in a filter here.

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// cohereChatWireFields is the top-level field set Cohere v2 /chat accepts — all
// 21 parameters the API reference enumerates, each confirmed against the live
// endpoint on 2026-08-05.
//
// `logprobs`, `thinking` and `priority` are here despite failing on some
// requests, because their failure is a 400 naming a MODEL or REQUEST condition
// ("logprobs is not supported with the specified model", "`thinking` parameter is
// not supported with the specified model", "thinking.token_budget must be less
// than or equal to max_tokens") rather than a 422 saying the wire does not know
// the name. Filtering those here would replace a true per-model capability answer
// with our own silence.
var cohereChatWireFields = map[string]struct{}{
	"model": {}, "messages": {}, "stream": {}, "temperature": {},
	"max_tokens": {}, "p": {}, "k": {}, "stop_sequences": {},
	"presence_penalty": {}, "frequency_penalty": {}, "seed": {},
	"safety_mode": {}, "response_format": {}, "citation_options": {},
	"tools": {}, "tool_choice": {}, "documents": {}, "strict_tools": {},
	"logprobs": {}, "thinking": {}, "priority": {},
}

// cohereChatRenames maps a canonical (OpenAI) spelling to Cohere's spelling for
// the same concept. Every entry is a measured 422 on the left and an accepted
// field on the right; a rename wins over a value already present under the
// target name, matching the anthropic and gemini codecs' precedence for the two
// max-token spellings.
var cohereChatRenames = map[string]string{
	"top_p":                 "p",
	"top_k":                 "k",
	"stop":                  "stop_sequences",
	"max_completion_tokens": "max_tokens",
}

// projectToCohereChat returns body reduced to what Cohere v2 /chat accepts,
// with canonical spellings renamed.
//
// Dropping is safe for the top level in a way it would NOT be for message
// content: a sampling parameter Cohere cannot express is a degraded request,
// while a content block it cannot express is a request whose meaning changed —
// the model would answer about material it never received. Content is therefore
// handled separately and refused rather than dropped.
func projectToCohereChat(body []byte) []byte {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body
	}
	out := []byte(`{}`)
	// Renames are applied last so a canonical spelling wins over a value that
	// already sat under the target name.
	var renamed [][2]any
	root.ForEach(func(k, v gjson.Result) bool {
		key := k.String()
		if target, ok := cohereChatRenames[key]; ok {
			renamed = append(renamed, [2]any{target, v})
			return true
		}
		if _, ok := cohereChatWireFields[key]; ok {
			out, _ = sjson.SetRawBytes(out, escapeJSONPath(key), []byte(v.Raw))
		}
		return true
	})
	for _, r := range renamed {
		out, _ = sjson.SetRawBytes(out, escapeJSONPath(r[0].(string)), []byte(r[1].(gjson.Result).Raw))
	}
	return out
}

// escapeJSONPath escapes the characters sjson treats as path syntax. Every key
// this package writes is a fixed identifier with none of them, but the helper
// keeps that a property of the data rather than of the reader's memory.
func escapeJSONPath(key string) string {
	const specials = ".*?|#@"
	esc := make([]byte, 0, len(key))
	for i := range len(key) {
		for j := range len(specials) {
			if key[i] == specials[j] {
				esc = append(esc, '\\')
				break
			}
		}
		esc = append(esc, key[i])
	}
	return string(esc)
}
