// Package deepseekweb implements the deepseek-web traffic adapter for
// browser-side traffic to chat.deepseek.com (DeepSeek's consumer chat
// product). Distinct from api.deepseek.com (handled by the deepseek
// adapter — programmatic API).
//
// chat.deepseek.com's wire format is undocumented; the adapter follows
// the defensive pattern established by other web adapters.
package deepseekweb

import (
	"context"
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "deepseek-web"

type Adapter struct{}

func (a *Adapter) ID() string                       { return adapterID }
func (a *Adapter) Configure(_ map[string]any) error { return nil }

func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	if !looksLikeJSON(body) {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	var segments, toolCalls []string
	var reasoning []string

	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		msgs.ForEach(func(_, msg gjson.Result) bool {
			if content := msg.Get("content"); content.Type == gjson.String {
				segments = append(segments, content.Str)
			} else if content := msg.Get("content"); content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if part.Get("type").Str == "text" {
						segments = append(segments, part.Get("text").Str)
					}
					return true
				})
			}
			if tc := msg.Get("tool_calls"); tc.IsArray() {
				tc.ForEach(func(_, call gjson.Result) bool {
					toolCalls = append(toolCalls, call.Raw)
					return true
				})
			}
			// DeepSeek's reasoner content surfaces in messages too.
			if rc := msg.Get("reasoning_content"); rc.Type == gjson.String && rc.Str != "" {
				reasoning = append(reasoning, rc.Str)
			}
			return true
		})
	}

	for _, key := range []string{"prompt", "query", "text", "input"} {
		if v := gjson.GetBytes(body, key); v.Type == gjson.String && v.Str != "" {
			segments = append(segments, v.Str)
		}
	}

	if len(segments) == 0 && len(toolCalls) == 0 && len(reasoning) == 0 {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String && model.Str != "" {
		meta["model"] = model.Str
	}
	if conv := gjson.GetBytes(body, "chat_id"); conv.Type == gjson.String && conv.Str != "" {
		meta["chat_id"] = conv.Str
	}

	return traffic.NormalizedContent{
		Segments:          segments,
		ReasoningSegments: reasoning,
		ToolCallSegments:  toolCalls,
		Metadata:          meta,
	}, nil
}

func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, nil
	}
	if !looksLikeJSON(body) {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	if errMsg := gjson.GetBytes(body, "error.message"); errMsg.Type == gjson.String && errMsg.Str != "" {
		return traffic.NormalizedContent{Segments: []string{errMsg.Str}, Metadata: map[string]string{"error": "true"}}, nil
	}
	if msg := gjson.GetBytes(body, "message"); msg.Type == gjson.String && msg.Str != "" {
		return traffic.NormalizedContent{Segments: []string{msg.Str}, Metadata: map[string]string{"error": "true"}}, nil
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	if len(chunk) == 0 {
		return traffic.NormalizedContent{}, nil
	}
	if !looksLikeJSON(chunk) {
		return traffic.NormalizedContent{}, nil
	}
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, nil
	}

	var segments, toolCalls, reasoning []string

	delta := gjson.GetBytes(chunk, "choices.0.delta")
	if delta.IsObject() {
		if c := delta.Get("content"); c.Type == gjson.String && c.Str != "" {
			segments = append(segments, c.Str)
		}
		if rc := delta.Get("reasoning_content"); rc.Type == gjson.String && rc.Str != "" {
			reasoning = append(reasoning, rc.Str)
		}
		if tc := delta.Get("tool_calls"); tc.IsArray() {
			tc.ForEach(func(_, call gjson.Result) bool {
				toolCalls = append(toolCalls, call.Raw)
				return true
			})
		}
		return traffic.NormalizedContent{
			Segments:          segments,
			ReasoningSegments: reasoning,
			ToolCallSegments:  toolCalls,
		}, nil
	}
	if t := gjson.GetBytes(chunk, "text"); t.Type == gjson.String && t.Str != "" {
		segments = append(segments, t.Str)
	}
	if c := gjson.GetBytes(chunk, "content"); c.Type == gjson.String && c.Str != "" {
		segments = append(segments, c.Str)
	}
	return traffic.NormalizedContent{Segments: segments}, nil
}

func (a *Adapter) DetectRequestMeta(_ *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "deepseek-web"}
	if !gjson.ValidBytes(body) {
		return meta
	}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
		meta.Model = model.Str
	}
	return meta
}

func (a *Adapter) DetectResponseUsage(_ *http.Response, _ []byte) traffic.UsageMeta {
	return traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
}

func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}

func looksLikeJSON(b []byte) bool {
	for _, c := range b {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		return c == '{' || c == '['
	}
	return false
}
