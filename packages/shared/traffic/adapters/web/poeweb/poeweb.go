// Package poeweb implements the poe-web traffic adapter for browser
// traffic to Quora's multi-model aggregator at poe.com.
//
// Poe's wire format is undocumented and shifts between bot families
// (OpenAI, Anthropic, Google, etc.). The adapter is defensive: it
// pulls common segment-bearing fields out of arbitrary JSON bodies
// and falls back to a binary preview when the body is non-JSON.
package poeweb

import (
	"bytes"
	"context"
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "poe-web"

var requestKnownKeys = []string{
	"messages", "query", "queries", "text", "prompt", "input",
	"chatId", "chat_id", "messageId", "message_id", "bot",
	"botName", "modelName", "stream", "tools",
}

type Adapter struct{}

func (a *Adapter) ID() string                       { return adapterID }
func (a *Adapter) Configure(_ map[string]any) error { return nil }

func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	if !looksLikeJSON(body) {
		return traffic.NormalizedContent{Extra: map[string]string{"binary_preview": preview(body)}}, traffic.ErrUnknownSchema
	}
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	var segments []string
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		msgs.ForEach(func(_, msg gjson.Result) bool {
			if c := msg.Get("content"); c.Type == gjson.String && c.Str != "" {
				segments = append(segments, c.Str)
			}
			return true
		})
	}
	if qs := gjson.GetBytes(body, "queries"); qs.IsArray() {
		qs.ForEach(func(_, q gjson.Result) bool {
			if c := q.Get("content"); c.Type == gjson.String && c.Str != "" {
				segments = append(segments, c.Str)
			}
			return true
		})
	}
	for _, key := range []string{"query", "text", "prompt", "input"} {
		if v := gjson.GetBytes(body, key); v.Type == gjson.String && v.Str != "" {
			segments = append(segments, v.Str)
		}
	}
	if len(segments) == 0 {
		return traffic.NormalizedContent{Extra: traffic.CollectExtra(body, requestKnownKeys)}, traffic.ErrUnknownSchema
	}
	meta := map[string]string{}
	for _, k := range []string{"bot", "botName", "modelName"} {
		if v := gjson.GetBytes(body, k); v.Type == gjson.String && v.Str != "" {
			meta["bot"] = v.Str
			break
		}
	}
	for _, k := range []string{"chatId", "chat_id"} {
		if v := gjson.GetBytes(body, k); v.Type == gjson.String && v.Str != "" {
			meta["chat_id"] = v.Str
			break
		}
	}
	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
		Extra:    traffic.CollectExtra(body, requestKnownKeys),
	}, nil
}

func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, nil
	}
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	for _, k := range []string{"text", "content", "message", "error"} {
		if v := gjson.GetBytes(body, k); v.Type == gjson.String && v.Str != "" {
			return traffic.NormalizedContent{Segments: []string{v.Str}}, nil
		}
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	if len(chunk) == 0 || !looksLikeJSON(chunk) || !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, nil
	}
	var segments []string
	for _, key := range []string{"text", "content", "delta", "token"} {
		if v := gjson.GetBytes(chunk, key); v.Type == gjson.String && v.Str != "" {
			segments = append(segments, v.Str)
		}
	}
	return traffic.NormalizedContent{Segments: segments}, nil
}

func (a *Adapter) DetectRequestMeta(_ *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "poe-web"}
	if gjson.ValidBytes(body) {
		for _, k := range []string{"bot", "botName", "modelName"} {
			if v := gjson.GetBytes(body, k); v.Type == gjson.String && v.Str != "" {
				meta.Model = v.Str
				break
			}
		}
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

func preview(body []byte) string {
	if len(body) > 256 {
		body = body[:256]
	}
	clean := bytes.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return '.'
		}
		if r > 0x7e {
			return '.'
		}
		return r
	}, body)
	return string(clean)
}
