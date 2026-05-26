// Package huggingchatweb implements the huggingchat-web traffic
// adapter for browser traffic to huggingface.co/chat (HuggingChat —
// Hugging Face's free consumer chat product).
//
// The host huggingface.co serves both HuggingChat (under /chat/...)
// and many non-AI routes (model browsing, dataset uploads, repo
// management, etc.). The corresponding InterceptionDomain row uses
// `defaultPathAction: PASSTHROUGH` and a path-level rule scoped to
// `/chat/*` activates this adapter only for chat traffic — non-chat
// traffic flows through unchanged.
package huggingchatweb

import (
	"bytes"
	"context"
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "huggingchat-web"

var requestKnownKeys = []string{
	"messages", "prompt", "query", "text", "input", "inputs", "model",
	"stream", "session_id", "conversation_id", "id", "is_retry",
	"web_search", "tools", "files",
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
			if c := msg.Get("content"); c.Type == gjson.String {
				segments = append(segments, c.Str)
			}
			return true
		})
	}
	for _, key := range []string{"prompt", "query", "text", "input", "inputs"} {
		if v := gjson.GetBytes(body, key); v.Type == gjson.String && v.Str != "" {
			segments = append(segments, v.Str)
		}
	}
	if len(segments) == 0 {
		return traffic.NormalizedContent{Extra: traffic.CollectExtra(body, requestKnownKeys)}, traffic.ErrUnknownSchema
	}
	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String && model.Str != "" {
		meta["model"] = model.Str
	}
	if conv := gjson.GetBytes(body, "id"); conv.Type == gjson.String && conv.Str != "" {
		meta["id"] = conv.Str
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
	if errMsg := gjson.GetBytes(body, "message"); errMsg.Type == gjson.String && errMsg.Str != "" {
		return traffic.NormalizedContent{Segments: []string{errMsg.Str}, Metadata: map[string]string{"error": "true"}}, nil
	}
	if errMsg := gjson.GetBytes(body, "error"); errMsg.Type == gjson.String && errMsg.Str != "" {
		return traffic.NormalizedContent{Segments: []string{errMsg.Str}, Metadata: map[string]string{"error": "true"}}, nil
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	if len(chunk) == 0 || !looksLikeJSON(chunk) || !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, nil
	}
	var segments []string
	// HuggingChat streams JSON lines with various event types.
	// `stream` event carries token text; `finalAnswer` carries the
	// completed response.
	if t := gjson.GetBytes(chunk, "type"); t.Type == gjson.String {
		switch t.Str {
		case "stream":
			if tok := gjson.GetBytes(chunk, "token"); tok.Type == gjson.String && tok.Str != "" {
				segments = append(segments, tok.Str)
			}
		case "finalAnswer":
			if t := gjson.GetBytes(chunk, "text"); t.Type == gjson.String && t.Str != "" {
				segments = append(segments, t.Str)
			}
		}
		return traffic.NormalizedContent{Segments: segments}, nil
	}
	// Fallback: scan known fields.
	for _, key := range []string{"text", "content", "token"} {
		if v := gjson.GetBytes(chunk, key); v.Type == gjson.String && v.Str != "" {
			segments = append(segments, v.Str)
		}
	}
	return traffic.NormalizedContent{Segments: segments}, nil
}

func (a *Adapter) DetectRequestMeta(_ *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "huggingchat-web"}
	if gjson.ValidBytes(body) {
		if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
			meta.Model = model.Str
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
