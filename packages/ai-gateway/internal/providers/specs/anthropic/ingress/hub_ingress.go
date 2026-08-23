package ingress

import (
	"fmt"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
	// `output_config.format` is how this wire spells structured output, and the
	// canonical body is the OpenAI shape (§3a), so it becomes `response_format`.
	//
	// Without this the schema stopped here. Measured live on prod: `/v1/messages`
	// with `model: auto` and a schema routed to gpt-5.6-terra — the routing
	// dimension had correctly picked a model that honours schemas — and answered
	// `{"should_respond": true, "probe_id": "..."}`. Valid JSON, wrong keys: the
	// model was told to produce JSON and never told the shape.
	//
	// `output_config.effort` is deliberately not read here: it is reasoning depth,
	// not a response format, and a body carrying only that asks for no schema.
	// Inventing one would narrow the routing pool for a constraint the caller
	// never wrote.
	if f := root.Get("output_config.format"); f.IsObject() &&
		f.Get("type").String() == "json_schema" {
		// The schema rides along only when the caller sent one. An absent schema
		// is the shape Anthropic itself rejects with "output_config.format.schema:
		// Field required", and fabricating one here would answer for them.
		//
		// The envelope's `name` is filled by specutil because the canonical body
		// IS an OpenAI body and OpenAI requires it — measured live: without it an
		// OpenAI target answers 400 "Missing required parameter:
		// 'response_format.json_schema.name'" while an Anthropic target serves the
		// same request fine, so the omission only shows up on half the fleet.
		var m map[string]any
		if sch := f.Get("schema"); sch.IsObject() {
			if err := json.Unmarshal([]byte(sch.Raw), &m); err != nil {
				m = nil
			}
		}
		out["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": specutil.CanonicalJSONSchema(m),
		}
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
		// The extension above is only legible to this wire. Every OTHER wire
		// reads the canonical level, so without it a caller who sized their
		// reasoning in tokens arrives at an OpenAI or Gemini target having said
		// nothing at all — and a caller who DISABLED thinking arrives at a
		// reasoning model that reasons anyway, which they pay for.
		if effort := canonicalEffortForThinking(thinking); effort != "" {
			body, err = sjson.SetBytes(body, "reasoning_effort", effort)
			if err != nil {
				return nil, err
			}
		}
	}
	return body, nil
}

// canonicalEffortForThinking states an Anthropic `thinking` block in the
// canonical vocabulary, or "" when the block says nothing a level can carry.
//
// The exact budget is not thrown away — it rides in this provider's extension,
// so an Anthropic target restores the caller's own figure. This is what the
// request says to everyone else.
func canonicalEffortForThinking(thinking gjson.Result) string {
	if thinking.Get("type").String() == "disabled" {
		return "none"
	}
	if b := thinking.Get("budget_tokens"); b.Exists() {
		return normcore.EffortForBudget(int(b.Int()))
	}
	// Enabled with no budget. Anthropic's own API rejects this, so it is not a
	// shape we can round-trip — but it is unambiguously an ask to reason, and
	// dropping it would turn a malformed request into a silently unreasoned
	// answer instead of the upstream 400 the caller would otherwise see.
	if thinking.Get("type").String() == "enabled" {
		return "medium"
	}
	return ""
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
		// Canonical→Anthropic is a UNIT CONVENTION change, not a copy, and it
		// lives in ToAnthropicCounters so the streaming encoder applies exactly
		// the same one. See that function for what the two conventions are and
		// what the overlap cost in production.
		prompt := u.Get("prompt_tokens").Int()
		cacheRead := u.Get("prompt_tokens_details.cached_tokens").Int()
		cacheCreation := u.Get("prompt_tokens_details.cache_creation_tokens").Int()
		c := ToAnthropicCounters(prompt, cacheRead, cacheCreation)
		if c.Contradictory {
			WarnContradictoryUsage(root.Get("model").String(), prompt, cacheRead, cacheCreation)
		}
		if u.Get("prompt_tokens").Exists() {
			usage["input_tokens"] = c.InputTokens
		}
		if v := u.Get("completion_tokens"); v.Exists() {
			usage["output_tokens"] = v.Int()
		}
		if c.CacheReadTokens > 0 {
			usage["cache_read_input_tokens"] = c.CacheReadTokens
		}
		if c.CacheCreationTokens > 0 {
			usage["cache_creation_input_tokens"] = c.CacheCreationTokens
		}
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
//
// content_filter → "refusal", the documented Anthropic value for "when
// streaming classifiers intervene to handle potential policy violations".
// It previously produced "stop_sequence", which claims the caller's OWN
// custom stop string was generated — a different fact, not a coarser one.
// Must stay in lockstep with canonicalbridge.canonicalFinishToAnthropicStop.
func MapOpenAIFinishToStopReason(r string) string {
	switch r {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		if r == "" {
			return "end_turn"
		}
		return r
	}
}
