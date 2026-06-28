// Package cohere implements the cohere traffic adapter for Cohere's
// Chat API v2 at api.cohere.com. Replaces the generic-jsonpath
// placeholder used in the original seed.
//
// Cohere Chat v2 wire format reference:
//   - Request body: {model, messages: [{role, content}], stream,
//     tools, temperature, ...}
//   - Response (non-streaming): {id, message: {role, content:
//     [{type:"text", text:"..."}], tool_plan, tool_calls},
//     finish_reason, usage}
//   - Streaming events (SSE):
//     content-start / content-delta / content-end       — assistant text
//     tool-plan-delta                                   — reasoning trace
//     tool-call-start / tool-call-delta / tool-call-end — tool invocations
//     message-start / message-end                       — frame markers
package cohere

import (
	"context"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "cohere"

type Adapter struct{}

func (a *Adapter) ID() string                       { return adapterID }
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses a Cohere Chat v2 request body.
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments, toolCalls []string
	messages.ForEach(func(_, msg gjson.Result) bool {
		// Cohere supports both string content and array-of-blocks content.
		if c := msg.Get("content"); c.Type == gjson.String {
			segments = append(segments, c.Str)
		} else if c := msg.Get("content"); c.IsArray() {
			c.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").Str == "text" {
					segments = append(segments, part.Get("text").Str)
				}
				return true
			})
		}
		// Assistant tool_calls echoed in conversation history.
		if tc := msg.Get("tool_calls"); tc.IsArray() {
			tc.ForEach(func(_, call gjson.Result) bool {
				toolCalls = append(toolCalls, call.Raw)
				return true
			})
		}
		// Tool result content (role=tool).
		if msg.Get("role").Str == "tool" {
			if results := msg.Get("tool_results"); results.IsArray() {
				results.ForEach(func(_, r gjson.Result) bool {
					if doc := r.Get("document.data"); doc.Type == gjson.String && doc.Str != "" {
						segments = append(segments, doc.Str)
					}
					if t := r.Get("text"); t.Type == gjson.String && t.Str != "" {
						segments = append(segments, t.Str)
					}
					return true
				})
			}
		}
		return true
	})

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
		meta["model"] = model.Str
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		meta["tools"] = tools.Raw
	}
	if docs := gjson.GetBytes(body, "documents"); docs.IsArray() {
		meta["documents"] = docs.Raw
	}

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
	}, nil
}

// ExtractResponse parses a Cohere Chat v2 non-streaming response.
//
//   - message.content[type=text].text → Segments
//   - message.tool_calls              → ToolCallSegments
//   - message.tool_plan (reasoning trace) → ReasoningSegments
//   - finish_reason                   → Metadata
func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	if !gjson.GetBytes(body, "message").Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments, reasoning, toolCalls []string

	gjson.GetBytes(body, "message.content").ForEach(func(_, part gjson.Result) bool {
		if part.Get("type").Str == "text" {
			segments = append(segments, part.Get("text").Str)
		}
		return true
	})
	if plan := gjson.GetBytes(body, "message.tool_plan"); plan.Type == gjson.String && plan.Str != "" {
		reasoning = append(reasoning, plan.Str)
	}
	gjson.GetBytes(body, "message.tool_calls").ForEach(func(_, call gjson.Result) bool {
		toolCalls = append(toolCalls, call.Raw)
		return true
	})

	meta := map[string]string{}
	if id := gjson.GetBytes(body, "id"); id.Type == gjson.String && id.Str != "" {
		meta["id"] = id.Str
	}
	if fr := gjson.GetBytes(body, "finish_reason"); fr.Type == gjson.String && fr.Str != "" {
		meta["finish_reason"] = fr.Str
	}

	return traffic.NormalizedContent{
		Segments:          segments,
		ReasoningSegments: reasoning,
		ToolCallSegments:  toolCalls,
		Metadata:          meta,
	}, nil
}

// ExtractStreamChunk parses one Cohere Chat v2 SSE event payload.
//
// Event types:
//   - content-delta: delta.message.content.text → Segments
//   - tool-plan-delta: delta.message.tool_plan → ReasoningSegments
//   - tool-call-start: delta.message.tool_calls.[].function → ToolCallSegments (initial)
//   - tool-call-delta: delta.message.tool_calls.[].function.arguments → ToolCallSegments (delta)
//   - message-end: delta.finish_reason → Metadata
//
// Other events (content-start/end, tool-call-end, message-start, citations)
// carry no content and return empty.
func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	var segments, reasoning, toolCalls []string
	meta := map[string]string{}

	switch gjson.GetBytes(chunk, "type").Str {
	case "content-delta":
		if t := gjson.GetBytes(chunk, "delta.message.content.text"); t.Type == gjson.String && t.Str != "" {
			segments = append(segments, t.Str)
		}
	case "tool-plan-delta":
		if t := gjson.GetBytes(chunk, "delta.message.tool_plan"); t.Type == gjson.String && t.Str != "" {
			reasoning = append(reasoning, t.Str)
		}
	case "tool-call-start":
		gjson.GetBytes(chunk, "delta.message.tool_calls").ForEach(func(_, call gjson.Result) bool {
			toolCalls = append(toolCalls, call.Raw)
			return true
		})
	case "tool-call-delta":
		// Argument fragment streaming. The pipeline accumulates across chunks.
		if d := gjson.GetBytes(chunk, "delta.message.tool_calls"); d.IsArray() {
			d.ForEach(func(_, call gjson.Result) bool {
				toolCalls = append(toolCalls, call.Raw)
				return true
			})
		}
	case "message-end":
		if fr := gjson.GetBytes(chunk, "delta.finish_reason"); fr.Type == gjson.String && fr.Str != "" {
			meta["finish_reason"] = fr.Str
		}
	}

	var outMeta map[string]string
	if len(meta) > 0 {
		outMeta = meta
	}
	return traffic.NormalizedContent{
		Segments:          segments,
		ReasoningSegments: reasoning,
		ToolCallSegments:  toolCalls,
		Metadata:          outMeta,
	}, nil
}

func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "cohere"}
	if r != nil {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			tok := strings.TrimSpace(auth[len("Bearer "):])
			if tok != "" {
				meta.ApiKeyClass = "cohere-bearer"
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

// DetectResponseUsage extracts Cohere's usage block.
//
//	{"usage":{"billed_units":{"input_tokens":N,"output_tokens":N},
//	          "tokens":{"input_tokens":N,"output_tokens":N}}}
//
// We use the `tokens` totals (true counts) over `billed_units`
// (which can differ when caching is involved).
func (a *Adapter) DetectResponseUsage(_ *http.Response, body []byte) traffic.UsageMeta {
	if len(body) == 0 {
		return traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
	}
	if !gjson.ValidBytes(body) {
		return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
	}
	usage := traffic.UsageMeta{Status: traffic.UsageStatusOK}
	if pt := gjson.GetBytes(body, "usage.tokens.input_tokens"); pt.Exists() && pt.Type == gjson.Number {
		v := int(pt.Int())
		usage.PromptTokens = &v
	}
	if ct := gjson.GetBytes(body, "usage.tokens.output_tokens"); ct.Exists() && ct.Type == gjson.Number {
		v := int(ct.Int())
		usage.CompletionTokens = &v
	}
	if usage.PromptTokens == nil && usage.CompletionTokens == nil {
		usage.Status = traffic.UsageStatusParseFailed
	}
	return usage
}

func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
