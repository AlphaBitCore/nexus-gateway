package ingress

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
)

// GenerateContentRequestToOpenAIChatCompletion converts a Gemini
// `generateContent` JSON body into canonical OpenAI chat.completions JSON.
// model must be the resolved Gemini model id (often taken from the URL path).
func GenerateContentRequestToOpenAIChatCompletion(native []byte, model string) ([]byte, error) {
	if len(native) == 0 {
		return nil, fmt.Errorf("gemini hub: empty body")
	}
	root := gjson.ParseBytes(native)
	if model == "" {
		model = root.Get("model").String()
	}
	if model == "" {
		return nil, fmt.Errorf("gemini hub: missing model")
	}
	out := map[string]any{"model": model}

	if gc := root.Get("generationConfig"); gc.Exists() {
		if v := gc.Get("temperature"); v.Exists() {
			out["temperature"] = v.Float()
		}
		if v := gc.Get("topP"); v.Exists() {
			out["top_p"] = v.Float()
		}
		if v := gc.Get("topK"); v.Exists() {
			out["top_k"] = v.Int()
		}
		if v := gc.Get("maxOutputTokens"); v.Exists() {
			out["max_tokens"] = v.Int()
		}
		if ss := gc.Get("stopSequences"); ss.Exists() && ss.IsArray() {
			var list []string
			ss.ForEach(func(_, v gjson.Result) bool {
				list = append(list, v.String())
				return true
			})
			if len(list) == 1 {
				out["stop"] = list[0]
			} else if len(list) > 1 {
				out["stop"] = list
			}
		}
	}

	var messages []map[string]any
	if si := root.Get("systemInstruction.parts"); si.Exists() && si.IsArray() {
		var sys string
		si.ForEach(func(_, p gjson.Result) bool {
			if p.Get("text").Exists() {
				if sys != "" {
					sys += "\n"
				}
				sys += p.Get("text").String()
			}
			return true
		})
		if sys != "" {
			messages = append(messages, map[string]any{"role": "system", "content": sys})
		}
	}

	contents := root.Get("contents")
	if !contents.Exists() || !contents.IsArray() {
		return nil, fmt.Errorf("gemini hub: missing contents")
	}
	contents.ForEach(func(_, c gjson.Result) bool {
		role := c.Get("role").String()
		openAIRole := role
		if role == "model" {
			openAIRole = "assistant"
		}
		if openAIRole == "" {
			openAIRole = "user"
		}
		text := ""
		var toolCalls []any
		var toolMsgs []map[string]any
		var images []map[string]any
		parts := c.Get("parts")
		if parts.IsArray() {
			parts.ForEach(func(_, p gjson.Result) bool {
				if t := p.Get("text"); t.Exists() {
					if text != "" {
						text += "\n"
					}
					text += t.String()
				}
				if inline := p.Get("inlineData"); inline.Exists() {
					mime := inline.Get("mimeType").String()
					data := inline.Get("data").String()
					if data != "" {
						url := "data:" + mime + ";base64," + data
						images = append(images, map[string]any{
							"type":      "image_url",
							"image_url": map[string]any{"url": url, "detail": "auto"},
						})
					}
				}
				if file := p.Get("fileData"); file.Exists() {
					if uri := file.Get("fileUri").String(); uri != "" {
						images = append(images, map[string]any{
							"type":      "image_url",
							"image_url": map[string]any{"url": uri, "detail": "auto"},
						})
					}
				}
				if fc := p.Get("functionCall"); fc.Exists() {
					args := fc.Get("args").Raw
					if args == "" {
						args = "{}"
					}
					id := fc.Get("id").String()
					if id == "" {
						h := sha1.Sum([]byte(fc.Get("name").String() + "\x00" + args))
						id = "call_" + fmt.Sprintf("%x", h)[:10]
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":   id,
						"type": "function",
						"function": map[string]any{
							"name":      fc.Get("name").String(),
							"arguments": args,
						},
					})
				}
				if fr := p.Get("functionResponse"); fr.Exists() {
					name := fr.Get("name").String()
					resp := fr.Get("response")
					var contentStr string
					if resp.Exists() {
						contentStr = resp.Raw
						if resp.Type == gjson.String {
							contentStr = resp.String()
						}
					}
					// Prefer Gemini 3+ functionResponse.id as the
					// canonical OpenAI tool_call_id; fall back to name
					// for older models that don't emit id (canonical
					// callers still get a stable identifier and the
					// codec encode-side echoes it back to Gemini).
					tid := fr.Get("id").String()
					if tid == "" {
						tid = name
					}
					toolMsgs = append(toolMsgs, map[string]any{
						"role":         "tool",
						"tool_call_id": tid,
						"content":      contentStr,
					})
				}
				return true
			})
		}
		if len(toolMsgs) > 0 {
			if text != "" || len(images) > 0 {
				messages = append(messages, geminiCompositeMessage(openAIRole, text, images, nil))
			}
			messages = append(messages, toolMsgs...)
			return true
		}
		messages = append(messages, geminiCompositeMessage(openAIRole, text, images, toolCalls))
		return true
	})
	if len(messages) == 0 {
		return nil, fmt.Errorf("gemini hub: no messages from contents")
	}
	out["messages"] = messages

	if tools := root.Get("tools"); tools.IsArray() && len(tools.Array()) > 0 {
		var canonicalTools []map[string]any
		tools.ForEach(func(_, toolGroup gjson.Result) bool {
			toolGroup.Get("functionDeclarations").ForEach(func(_, fn gjson.Result) bool {
				name := fn.Get("name").String()
				if name == "" {
					return true
				}
				canonicalFn := map[string]any{
					"name":        name,
					"description": fn.Get("description").String(),
				}
				if params := fn.Get("parameters"); params.Exists() && params.Raw != "" {
					var paramsObj any
					if err := json.Unmarshal([]byte(params.Raw), &paramsObj); err == nil && paramsObj != nil {
						canonicalFn["parameters"] = paramsObj
					}
				}
				canonicalTools = append(canonicalTools, map[string]any{
					"type":     "function",
					"function": canonicalFn,
				})
				return true
			})
			return true
		})
		if len(canonicalTools) > 0 {
			out["tools"] = canonicalTools
		}
	}
	if cfg := root.Get("toolConfig.functionCallingConfig"); cfg.Exists() {
		mode := cfg.Get("mode").String()
		allowed := cfg.Get("allowedFunctionNames")
		switch mode {
		case "AUTO":
			out["tool_choice"] = "auto"
		case "NONE":
			out["tool_choice"] = "none"
		case "ANY":
			if allowed.IsArray() && len(allowed.Array()) == 1 {
				out["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": allowed.Array()[0].String()},
				}
			} else {
				out["tool_choice"] = "required"
			}
		}
	}

	return json.Marshal(out)
}

// geminiCompositeMessage assembles a canonical OpenAI chat message that may
// carry text, image_url parts, and (assistant-side) tool_calls. Pure text
// turns stay as a string content field for compatibility with strict OpenAI
// SDKs; mixed content collapses to the parts-array form.
func geminiCompositeMessage(role, text string, images []map[string]any, toolCalls []any) map[string]any {
	entry := map[string]any{"role": role}
	if role == "assistant" && len(toolCalls) > 0 {
		entry["tool_calls"] = toolCalls
	}
	if len(images) == 0 {
		if role == "assistant" && len(toolCalls) > 0 && text == "" {
			entry["content"] = nil
		} else {
			entry["content"] = text
		}
		return entry
	}
	parts := make([]any, 0, len(images)+1)
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	for _, im := range images {
		parts = append(parts, im)
	}
	entry["content"] = parts
	return entry
}

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
