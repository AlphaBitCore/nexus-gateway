// Package perplexityweb implements the perplexity-web traffic adapter
// for browser-side traffic to www.perplexity.ai and perplexity.ai.
// Distinct from the public Perplexity Sonar API on api.perplexity.ai
// (handled by openai-compat).
//
// Perplexity's web wire format is undocumented; the adapter follows
// the defensive pattern established by cursor / codeium / grok-web:
// JSON-aware content extraction.
package perplexityweb

import (
	"context"
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "perplexity-web"

// Adapter implements perplexity-web extraction.
type Adapter struct{}

// ID returns the canonical adapter identifier.
func (a *Adapter) ID() string { return adapterID }

// Configure is a no-op.
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses a www.perplexity.ai request body.
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
			return true
		})
	}

	for _, key := range []string{"query", "prompt", "text", "input"} {
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
	if focus := gjson.GetBytes(body, "search_focus"); focus.Type == gjson.String && focus.Str != "" {
		meta["search_focus"] = focus.Str
	}

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
	}, nil
}

// ExtractResponse handles JSON error envelopes.
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
	if detail := gjson.GetBytes(body, "detail"); detail.Type == gjson.String && detail.Str != "" {
		return traffic.NormalizedContent{Segments: []string{detail.Str}, Metadata: map[string]string{"error": "true"}}, nil
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// ExtractStreamChunk parses one streaming chunk. Perplexity streams
// search results + answer chunks; common shapes include:
//   - {"chunks":[{"text":"..."}], "answer":"..."}
//   - OpenAI-compat {"choices":[{"delta":{"content":"..."}}]}
//   - {"text":"..."} plain text
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

	if a := gjson.GetBytes(chunk, "answer"); a.Type == gjson.String && a.Str != "" {
		segments = append(segments, a.Str)
	}
	if t := gjson.GetBytes(chunk, "text"); t.Type == gjson.String && t.Str != "" {
		segments = append(segments, t.Str)
	}
	if c := gjson.GetBytes(chunk, "content"); c.Type == gjson.String && c.Str != "" {
		segments = append(segments, c.Str)
	}
	gjson.GetBytes(chunk, "chunks").ForEach(func(_, ch gjson.Result) bool {
		if t := ch.Get("text"); t.Type == gjson.String && t.Str != "" {
			segments = append(segments, t.Str)
		}
		return true
	})
	return traffic.NormalizedContent{Segments: segments}, nil
}

// DetectRequestMeta sets Provider="perplexity-web". Auth via session
// cookies / session_token cookie; ApiKey* fields stay empty.
func (a *Adapter) DetectRequestMeta(_ *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "perplexity-web"}
	if !gjson.ValidBytes(body) {
		return meta
	}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
		meta.Model = model.Str
	}
	return meta
}

// DetectResponseUsage returns Status=non_llm.
func (a *Adapter) DetectResponseUsage(_ *http.Response, _ []byte) traffic.UsageMeta {
	return traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
}

// RewriteRequestBody is unsupported.
func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}

// RewriteResponseBody is unsupported.
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
