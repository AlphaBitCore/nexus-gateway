package openai

import (
	"context"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// RewriteRequestBody reverses ExtractRequest: it walks the same schema slots
// in the same order and overwrites text content with segments[i].
//
// Supported endpoints:
//   - /chat/completions — messages[i].content in both string and
//     [{type:"text", text:"…"}, …] shapes.
//   - /responses — input as string OR array of items whose content is a
//     string or an array of parts with type in {"input_text","text"}.
//
// /embeddings is intentionally unsupported: its content is a top-level
// `input` field (a single or list of arbitrary user strings) — rewriting a
// redacted variant end-to-end is not yet implemented. Returning
// ErrRewriteUnsupported makes the AG proxy fall through to forwarding the
// original body with a warn log instead of 500ing.
func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	switch {
	case strings.Contains(path, "/chat/completions"):
		return rewriteChatRequest(body, content)
	case strings.Contains(path, "/responses"):
		return rewriteResponsesCreate(body, content)
	case strings.Contains(path, "/embeddings"):
		return nil, 0, traffic.ErrRewriteUnsupported
	default:
		return nil, 0, traffic.ErrRewriteUnsupported
	}
}

// rewriteChatRequest updates messages[i].content in-place. It mirrors the
// extractor's iteration order precisely: for each message either the top
// string content slot is replaced, or each {type:"text"} part inside an
// array content is replaced. Non-text parts (images, tool_calls, …) are
// left untouched, and extra segments beyond the schema's text slot count
// are silently dropped.
func rewriteChatRequest(body []byte, content traffic.NormalizedContent) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, traffic.ErrMalformed
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() {
		return nil, 0, traffic.ErrUnknownSchema
	}

	out := body
	segIdx := 0
	written := 0
	var err error

	msgList := messages.Array()
	for mIdx := range msgList {
		msg := msgList[mIdx]
		c := msg.Get("content")
		if !c.Exists() {
			continue
		}
		switch {
		case c.Type == gjson.String:
			if segIdx >= len(content.Segments) {
				return out, written, nil
			}
			p := fmt.Sprintf("messages.%d.content", mIdx)
			out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
			if err != nil {
				return nil, written, fmt.Errorf("openai: rewrite %s: %w", p, err)
			}
			segIdx++
			written++
		case c.IsArray():
			parts := c.Array()
			for pIdx := range parts {
				if parts[pIdx].Get("type").Str != "text" {
					continue
				}
				if segIdx >= len(content.Segments) {
					return out, written, nil
				}
				p := fmt.Sprintf("messages.%d.content.%d.text", mIdx, pIdx)
				out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
				if err != nil {
					return nil, written, fmt.Errorf("openai: rewrite %s: %w", p, err)
				}
				segIdx++
				written++
			}
		}
	}
	return out, written, nil
}

// rewriteResponsesCreate mirrors extractResponsesCreate: either a top-level
// input string, or an array whose items have a content field that is a
// string or an array of {type in {"input_text","text"}, text:"…"} parts.
func rewriteResponsesCreate(body []byte, content traffic.NormalizedContent) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, traffic.ErrMalformed
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return nil, 0, traffic.ErrUnknownSchema
	}

	out := body
	segIdx := 0
	written := 0
	var err error

	switch {
	case input.Type == gjson.String:
		if segIdx >= len(content.Segments) {
			return out, 0, nil
		}
		out, err = sjson.SetBytes(out, "input", content.Segments[segIdx])
		if err != nil {
			return nil, 0, fmt.Errorf("openai: rewrite input: %w", err)
		}
		return out, 1, nil
	case input.IsArray():
		items := input.Array()
		for iIdx := range items {
			c := items[iIdx].Get("content")
			switch {
			case c.Type == gjson.String:
				if segIdx >= len(content.Segments) {
					return out, written, nil
				}
				p := fmt.Sprintf("input.%d.content", iIdx)
				out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
				if err != nil {
					return nil, written, fmt.Errorf("openai: rewrite %s: %w", p, err)
				}
				segIdx++
				written++
			case c.IsArray():
				parts := c.Array()
				for pIdx := range parts {
					t := parts[pIdx].Get("type").Str
					if t != "input_text" && t != "text" {
						continue
					}
					if segIdx >= len(content.Segments) {
						return out, written, nil
					}
					p := fmt.Sprintf("input.%d.content.%d.text", iIdx, pIdx)
					out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
					if err != nil {
						return nil, written, fmt.Errorf("openai: rewrite %s: %w", p, err)
					}
					segIdx++
					written++
				}
			}
		}
	}
	return out, written, nil
}

// RewriteResponseBody reverses ExtractResponse for supported non-streaming
// endpoints (chat/completions assistant message content, responses API
// output_text parts). Embeddings responses are unsupported.
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	switch {
	case strings.Contains(path, "/chat/completions"):
		return rewriteChatResponseBody(body, content)
	case strings.Contains(path, "/embeddings"):
		return nil, 0, traffic.ErrRewriteUnsupported
	case strings.Contains(path, "/responses"):
		return rewriteResponsesResponseBody(body, content)
	default:
		return nil, 0, traffic.ErrRewriteUnsupported
	}
}

func rewriteChatResponseBody(body []byte, content traffic.NormalizedContent) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, traffic.ErrMalformed
	}
	choices := gjson.GetBytes(body, "choices")
	if !choices.Exists() {
		return nil, 0, traffic.ErrUnknownSchema
	}
	out := body
	segIdx := 0
	written := 0
	var err error

	choiceList := choices.Array()
	for cIdx := range choiceList {
		// Slot order matches extractChatResponse: message.content first,
		// then message.refusal. Either slot may be absent — when it is,
		// no segment was emitted for it, so we skip without consuming
		// from segments[].
		if msgContent := choiceList[cIdx].Get("message.content"); msgContent.Exists() && msgContent.Type == gjson.String {
			if segIdx >= len(content.Segments) {
				return out, written, nil
			}
			p := fmt.Sprintf("choices.%d.message.content", cIdx)
			out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
			if err != nil {
				return nil, written, fmt.Errorf("openai: rewrite response %s: %w", p, err)
			}
			segIdx++
			written++
		}
		if r := choiceList[cIdx].Get("message.refusal"); r.Exists() && r.Type == gjson.String && r.Str != "" {
			if segIdx >= len(content.Segments) {
				return out, written, nil
			}
			p := fmt.Sprintf("choices.%d.message.refusal", cIdx)
			out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
			if err != nil {
				return nil, written, fmt.Errorf("openai: rewrite response %s: %w", p, err)
			}
			segIdx++
			written++
		}
	}
	return out, written, nil
}

func rewriteResponsesResponseBody(body []byte, content traffic.NormalizedContent) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, traffic.ErrMalformed
	}
	output := gjson.GetBytes(body, "output")
	if !output.Exists() || !output.IsArray() {
		return nil, 0, traffic.ErrUnknownSchema
	}
	out := body
	segIdx := 0
	written := 0
	var err error

	items := output.Array()
	for oIdx := range items {
		contentArr := items[oIdx].Get("content")
		if !contentArr.IsArray() {
			continue
		}
		parts := contentArr.Array()
		for pIdx := range parts {
			if parts[pIdx].Get("type").Str != "output_text" {
				continue
			}
			if segIdx >= len(content.Segments) {
				return out, written, nil
			}
			p := fmt.Sprintf("output.%d.content.%d.text", oIdx, pIdx)
			out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
			if err != nil {
				return nil, written, fmt.Errorf("openai: rewrite response %s: %w", p, err)
			}
			segIdx++
			written++
		}
	}
	return out, written, nil
}
