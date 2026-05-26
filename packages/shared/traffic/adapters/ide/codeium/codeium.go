// Package codeium implements the codeium traffic adapter for Codeium
// / Windsurf IDE backend traffic to server.codeium.com,
// inference.codeium.com, and api.codeium.com.
//
// Codeium's wire format is **heavily proprietary**: most endpoints
// use Connect-RPC with protobuf payloads (LangServerService,
// WindsurfService). Without runtime fixtures the adapter cannot
// generally parse protobuf, so it is defensive in the same shape as
// the cursor adapter:
//
//  1. JSON request bodies with chat-like fields (`messages`,
//     `prompt`, `query`, `text`) extract content normally; OpenAI-
//     compat messages with tool_calls flow through to ToolCallSegments.
//  2. Binary protobuf bodies surface ErrUnknownSchema with a
//     truncated ASCII-safe preview captured in
//     Extra["binary_preview"] for offline analysis.
//  3. Streaming chunks parse as JSON when possible; binary chunks
//     fail-open with empty content.
//
package codeium

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "codeium"

var requestKnownKeys = []string{
	"messages", "prompt", "query", "text", "input", "model",
	"stream", "session_id", "conversation_id", "request_id",
	"workspace_root", "context", "metadata", "service_key",
}

// Adapter implements codeium extraction.
type Adapter struct{}

// ID returns the canonical adapter identifier.
func (a *Adapter) ID() string { return adapterID }

// Configure is a no-op.
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses a Codeium request body.
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	if !looksLikeJSON(body) {
		return traffic.NormalizedContent{
			Extra: map[string]string{"binary_preview": preview(body)},
		}, traffic.ErrUnknownSchema
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
		return traffic.NormalizedContent{
			Extra: traffic.CollectExtra(body, requestKnownKeys),
		}, traffic.ErrUnknownSchema
	}

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String && model.Str != "" {
		meta["model"] = model.Str
	}
	if conv := gjson.GetBytes(body, "conversation_id"); conv.Type == gjson.String && conv.Str != "" {
		meta["conversation_id"] = conv.Str
	}
	if sid := gjson.GetBytes(body, "session_id"); sid.Type == gjson.String && sid.Str != "" {
		meta["session_id"] = sid.Str
	}

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
		Extra:            traffic.CollectExtra(body, requestKnownKeys),
	}, nil
}

// ExtractResponse handles JSON responses (mostly error envelopes;
// successful responses are streamed).
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
		return traffic.NormalizedContent{
			Segments: []string{errMsg.Str},
			Metadata: map[string]string{"error": "true"},
		}, nil
	}
	if msg := gjson.GetBytes(body, "message"); msg.Type == gjson.String && msg.Str != "" {
		return traffic.NormalizedContent{
			Segments: []string{msg.Str},
			Metadata: map[string]string{"error": "true"},
		}, nil
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

// ExtractStreamChunk handles streaming chunks. JSON chunks parse as
// OpenAI-compat / plain text; binary chunks fail-open.
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

// DetectRequestMeta sets Provider="codeium". Codeium IDE clients
// authenticate with a service key carried as a Bearer token (or in
// the request body for the gRPC-style endpoints).
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "codeium"}
	if r != nil {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			tok := strings.TrimSpace(auth[len("Bearer "):])
			if tok != "" {
				meta.ApiKeyClass = "codeium-bearer"
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
