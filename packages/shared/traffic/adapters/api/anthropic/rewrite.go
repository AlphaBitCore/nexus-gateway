package anthropic

import (
	"context"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// RewriteRequestBody reverses ExtractRequest for the Anthropic Messages API.
//
// Iteration order mirrors extractor: first the `system` prompt (string or
// [{type:"text", text:"…"}]), then each `messages[i].content` slot (same
// two shapes). Non-text blocks (tool_use, tool_result, image) are skipped
// and left untouched.
func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, content traffic.NormalizedContent) ([]byte, int, error) {
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

	// 1) system (optional).
	sys := gjson.GetBytes(out, "system")
	if sys.Exists() {
		switch {
		case sys.Type == gjson.String:
			if segIdx >= len(content.Segments) {
				return out, written, nil
			}
			out, err = sjson.SetBytes(out, "system", content.Segments[segIdx])
			if err != nil {
				return nil, written, fmt.Errorf("anthropic: rewrite system: %w", err)
			}
			segIdx++
			written++
		case sys.IsArray():
			parts := sys.Array()
			for pIdx := range parts {
				if parts[pIdx].Get("type").Str != "text" {
					continue
				}
				if segIdx >= len(content.Segments) {
					return out, written, nil
				}
				p := fmt.Sprintf("system.%d.text", pIdx)
				out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
				if err != nil {
					return nil, written, fmt.Errorf("anthropic: rewrite %s: %w", p, err)
				}
				segIdx++
				written++
			}
		}
	}

	// 2) messages[].content.
	msgList := gjson.GetBytes(out, "messages").Array()
	for mIdx := range msgList {
		c := msgList[mIdx].Get("content")
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
				return nil, written, fmt.Errorf("anthropic: rewrite %s: %w", p, err)
			}
			segIdx++
			written++
		case c.IsArray():
			parts := c.Array()
			for pIdx := range parts {
				switch parts[pIdx].Get("type").Str {
				case "text":
					if segIdx >= len(content.Segments) {
						return out, written, nil
					}
					p := fmt.Sprintf("messages.%d.content.%d.text", mIdx, pIdx)
					out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
					if err != nil {
						return nil, written, fmt.Errorf("anthropic: rewrite %s: %w", p, err)
					}
					segIdx++
					written++
				case "tool_result":
					tc := parts[pIdx].Get("content")
					switch {
					case tc.Type == gjson.String:
						if segIdx >= len(content.Segments) {
							return out, written, nil
						}
						p := fmt.Sprintf("messages.%d.content.%d.content", mIdx, pIdx)
						out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
						if err != nil {
							return nil, written, fmt.Errorf("anthropic: rewrite %s: %w", p, err)
						}
						segIdx++
						written++
					case tc.IsArray():
						subs := tc.Array()
						for sIdx := range subs {
							if subs[sIdx].Get("type").Str != "text" {
								continue
							}
							if segIdx >= len(content.Segments) {
								return out, written, nil
							}
							p := fmt.Sprintf("messages.%d.content.%d.content.%d.text", mIdx, pIdx, sIdx)
							out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
							if err != nil {
								return nil, written, fmt.Errorf("anthropic: rewrite %s: %w", p, err)
							}
							segIdx++
							written++
						}
					}
				}
			}
		}
	}
	return out, written, nil
}

// RewriteResponseBody reverses ExtractResponse for Anthropic Messages API
// non-streaming responses (top-level content[] text blocks).
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, content traffic.NormalizedContent) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, traffic.ErrMalformed
	}
	c := gjson.GetBytes(body, "content")
	if !c.Exists() || !c.IsArray() {
		return nil, 0, traffic.ErrUnknownSchema
	}
	out := body
	segIdx := 0
	written := 0
	var err error
	parts := c.Array()
	for pIdx := range parts {
		if parts[pIdx].Get("type").Str != "text" {
			continue
		}
		if segIdx >= len(content.Segments) {
			return out, written, nil
		}
		p := fmt.Sprintf("content.%d.text", pIdx)
		out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
		if err != nil {
			return nil, written, fmt.Errorf("anthropic: rewrite response %s: %w", p, err)
		}
		segIdx++
		written++
	}
	return out, written, nil
}
