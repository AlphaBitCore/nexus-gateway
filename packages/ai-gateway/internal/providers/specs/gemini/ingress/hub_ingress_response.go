// The RESPONSE leg of the Gemini hub ingress: canonical OpenAI chat completion
// out to a Gemini generateContent response. Split from hub_ingress.go, which
// keeps the request leg — the two directions share no state and change for
// different reasons.
package ingress

import (
	"fmt"

	"github.com/goccy/go-json"

	"github.com/tidwall/gjson"
)

// OpenAIChatCompletionToGenerateContentResponse converts canonical OpenAI
// chat.completion JSON into a Gemini `generateContent` response envelope.
func OpenAIChatCompletionToGenerateContentResponse(openaiBody []byte) ([]byte, error) {
	if len(openaiBody) == 0 {
		return nil, fmt.Errorf("gemini hub: empty openai response")
	}
	root := gjson.ParseBytes(openaiBody)

	msg := root.Get("choices.0.message")
	text := msg.Get("content").String()
	var parts []map[string]any
	if tcs := msg.Get("tool_calls"); tcs.Exists() && tcs.IsArray() {
		tcs.ForEach(func(_, tc gjson.Result) bool {
			fn := tc.Get("function")
			args := fn.Get("arguments").String()
			if args == "" {
				args = "{}"
			}
			var argsObj any
			_ = json.Unmarshal([]byte(args), &argsObj)
			if argsObj == nil {
				argsObj = map[string]any{}
			}
			fc := map[string]any{
				"name": fn.Get("name").String(),
				"args": argsObj,
			}
			// Only forward id when canonical carried one. Older Gemini
			// models reject unknown fields on request bodies; the
			// response shape mirrors that and clients tolerate the
			// absence. See codec.go openAIMessageToGeminiParts.
			if id := tc.Get("id").String(); id != "" {
				fc["id"] = id
			}
			parts = append(parts, map[string]any{
				"functionCall": fc,
			})
			return true
		})
	}
	if text != "" {
		parts = append([]map[string]any{{"text": text}}, parts...)
	}
	// Cross-format reasoning preservation: canonical reasoning_content
	// → Gemini `{text:"...", thought:true}` part. Matches the L1→L2
	// forward path that already collects Gemini `thought:true` parts
	// AND OpenAI/Anthropic/DeepSeek-shape reasoning into the canonical
	// reasoning_content field. Prepended so the thinking summary
	// appears before the visible text in the candidate's parts — same
	// ordering Gemini 2.5+ uses natively when
	// generationConfig.thinkingConfig.includeThoughts is set.
	if r := msg.Get("reasoning_content").String(); r != "" {
		parts = append([]map[string]any{{"text": r, "thought": true}}, parts...)
	}
	if len(parts) == 0 {
		parts = []map[string]any{{"text": ""}}
	}

	finish := mapOpenAIFinishToGemini(root.Get("choices.0.finish_reason").String())

	cand := map[string]any{
		"index":        0,
		"content":      map[string]any{"parts": parts, "role": "model"},
		"finishReason": finish,
	}

	usageMeta := map[string]any{}
	if u := root.Get("usage"); u.Exists() {
		if v := u.Get("prompt_tokens"); v.Exists() {
			usageMeta["promptTokenCount"] = v.Int()
		}
		if v := u.Get("completion_tokens"); v.Exists() {
			usageMeta["candidatesTokenCount"] = v.Int()
		}
		if v := u.Get("total_tokens"); v.Exists() {
			usageMeta["totalTokenCount"] = v.Int()
		}
		// Cache-hit token count. The canonical chat-completions shape
		// carries this as `prompt_tokens_details.cached_tokens`
		// (Anthropic's cross-format codec also restores cache_read_*
		// fields here — see specutil.cachedTokenAliases). Gemini's
		// native response field is `cachedContentTokenCount`; without
		// this translation, cross-routed requests that hit upstream
		// cache silently return usageMetadata WITHOUT
		// cachedContentTokenCount — the client-visible Gemini envelope
		// would not reflect the cache hit even though traffic_event records it.
		if v := u.Get("prompt_tokens_details.cached_tokens"); v.Exists() && v.Int() > 0 {
			usageMeta["cachedContentTokenCount"] = v.Int()
		}
		// Reasoning tokens — Gemini exposes thoughts as a separate count.
		// Canonical maps to OpenAI's completion_tokens_details.reasoning_tokens
		// (specutil.cachedTokenAliases). When present, surface as
		// thoughtsTokenCount on the Gemini envelope so clients that show
		// reasoning effort don't see 0.
		if v := u.Get("completion_tokens_details.reasoning_tokens"); v.Exists() && v.Int() > 0 {
			usageMeta["thoughtsTokenCount"] = v.Int()
		}
	}

	out := map[string]any{
		"responseId":    root.Get("id").String(),
		"modelVersion":  root.Get("model").String(),
		"candidates":    []map[string]any{cand},
		"usageMetadata": usageMeta,
	}
	return json.Marshal(out)
}

func mapOpenAIFinishToGemini(r string) string {
	switch r {
	case "stop":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "tool_calls":
		return "STOP"
	default:
		if r == "" {
			return "STOP"
		}
		return "OTHER"
	}
}
