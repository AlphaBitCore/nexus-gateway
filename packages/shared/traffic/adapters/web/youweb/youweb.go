// Package youweb implements the you-web traffic adapter for browser
// traffic to you.com and chat.you.com (You.com search + AI chat).
// Defensive JSON-aware adapter.
package youweb

import (
	"context"
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "you-web"

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
	var segments []string
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		msgs.ForEach(func(_, msg gjson.Result) bool {
			if c := msg.Get("content"); c.Type == gjson.String {
				segments = append(segments, c.Str)
			}
			return true
		})
	}
	for _, key := range []string{"prompt", "query", "q", "text", "input"} {
		if v := gjson.GetBytes(body, key); v.Type == gjson.String && v.Str != "" {
			segments = append(segments, v.Str)
		}
	}
	if len(segments) == 0 {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String && model.Str != "" {
		meta["model"] = model.Str
	}
	if mode := gjson.GetBytes(body, "chatMode"); mode.Type == gjson.String && mode.Str != "" {
		meta["chatMode"] = mode.Str
	}
	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
	}, nil
}

func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, nil
	}
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	if errMsg := gjson.GetBytes(body, "error.message"); errMsg.Type == gjson.String && errMsg.Str != "" {
		return traffic.NormalizedContent{Segments: []string{errMsg.Str}, Metadata: map[string]string{"error": "true"}}, nil
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	if len(chunk) == 0 || !looksLikeJSON(chunk) || !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, nil
	}
	var segments []string
	for _, key := range []string{"text", "content", "answer"} {
		if v := gjson.GetBytes(chunk, key); v.Type == gjson.String && v.Str != "" {
			segments = append(segments, v.Str)
		}
	}
	if c := gjson.GetBytes(chunk, "youChatToken"); c.Type == gjson.String && c.Str != "" {
		// You.com SSE token field for the chat answer stream.
		segments = append(segments, c.Str)
	}
	return traffic.NormalizedContent{Segments: segments}, nil
}

func (a *Adapter) DetectRequestMeta(_ *http.Request, _ []byte) traffic.RequestMeta {
	return traffic.RequestMeta{Provider: "you-web"}
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
