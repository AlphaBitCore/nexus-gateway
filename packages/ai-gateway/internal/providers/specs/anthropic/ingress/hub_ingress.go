package ingress

import (
	"fmt"
	"github.com/goccy/go-json"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	"github.com/tidwall/gjson"
)

// MessagesRequestToOpenAIChatCompletion converts an Anthropic Messages API
// request body into the canonical OpenAI chat.completions JSON shape used
// across the AI Gateway hub. It is the inverse of [codec.EncodeRequest] for
// the supported subset (text-first messages, optional system, sampling knobs).
func MessagesRequestToOpenAIChatCompletion(native []byte, providerModelID string) ([]byte, error) {
	if len(native) == 0 {
		return nil, fmt.Errorf("anthropic hub: empty body")
	}
	root := gjson.ParseBytes(native)
	model := root.Get("model").String()
	if providerModelID != "" {
		model = providerModelID
	}
	if model == "" {
		return nil, fmt.Errorf("anthropic hub: missing model")
	}
	out := map[string]any{"model": model}

	if v := root.Get("max_tokens"); v.Exists() {
		out["max_tokens"] = v.Int()
	}
	if v := root.Get("temperature"); v.Exists() {
		out["temperature"] = v.Float()
	}
	if v := root.Get("top_p"); v.Exists() {
		out["top_p"] = v.Float()
	}
	if v := root.Get("top_k"); v.Exists() {
		out["top_k"] = v.Int()
	}
	if v := root.Get("stream"); v.Exists() {
		out["stream"] = v.Bool()
	}
	if ss := root.Get("stop_sequences"); ss.Exists() {
		switch {
		case ss.IsArray():
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
		case ss.Type == gjson.String:
			out["stop"] = ss.String()
		}
	}

	var messages []map[string]any

	sys := root.Get("system")
	if sys.Exists() {
		if sys.Type == gjson.String {
			if s := sys.String(); s != "" {
				messages = append(messages, map[string]any{"role": "system", "content": s})
			}
		} else if sys.IsArray() {
			var parts []string
			sys.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					parts = append(parts, part.Get("text").String())
				}
				return true
			})
			if len(parts) > 0 {
				joined := parts[0]
				for i := 1; i < len(parts); i++ {
					joined += "\n" + parts[i]
				}
				messages = append(messages, map[string]any{"role": "system", "content": joined})
			}
		}
	}

	msgs := root.Get("messages")
	if !msgs.Exists() || !msgs.IsArray() {
		return nil, fmt.Errorf("anthropic hub: missing messages array")
	}
	msgs.ForEach(func(_, msg gjson.Result) bool {
		converted := anthropicMessageToOpenAI(msg)
		messages = append(messages, converted...)
		return true
	})

	if len(messages) == 0 {
		return nil, fmt.Errorf("anthropic hub: no messages")
	}
	out["messages"] = messages

	if tools := root.Get("tools"); tools.IsArray() && len(tools.Array()) > 0 {
		var canonicalTools []map[string]any
		tools.ForEach(func(_, t gjson.Result) bool {
			name := t.Get("name").String()
			if name == "" {
				return true
			}
			fn := map[string]any{
				"name":        name,
				"description": t.Get("description").String(),
			}
			if schema := t.Get("input_schema"); schema.Exists() && schema.Raw != "" {
				var paramsObj any
				if err := json.Unmarshal([]byte(schema.Raw), &paramsObj); err == nil && paramsObj != nil {
					fn["parameters"] = paramsObj
				}
			}
			canonicalTools = append(canonicalTools, map[string]any{
				"type":     "function",
				"function": fn,
			})
			return true
		})
		if len(canonicalTools) > 0 {
			out["tools"] = canonicalTools
		}
	}
	if tc := root.Get("tool_choice"); tc.Exists() && tc.IsObject() {
		switch tc.Get("type").String() {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			if name := tc.Get("name").String(); name != "" {
				out["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}

	// G7: preserve Anthropic-native `thinking` configuration so the
	// canonical → wire encode (codec.EncodeRequest, line ~250) can
	// re-inject it. Without this, an Anthropic ingress client that
	// asks for extended thinking would silently lose the config when
	// the canonical body is routed through the broker / cache layer.
	// The codec's read side already keys off nexus.ext.anthropic.thinking.
	if thinking := root.Get("thinking"); thinking.Exists() && thinking.IsObject() {
		var thinkingObj any
		if jerr := json.Unmarshal([]byte(thinking.Raw), &thinkingObj); jerr == nil && thinkingObj != nil {
			body, err = canonicalext.Set(body, "anthropic", "thinking", thinkingObj)
			if err != nil {
				return nil, err
			}
		}
	}
	return body, nil
}

func anthropicMessageToOpenAI(msg gjson.Result) []map[string]any {
	role := msg.Get("role").String()
	if role == "" {
		role = "user"
	}
	content := msg.Get("content")
	if content.Type == gjson.String {
		return []map[string]any{{"role": role, "content": content.String()}}
	}
	if !content.IsArray() {
		return []map[string]any{{"role": role, "content": ""}}
	}

	var textLines []string
	var images []map[string]any
	var toolUseBlocks []gjson.Result
	var toolResults []map[string]any

	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "text":
			textLines = append(textLines, part.Get("text").String())
		case "image":
			src := part.Get("source")
			switch src.Get("type").String() {
			case "url":
				images = append(images, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url":    src.Get("url").String(),
						"detail": "auto",
					},
				})
			case "base64":
				mime := src.Get("media_type").String()
				data := src.Get("data").String()
				images = append(images, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url":    "data:" + mime + ";base64," + data,
						"detail": "auto",
					},
				})
			}
		case "tool_use":
			toolUseBlocks = append(toolUseBlocks, part)
		case "tool_result":
			toolResults = append(toolResults, map[string]any{
				"role":         "tool",
				"tool_call_id": part.Get("tool_use_id").String(),
				"content":      StringifyAnthropicToolResult(part.Get("content")),
			})
		}
		return true
	})

	if len(toolResults) > 0 {
		out := make([]map[string]any, 0, len(toolResults)+1)
		if joined := strings.Join(textLines, "\n"); joined != "" {
			out = append(out, map[string]any{"role": "user", "content": joined})
		}
		out = append(out, toolResults...)
		return out
	}

	if role == "assistant" && len(toolUseBlocks) > 0 {
		var tcalls []any
		for _, part := range toolUseBlocks {
			input := part.Get("input")
			args := input.Raw
			if args == "" {
				args = "{}"
			}
			tcalls = append(tcalls, map[string]any{
				"id":   part.Get("id").String(),
				"type": "function",
				"function": map[string]any{
					"name":      part.Get("name").String(),
					"arguments": args,
				},
			})
		}
		entry := map[string]any{
			"role":       "assistant",
			"tool_calls": tcalls,
		}
		if len(textLines) > 0 || len(images) > 0 {
			var parts []any
			for _, line := range textLines {
				if line != "" {
					parts = append(parts, map[string]any{"type": "text", "text": line})
				}
			}
			for _, im := range images {
				parts = append(parts, im)
			}
			if len(parts) == 1 {
				if m, ok := parts[0].(map[string]any); ok && m["type"] == "text" {
					entry["content"] = m["text"]
				} else {
					entry["content"] = parts
				}
			} else if len(parts) > 0 {
				entry["content"] = parts
			}
		}
		return []map[string]any{entry}
	}

	entry := map[string]any{"role": role}
	if len(images) == 0 && len(textLines) <= 1 && len(textLines) == 1 {
		entry["content"] = textLines[0]
		return []map[string]any{entry}
	}
	var parts []any
	for _, line := range textLines {
		if line != "" {
			parts = append(parts, map[string]any{"type": "text", "text": line})
		}
	}
	for _, im := range images {
		parts = append(parts, im)
	}
	switch {
	case len(parts) == 0:
		entry["content"] = ""
	case len(parts) == 1:
		if m, ok := parts[0].(map[string]any); ok && m["type"] == "text" {
			entry["content"] = m["text"]
		} else {
			entry["content"] = parts
		}
	default:
		entry["content"] = parts
	}
	return []map[string]any{entry}
}

