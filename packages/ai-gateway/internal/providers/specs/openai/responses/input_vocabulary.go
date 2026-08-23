package responses

import (
	"bytes"

	"github.com/tidwall/gjson"
)

// The /v1/responses wire carries a FIXED set of input content parts, and audio
// is not among them. OpenAI's own rejection enumerates the whole vocabulary —
// observed 21 times in production, every row model gpt-audio-mini:
//
//	Invalid value: 'input_audio'. Supported values are: 'input_text',
//	'input_image', 'output_text', 'refusal', 'input_file',
//	'computer_screenshot', 'summary_text', and 'encrypted_content'.
//
// Of those, input_text, input_image and input_file are the parts a caller may
// SEND; the rest are output or echo types the wire produces.
//
// Accept-list, not denylist: a part type nobody has sent yet lands OUTSIDE the
// rule and is handled, rather than in a denylist's blind spot on the way to a
// 400. `input_video` will need no edit here when it exists.
//
// Live-probed: gpt-audio-mini accepts input_audio on /v1/chat/completions and
// rejects it here, which is what makes the downgrade a real answer.
var responsesInputParts = map[string]bool{
	"input_text":  true,
	"input_image": true,
	"input_file":  true,
}

// Only `input[].content[]` entries spelled `input_*` are examined — those are
// the caller-authored parts. An item echoed back from a previous turn
// (function_call_output, reasoning, output_text) asks the wire to carry no new
// content, and treating one as unservable would downgrade a working
// conversation. A string `input` carries text only, so it never triggers.
func ExceedsInputVocabulary(body []byte) (string, bool) {
	if len(body) == 0 || !hasUnservedInputToken(body) {
		return "", false
	}
	if !gjson.ValidBytes(body) {
		return "", false
	}
	items := gjson.GetBytes(body, "input")
	if !items.IsArray() {
		return "", false
	}
	var found string
	items.ForEach(func(_, item gjson.Result) bool {
		content := item.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, part gjson.Result) bool {
			t := part.Get("type").String()
			if len(t) < 6 || t[:6] != "input_" {
				return true
			}
			if !responsesInputParts[t] {
				found = t
				return false
			}
			return true
		})
		return found == ""
	})
	return found, found != ""
}

// A byte scan that parses and allocates nothing, because the authoritative walk
// is expensive on exactly the bodies that need it: gjson.GetBytes COPIES the
// value it returns, so reading `input` on a request carrying a 32 KB audio part
// allocated 49 KB and cost ~100 µs — and the predicate calling this runs up to
// five times per request (routing, cache-prep, bridge, once per failover
// target). A large SERVED image paid the same 49 KB to answer "nothing to do".
//
// The error direction is the point. A false YES only sends the walk to look. A
// false NO forwards an unservable part to a 400, which a literal scan alone did:
// `{"type":"\u0069nput_audio"}` spells the same part type and the scan cannot
// see it. \uXXXX is the only JSON escape producing an arbitrary character, so a
// body carrying `\u` anywhere goes to the walk and everything else stops at the
// scan.
func hasUnservedInputToken(body []byte) bool {
	const tok = `"input_`
	for i := 0; ; {
		j := bytes.Index(body[i:], []byte(tok))
		if j < 0 {
			return mayHideAToken(body)
		}
		start := i + j + 1 // first byte of the name, past the opening quote
		end := start
		for end < len(body) && body[end] != '"' {
			end++
		}
		if end >= len(body) {
			return mayHideAToken(body) // unterminated; let the walk decide
		}
		if !responsesInputParts[string(body[start:end])] {
			return true
		}
		i = end + 1
	}
}

// Answering yes costs a walk; answering no wrongly costs the caller a 400 on a
// wire that could have served them.
func mayHideAToken(body []byte) bool {
	return bytes.Contains(body, []byte(`\u`))
}
