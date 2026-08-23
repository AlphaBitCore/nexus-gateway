package ingress

import (
	"crypto/sha1"
	"fmt"
	"github.com/goccy/go-json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
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
		// `responseSchema` is how this wire spells structured output, and the
		// canonical body is the OpenAI shape (§3a), so it becomes
		// `response_format.json_schema`. Without this the schema stopped here and
		// whatever target routing picked was asked for JSON with no shape.
		//
		// `responseMimeType: application/json` on its OWN is the json_object case,
		// not this one: it asks for "some JSON", which every target either honours
		// natively or is instructed into, so treating it as a schema constraint
		// would narrow the routing pool for a requirement no model fails.
		//
		// The envelope's `name` comes from specutil because OpenAI requires it and
		// this wire has no field to take it from — see CanonicalSchemaName.
		if rs := gc.Get("responseSchema"); rs.IsObject() {
			var m map[string]any
			if err := json.Unmarshal([]byte(rs.Raw), &m); err == nil && len(m) > 0 {
				out["response_format"] = map[string]any{
					"type":        "json_schema",
					"json_schema": specutil.CanonicalJSONSchema(m),
				}
			}
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
	// Pending tool calls awaiting their response, keyed by function name and
	// held FIFO so parallel calls to the same function are matched in order.
	//
	// Gemini pairs a functionResponse with its functionCall by NAME when the
	// model emits no id — which is every model before Gemini 3. Canonical
	// OpenAI pairs them by tool_call_id, so the id we synthesize for the call
	// has to be the id the response quotes. Deriving them independently is what
	// broke: the call hashed name+args into call_<hash> while the response fell
	// back to the bare name, so the two never matched and an OpenAI-compatible
	// upstream saw a tool result referring to a call it had never been given.
	pendingCallIDs := map[string][]string{}
	contents.ForEach(func(contentIndex, c gjson.Result) bool {
		role := c.Get("role").String()
		openAIRole := role
		if role == "model" {
			openAIRole = "assistant"
		}
		if openAIRole == "" {
			openAIRole = "user"
		}
		text := ""
		reasoning := ""
		var toolCalls []any
		var toolMsgs []map[string]any
		var images []map[string]any
		parts := c.Get("parts")
		if parts.IsArray() {
			parts.ForEach(func(partIndex, p gjson.Result) bool {
				if t := p.Get("text"); t.Exists() {
					// A thought part is the model's reasoning, not visible
					// content — folding it into text would corrupt a
					// replayed history and mislead the router/hooks. Route
					// it to reasoning_content, the L2 universal field, so
					// the response-side symmetry (reasoning_content →
					// {text,thought:true}) round-trips instead of the
					// thinking summary leaking into the assistant's answer.
					if p.Get("thought").Bool() {
						if reasoning != "" {
							reasoning += "\n"
						}
						reasoning += t.String()
						return true
					}
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
						if isImageMime(mime) {
							images = append(images, map[string]any{
								"type":      "image_url",
								"image_url": map[string]any{"url": url, "detail": "auto"},
							})
						} else {
							images = append(images, map[string]any{
								"type": "file",
								"file": map[string]any{"file_data": url},
							})
						}
					}
				}
				if file := p.Get("fileData"); file.Exists() {
					if uri := file.Get("fileUri").String(); uri != "" {
						// Gemini uses one part shape for every attachment, so
						// the declared mime type is the only thing that says
						// which modality it is. Reading them all as images made
						// a PDF arrive at the next wire claiming to be one —
						// the same masquerade the canonical's separate file
						// part exists to prevent.
						if isImageMime(file.Get("mimeType").String()) {
							images = append(images, map[string]any{
								"type":      "image_url",
								"image_url": map[string]any{"url": uri, "detail": "auto"},
							})
						} else {
							images = append(images, map[string]any{
								"type": "file",
								"file": map[string]any{"file_url": uri},
							})
						}
					}
				}
				if fc := p.Get("functionCall"); fc.Exists() {
					args := fc.Get("args").Raw
					if args == "" {
						args = "{}"
					}
					fnName := fc.Get("name").String()
					id := fc.Get("id").String()
					if id == "" {
						id = geminiSyntheticCallID(fnName, args, int(contentIndex.Int()), int(partIndex.Int()))
					}
					pendingCallIDs[fnName] = append(pendingCallIDs[fnName], id)
					function := map[string]any{
						"name":      fnName,
						"arguments": args,
					}
					// thoughtSignature belongs to this exact Gemini Part, not
					// to the function name or to the enclosing content. Keep it
					// on the canonical function so parallel identical calls do
					// not exchange provider-native signatures.
					if sig := p.Get("thoughtSignature").String(); sig != "" {
						function["thought_signature"] = sig
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":       id,
						"type":     "function",
						"function": function,
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
					// Prefer Gemini 3+ functionResponse.id as the canonical
					// OpenAI tool_call_id. Without one, quote the id assigned
					// to the matching earlier functionCall — same name, FIFO
					// for parallel calls — so the pair correlates on the
					// canonical side exactly as it does on the Gemini wire.
					// The bare name is the last resort, for a response whose
					// call is not in this request (a truncated history).
					tid := fr.Get("id").String()
					if tid == "" {
						if q := pendingCallIDs[name]; len(q) > 0 {
							tid, pendingCallIDs[name] = q[0], q[1:]
						} else {
							tid = name
						}
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
				messages = append(messages, geminiCompositeMessage(openAIRole, text, reasoning, images, nil))
			}
			messages = append(messages, toolMsgs...)
			return true
		}
		messages = append(messages, geminiCompositeMessage(openAIRole, text, reasoning, images, toolCalls))
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

	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}

	// Preserve the caller's `thinkingConfig` so the canonical → wire encode can
	// re-inject it, the same way the Anthropic ingress preserves `thinking`.
	//
	// Without this the field dies on this leg: the codec's read side keys off
	// `nexus.ext.gemini.thinking_config`, and nothing in the repo was setting
	// it — so a Gemini client asking for a thinking budget got a request with
	// no budget, and the only sign was the answer arriving without the
	// reasoning it paid for. `thinkingBudget: -1` (Gemini for "you decide") is
	// carried through unchanged; it is an expression, not an absent value.
	if tc := root.Get("generationConfig.thinkingConfig"); tc.Exists() && tc.IsObject() {
		var cfg any
		if jerr := json.Unmarshal([]byte(tc.Raw), &cfg); jerr == nil && cfg != nil {
			body, err = canonicalext.Set(body, "gemini", "thinking_config", cfg)
			if err != nil {
				return nil, err
			}
		}
		// The extension above is only legible to this wire. Every OTHER wire
		// reads the canonical level, so without it a caller who sized their
		// reasoning in tokens arrives at an Anthropic or OpenAI target having
		// said nothing at all.
		if effort := canonicalEffortForThinkingConfig(tc); effort != "" {
			body, err = sjson.SetBytes(body, "reasoning_effort", effort)
			if err != nil {
				return nil, err
			}
		}
	}
	return body, nil
}

// geminiSyntheticCallID is stable for one request and collision-free for
// duplicate name/argument pairs because the content and Part coordinates are
// part of the digest. Native Gemini ids are handled by the caller and never
// pass through this fallback.
func geminiSyntheticCallID(name, _ string, contentIndex, partIndex int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s\x00content:%d\x00part:%d", name, contentIndex, partIndex)))
	return "call_" + fmt.Sprintf("%x", h)[:10]
}

// canonicalEffortForThinkingConfig states a Gemini `thinkingConfig` in the
// canonical vocabulary, or "" when it says nothing a level can carry.
//
// The caller's own figure is not lost — it rides in this provider's extension,
// so a Gemini target restores it exactly. This is what the request says to
// everyone else.
func canonicalEffortForThinkingConfig(tc gjson.Result) string {
	b := tc.Get("thinkingBudget")
	switch {
	case !b.Exists():
		// Only `includeThoughts`. That asks for the reasoning to come BACK, not
		// for any particular amount of it, so there is no level to state — and
		// inventing one would make a request about the response's shape into a
		// request for more reasoning than the caller asked to pay for.
		return ""
	case b.Int() == 0:
		return "none"
	case b.Int() < 0:
		// Gemini's "you decide". It is an ask to reason with the amount left
		// open, which no level in the canonical vocabulary means — so this is
		// the one place a figure is chosen rather than read. The exact -1 stays
		// in the extension, so a Gemini target still gets "you decide".
		return "medium"
	default:
		return normcore.EffortForBudget(int(b.Int()))
	}
}

// geminiCompositeMessage assembles a canonical OpenAI chat message that may
// carry text, image_url parts, and (assistant-side) tool_calls. Pure text
// turns stay as a string content field for compatibility with strict OpenAI
// SDKs; mixed content collapses to the parts-array form.
func geminiCompositeMessage(role, text, reasoning string, images []map[string]any, toolCalls []any) map[string]any {
	entry := map[string]any{"role": role}
	if role == "assistant" && reasoning != "" {
		entry["reasoning_content"] = reasoning
	}
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

// isImageMime reports whether a Gemini attachment's declared mime type is an
// image. An absent mime is treated as an image: that was the behaviour for
// every attachment before this split, and a URI with no declared type is far
// more often an image than a document.
func isImageMime(mime string) bool {
	return mime == "" || strings.HasPrefix(mime, "image/")
}
