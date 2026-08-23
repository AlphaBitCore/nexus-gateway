// Message-level translation from Anthropic's content blocks into canonical
// OpenAI chat messages: text, thinking, image, document, tool_use, tool_result.
// Split from hub_ingress.go because this is the part that grows with every new
// content kind rather than with the request surface.
package ingress

import (
	"strings"

	"github.com/tidwall/gjson"
)

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
	// Assistant thinking history: without collecting it here the blocks
	// are silently dropped at Anthropic-ingress→canonical, and a
	// cross-format target that needs the reasoning back (DeepSeek's
	// thinking mode) gets an empty "" back-fill masking real text. The
	// text becomes reasoning_content — the L2 universal field the response
	// side already uses (see OpenAIChatCompletionToMessagesResponse). Each
	// block keeps its OWN signature (Anthropic validates a signature
	// against the exact thinking content it signed) under the
	// nexus_thinking message field, which the bridge drops before a
	// non-Anthropic upstream sees it (see addReasoning).
	var reasoningLines []string
	var thinkingBlocks []map[string]any

	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "text":
			textLines = append(textLines, part.Get("text").String())
		case "thinking":
			t := part.Get("thinking").String()
			if t != "" {
				reasoningLines = append(reasoningLines, t)
			}
			block := map[string]any{"thinking": t}
			if sig := part.Get("signature").String(); sig != "" {
				block["signature"] = sig
			}
			thinkingBlocks = append(thinkingBlocks, block)
		case "redacted_thinking":
			// Anthropic emits redacted_thinking for safety-filtered
			// reasoning; it carries opaque `data`, no plaintext. Preserve
			// it verbatim as a block so the round-trip re-emits it, but it
			// contributes no reasoning_content text.
			thinkingBlocks = append(thinkingBlocks, map[string]any{
				"redacted_data": part.Get("data").String(),
			})
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
		case "document":
			// The inverse of the codec's canonical file → Anthropic document
			// encode. Anthropic ingress carrying a PDF had no canonical
			// landing place, so the block fell through untranslated and the
			// document never reached a non-Anthropic target.
			src := part.Get("source")
			file := map[string]any{}
			switch src.Get("type").String() {
			case "url":
				file["file_url"] = src.Get("url").String()
			case "base64":
				file["file_data"] = "data:" + src.Get("media_type").String() + ";base64," + src.Get("data").String()
			case "file":
				file["file_id"] = src.Get("file_id").String()
			}
			if t := part.Get("title").String(); t != "" {
				file["filename"] = t
			}
			if len(file) > 0 {
				images = append(images, map[string]any{"type": "file", "file": file})
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
		addReasoning(entry, reasoningLines, thinkingBlocks)
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
	// Thinking legitimately appears only on assistant turns (the Gemini
	// sibling gates the same way); never stamp reasoning onto a user
	// message even if a malformed part slipped through.
	if role == "assistant" {
		addReasoning(entry, reasoningLines, thinkingBlocks)
	}
	if len(images) == 0 && len(textLines) == 1 {
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
