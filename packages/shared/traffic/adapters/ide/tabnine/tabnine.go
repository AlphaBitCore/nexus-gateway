// Package tabnine implements the tabnine traffic adapter for IDE
// traffic to api.tabnine.com (Tabnine's cloud-hosted code completion +
// chat backend). Self-hosted Tabnine deployments use customer-
// configured hosts and are out of scope for this adapter — admins can
// add an InterceptionDomain row pointing to the same adapter ID for
// each self-hosted host they want covered.
//
// Tabnine's wire format is undocumented; the adapter follows the
// defensive pattern established by cursor / codeium / grok-web.
package tabnine

import (
	"context"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "tabnine"

// Adapter implements tabnine extraction.
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
			if t := msg.Get("text"); t.Type == gjson.String && t.Str != "" {
				segments = append(segments, t.Str)
			}
			return true
		})
	}

	for _, key := range []string{"prompt", "query", "text", "input"} {
		if v := gjson.GetBytes(body, key); v.Type == gjson.String && v.Str != "" {
			segments = append(segments, v.Str)
		}
	}

	if len(segments) == 0 && len(toolCalls) == 0 {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String && model.Str != "" {
		meta["model"] = model.Str
	}
	if conv := gjson.GetBytes(body, "conversation_id"); conv.Type == gjson.String && conv.Str != "" {
		meta["conversation_id"] = conv.Str
	}

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
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
	if choices := gjson.GetBytes(body, "choices"); choices.IsArray() {
		var segments, toolCalls []string
		choices.ForEach(func(_, choice gjson.Result) bool {
			if c := choice.Get("message.content"); c.Type == gjson.String {
				segments = append(segments, c.Str)
			}
			if tc := choice.Get("message.tool_calls"); tc.IsArray() {
				tc.ForEach(func(_, call gjson.Result) bool {
					toolCalls = append(toolCalls, call.Raw)
					return true
				})
			}
			return true
		})
		return traffic.NormalizedContent{Segments: segments, ToolCallSegments: toolCalls}, nil
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

	var segments, toolCalls []string

	delta := gjson.GetBytes(chunk, "choices.0.delta")
	if delta.IsObject() {
		if c := delta.Get("content"); c.Type == gjson.String && c.Str != "" {
			segments = append(segments, c.Str)
		}
		if tc := delta.Get("tool_calls"); tc.IsArray() {
			tc.ForEach(func(_, call gjson.Result) bool {
				toolCalls = append(toolCalls, call.Raw)
				return true
			})
		}
		return traffic.NormalizedContent{Segments: segments, ToolCallSegments: toolCalls}, nil
	}
	if t := gjson.GetBytes(chunk, "text"); t.Type == gjson.String && t.Str != "" {
		segments = append(segments, t.Str)
	}
	if c := gjson.GetBytes(chunk, "content"); c.Type == gjson.String && c.Str != "" {
		segments = append(segments, c.Str)
	}
	return traffic.NormalizedContent{Segments: segments}, nil
}

func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "tabnine"}
	if r != nil {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			tok := strings.TrimSpace(auth[len("Bearer "):])
			if tok != "" {
				meta.ApiKeyClass = "tabnine-bearer"
				meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(tok)
			}
		}
	}
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
