package minimax

import (
	"context"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// RewriteRequestBody reverses ExtractRequest for MiniMax. Iteration order
// matches the extractor: optional top-level `prompt` first (native
// format), then each messages[i].text (native) OR messages[i].content
// (openai-compatible), detected from the first message's shape.
func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, content traffic.NormalizedContent) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, traffic.ErrMalformed
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return nil, 0, traffic.ErrUnknownSchema
	}

	out := body
	segIdx := 0
	written := 0
	var err error

	// 1) top-level prompt (native format, only emitted when non-empty).
	prompt := gjson.GetBytes(out, "prompt")
	if prompt.Exists() && prompt.Type == gjson.String && prompt.Str != "" {
		if segIdx >= len(content.Segments) {
			return out, written, nil
		}
		out, err = sjson.SetBytes(out, "prompt", content.Segments[segIdx])
		if err != nil {
			return nil, written, fmt.Errorf("minimax: rewrite prompt: %w", err)
		}
		segIdx++
		written++
	}

	// 2) messages[].text or messages[].content.
	msgList := gjson.GetBytes(out, "messages").Array()
	if len(msgList) == 0 {
		return out, written, nil
	}
	isNative := msgList[0].Get("text").Exists()

	for mIdx := range msgList {
		var field string
		if isNative {
			t := msgList[mIdx].Get("text")
			if !t.Exists() || t.Type != gjson.String {
				continue
			}
			field = "text"
		} else {
			c := msgList[mIdx].Get("content")
			if !c.Exists() || c.Type != gjson.String {
				continue
			}
			field = "content"
		}
		if segIdx >= len(content.Segments) {
			return out, written, nil
		}
		p := fmt.Sprintf("messages.%d.%s", mIdx, field)
		out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
		if err != nil {
			return nil, written, fmt.Errorf("minimax: rewrite %s: %w", p, err)
		}
		segIdx++
		written++
	}
	return out, written, nil
}

// RewriteResponseBody reverses ExtractResponse for MiniMax non-streaming
// responses (choices[].message.text native, or .message.content compat).
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, content traffic.NormalizedContent) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, traffic.ErrMalformed
	}
	choices := gjson.GetBytes(body, "choices")
	if !choices.Exists() || !choices.IsArray() {
		return nil, 0, traffic.ErrUnknownSchema
	}
	out := body
	segIdx := 0
	written := 0
	var err error

	choiceList := choices.Array()
	for cIdx := range choiceList {
		text := choiceList[cIdx].Get("message.text")
		if text.Exists() && text.Type == gjson.String {
			if segIdx >= len(content.Segments) {
				return out, written, nil
			}
			p := fmt.Sprintf("choices.%d.message.text", cIdx)
			out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
			if err != nil {
				return nil, written, fmt.Errorf("minimax: rewrite response %s: %w", p, err)
			}
			segIdx++
			written++
			continue
		}
		msgContent := choiceList[cIdx].Get("message.content")
		if msgContent.Exists() && msgContent.Type == gjson.String {
			if segIdx >= len(content.Segments) {
				return out, written, nil
			}
			p := fmt.Sprintf("choices.%d.message.content", cIdx)
			out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
			if err != nil {
				return nil, written, fmt.Errorf("minimax: rewrite response %s: %w", p, err)
			}
			segIdx++
			written++
		}
	}
	return out, written, nil
}