// StringifyAnthropicToolResult converts an Anthropic tool_result content value
// to a plain string for the canonical tool message. Exported for test access.
func StringifyAnthropicToolResult(c gjson.Result) string {
	if c.Type == gjson.String {
		return c.String()
	}
	if c.IsArray() {
		var lines []string
		c.ForEach(func(_, p gjson.Result) bool {
			if p.Get("type").String() == "text" {
				lines = append(lines, p.Get("text").String())
			}
			return true
		})
		return strings.Join(lines, "\n")
	}
	return c.Raw
}

// OpenAIChatCompletionToMessagesResponse converts a canonical OpenAI
// chat.completion JSON body into an Anthropic Messages API response shape.
// It mirrors the lossy mapping performed by [codec.DecodeResponse] in reverse.
func OpenAIChatCompletionToMessagesResponse(openaiBody []byte) ([]byte, error) {
	if len(openaiBody) == 0 {
		return nil, fmt.Errorf("anthropic hub: empty openai response")
	}
	root := gjson.ParseBytes(openaiBody)

	msg := root.Get("choices.0.message")
	if !msg.Exists() {
		return nil, fmt.Errorf("anthropic hub: missing choices.0.message")
	}

	var content []map[string]any
	// Cross-format reasoning preservation: when the canonical body
	// carries reasoning_content (the L2 universal field set by
	// upstreams returning thinking — OpenAI o-series / gpt-5 /
	// DeepSeek / Moonshot / Kimi), reconstruct an Anthropic-native
	// `thinking` content block so an Anthropic SDK client that
	// cross-routed to one of those upstreams still sees the model's
	// reasoning. Without this, calling /v1/messages with model="gpt-5"
	// would silently drop the reasoning text — the L2→L3 reverse
	// projection's symmetric counterpart to the L1→L2 forward path
	// that already collects `thinking` blocks into reasoning_content.
	// Per Anthropic's API contract, thinking blocks ride alongside
	// text blocks in the content array; we emit them first to match
	// the upstream's natural block ordering.
	if r := msg.Get("reasoning_content").String(); r != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": r})
	}
	toolCalls := msg.Get("tool_calls")
	if toolCalls.Exists() && toolCalls.IsArray() {
		toolCalls.ForEach(func(_, tc gjson.Result) bool {
			fn := tc.Get("function")
			args := fn.Get("arguments").String()
			if args == "" {
				args = "{}"
			}
			var inputObj map[string]any
			if err := json.Unmarshal([]byte(args), &inputObj); err != nil || inputObj == nil {
				inputObj = map[string]any{}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    tc.Get("id").String(),
				"name":  fn.Get("name").String(),
				"input": inputObj,
			})
			return true
		})
	}
	text := StringifyOpenAIMessageContent(msg.Get("content"))
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	finish := MapOpenAIFinishToStopReason(root.Get("choices.0.finish_reason").String())

	usage := map[string]any{}
	if u := root.Get("usage"); u.Exists() {
		if v := u.Get("prompt_tokens"); v.Exists() {
			usage["input_tokens"] = v.Int()
		}
		if v := u.Get("completion_tokens"); v.Exists() {
			usage["output_tokens"] = v.Int()
		}
		// Restore Anthropic-native cache usage fields from canonical extension.
		if v := u.Get("prompt_tokens_details.cached_tokens"); v.Exists() && v.Int() > 0 {
			usage["cache_read_input_tokens"] = v.Int()
		}
	}
	// cache_creation_input_tokens is stored in the canonical extension by the codec.
	if ext := root.Get("nexus.ext.anthropic.cache_creation_input_tokens"); ext.Exists() && ext.Int() > 0 {
		usage["cache_creation_input_tokens"] = ext.Int()
	}

	out := map[string]any{
		"id":          root.Get("id").String(),
		"type":        "message",
		"role":        "assistant",
		"content":     content,
		"model":       root.Get("model").String(),
		"stop_reason": finish,
		"usage":       usage,
	}
	return json.Marshal(out)
}

// StringifyOpenAIMessageContent converts an OpenAI message content value
// to a plain string. Exported for test access.
func StringifyOpenAIMessageContent(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		var buf string
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				if t := part.Get("text"); t.Exists() {
					if t.Type == gjson.String {
						if buf != "" {
							buf += "\n"
						}
						buf += t.String()
					} else if s := t.Get("value"); s.Exists() {
						if buf != "" {
							buf += "\n"
						}
						buf += s.String()
					}
				}
			}
			return true
		})
		return buf
	}
	return ""
}

// MapOpenAIFinishToStopReason maps an OpenAI finish_reason to an Anthropic stop_reason.
// Exported for test access.
func MapOpenAIFinishToStopReason(r string) string {
	switch r {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "stop_sequence"
	default:
		if r == "" {
			return "end_turn"
		}
		return r
	}
}
