// Package proxy — json_object_unwrap.go repairs the one case where
// `response_format: {"type": "json_object"}` can still reach the caller as
// unparseable text.
//
// On api.openai.com, json_object is a hard guarantee: the content parses. The
// Anthropic Messages API has no equivalent mode, so the anthropic codec forces
// JSON with a system instruction that explicitly says "do not wrap it in
// markdown code fences" (anthropicJSONObjectInstruction). A system instruction
// is a request, not a constraint — observed on staging 2026-07-27,
// claude-haiku-4-5 answered a json_object request with
//
//	```json\n{\n  "name": "Margaret Chen",\n  "age": 34\n}\n```
//
// which fails json.loads / JSON.parse outright. Prompt-based enforcement cannot
// be made reliable, so the fence is removed here instead.
//
// Deliberately narrow. The unwrap runs only when ALL of these hold:
//   - the request actually asked for json_object (stamped at request time — the
//     codecs are stateless and never see the originating request);
//   - the whole content is ONE fenced block, nothing before or after it;
//   - the fence's contents parse as JSON.
//
// So a caller who asked for prose containing a fenced example keeps their
// fence, and a body that is already bare JSON is untouched.
//
// SCOPE: non-streaming only. Doing this on a stream would mean buffering the
// whole completion to see whether a trailing fence arrives, which would destroy
// the time-to-first-token that streaming exists for. A streamed json_object
// request against a non-OpenAI target can therefore still deliver fenced
// content; that remains a documented divergence.
package proxy

import (
	"strings"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// jsonObjectRequested reports whether the request asked for
// response_format.type == "json_object". Stamped into rec.Metadata at request
// time by preStampJSONObjectMeta.
func jsonObjectRequested(existing any) bool {
	md, ok := existing.(map[string]any)
	if !ok {
		return false
	}
	chat, ok := md["chat"].(map[string]any)
	if !ok {
		return false
	}
	v, _ := chat["json_object_requested"].(bool)
	return v
}

// preStampJSONObjectMeta records that the caller asked for json_object so the
// response path can enforce the guarantee without re-reading the request body.
// Returns the updated metadata value for assignment back onto rec.Metadata.
func preStampJSONObjectMeta(existing any, reqBody []byte) any {
	if gjson.GetBytes(reqBody, "response_format.type").String() != "json_object" {
		return existing
	}
	md := mergeIntoMetadataMap(existing)
	chat, ok := md["chat"].(map[string]any)
	if !ok {
		chat = map[string]any{}
	}
	chat["json_object_requested"] = true
	md["chat"] = chat
	return md
}

// unwrapJSONObjectFences strips a markdown code fence from each assistant
// message content in a canonical chat.completion body, when the request asked
// for json_object and the fence wraps nothing but JSON. Returns the body
// unchanged when the request did not ask for json_object, when no choice needed
// unwrapping, or on any rewrite failure — a fenced body is a caller-visible bug,
// but a half-rewritten one is worse.
func unwrapJSONObjectFences(respBody []byte, metadata any) []byte {
	if !jsonObjectRequested(metadata) || len(respBody) == 0 {
		return respBody
	}
	choices := gjson.GetBytes(respBody, "choices")
	if !choices.IsArray() {
		return respBody
	}

	out := respBody
	changed := false
	failed := false
	choices.ForEach(func(idx, choice gjson.Result) bool {
		content := choice.Get("message.content")
		if content.Type != gjson.String {
			return true
		}
		stripped, ok := stripJSONCodeFence(content.Str)
		if !ok {
			return true
		}
		next, err := sjson.SetBytes(out, "choices."+idx.String()+".message.content", stripped)
		if err != nil {
			failed = true
			return false
		}
		out = next
		changed = true
		return true
	})
	if failed || !changed {
		return respBody
	}
	return out
}

// stripJSONCodeFence removes a single surrounding ```-fence and reports whether
// it did. Requires the trimmed content to open with ``` and close with ```, with
// the inner text parsing as JSON — the parse check is what keeps prose that
// merely happens to be fenced from being rewritten.
func stripJSONCodeFence(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "```") || !strings.HasSuffix(trimmed, "```") {
		return "", false
	}
	body := trimmed[3:]
	body = body[:len(body)-3]
	// Drop an optional language tag on the opening line ("json", "JSON", …).
	if nl := strings.IndexByte(body, '\n'); nl >= 0 {
		if tag := strings.TrimSpace(body[:nl]); tag == "" || isBareWord(tag) {
			body = body[nl+1:]
		}
	}
	body = strings.TrimSpace(body)
	if body == "" || !json.Valid([]byte(body)) {
		return "", false
	}
	return body, true
}

// isBareWord reports whether s is a single alphanumeric token, i.e. a plausible
// code-fence language tag rather than the first line of a JSON payload.
func isBareWord(s string) bool {
	if len(s) == 0 || len(s) > 16 {
		return false
	}
	for _, r := range s {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}
