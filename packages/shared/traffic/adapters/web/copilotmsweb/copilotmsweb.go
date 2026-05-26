// Package copilotmsweb implements the copilot-ms-web traffic adapter
// for browser-side traffic to copilot.microsoft.com (the consumer
// Microsoft Copilot product, descended from Bing Chat / Sydney).
//
// copilot.microsoft.com's wire format is **undocumented**. Historical
// reverse engineering shows three plausible JSON request shapes:
//
//  1. Modern Copilot shape:
//     {"messages":[{"author":"user","text":"<prompt>", "contentType":"Text"}],
//     "session_id":"...", "conversationId":"...", "model":"..."}
//
//  2. Legacy Bing/Sydney shape (envelope with `arguments` array):
//     {"arguments":[{"message":{"author":"user","text":"<prompt>"},
//     "optionsSets":[...], "isStartOfSession":true, ...}],
//     "invocationId":"...", "target":"chat", "type":4}
//
//  3. OpenAI-compatible shape (used by Microsoft Copilot REST API
//     deployments under copilot.microsoft.com/api):
//     {"messages":[{"role":"user","content":"<prompt>"}]}
//
// The adapter detects which shape is present and extracts text + tool
// invocations accordingly. Streaming responses are SSE-shaped or
// JSON-line chunked; per-chunk parsing follows the OpenAI-compatible
// delta shape when present and degrades to the legacy Sydney shape
// otherwise.
package copilotmsweb

import (
	"context"
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "copilot-ms-web"

// requestKnownKeys lists known top-level fields across the three
// shapes; anything else lands in Extra.
var requestKnownKeys = []string{
	// Modern shape:
	"messages", "session_id", "conversationId", "conversation_id",
	"model", "settings", "stream", "isStartOfSession",
	// Legacy Sydney shape:
	"arguments", "invocationId", "target", "type",
	// OpenAI-compat shape extras:
	"max_tokens", "temperature", "top_p", "tools", "tool_choice",
	"function_call", "functions", "stop", "user",
	// Common metadata:
	"locale", "market", "region", "client_metadata",
}

// Adapter implements copilot-ms-web extraction.
type Adapter struct{}

// ID returns the canonical adapter identifier.
func (a *Adapter) ID() string { return adapterID }

// Configure is a no-op — copilot-ms-web has no per-domain configuration.
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses a copilot.microsoft.com request body. Detects
// which of the three known shapes is present and routes accordingly.
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	// (1) + (3): top-level `messages` array.
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		return extractMessagesShape(body, msgs)
	}
	// (2): Legacy Sydney `arguments` envelope.
	if args := gjson.GetBytes(body, "arguments"); args.IsArray() {
		return extractLegacySydneyShape(body, args)
	}

	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// extractMessagesShape handles both the Modern Copilot shape (author /
// text) and the OpenAI-compatible shape (role / content). The two are
// distinguished by the field names on each entry.
func extractMessagesShape(body []byte, messages gjson.Result) (traffic.NormalizedContent, error) {
	var segments, toolCalls []string
	messages.ForEach(func(_, msg gjson.Result) bool {
		// OpenAI-compat: role + content.
		if content := msg.Get("content"); content.Exists() {
			if content.Type == gjson.String {
				segments = append(segments, content.Str)
			} else if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if part.Get("type").Str == "text" {
						segments = append(segments, part.Get("text").Str)
					}
					return true
				})
			}
		}
		// OpenAI-compat: tool_calls / function_call echoed in history.
		if tc := msg.Get("tool_calls"); tc.IsArray() {
			tc.ForEach(func(_, call gjson.Result) bool {
				toolCalls = append(toolCalls, call.Raw)
				return true
			})
		}
		if fc := msg.Get("function_call"); fc.IsObject() {
			toolCalls = append(toolCalls, fc.Raw)
		}
		// Modern Copilot: author + text.
		if text := msg.Get("text"); text.Type == gjson.String && text.Str != "" {
			segments = append(segments, text.Str)
		}
		return true
	})

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Exists() && model.Type == gjson.String {
		meta["model"] = model.Str
	}
	if conv := gjson.GetBytes(body, "conversationId"); conv.Type == gjson.String && conv.Str != "" {
		meta["conversationId"] = conv.Str
	}
	if conv := gjson.GetBytes(body, "conversation_id"); conv.Type == gjson.String && conv.Str != "" {
		meta["conversation_id"] = conv.Str
	}
	if sid := gjson.GetBytes(body, "session_id"); sid.Type == gjson.String && sid.Str != "" {
		meta["session_id"] = sid.Str
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		meta["tools"] = tools.Raw
	}

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
		Extra:            traffic.CollectExtra(body, requestKnownKeys),
	}, nil
}

// extractLegacySydneyShape handles the legacy Bing/Sydney request
// envelope: arguments[].message.{author,text} for the new prompt; the
// optionsSets and other fields land in Extra.
func extractLegacySydneyShape(body []byte, arguments gjson.Result) (traffic.NormalizedContent, error) {
	var segments []string
	arguments.ForEach(func(_, arg gjson.Result) bool {
		if text := arg.Get("message.text"); text.Type == gjson.String && text.Str != "" {
			segments = append(segments, text.Str)
		}
		// Sydney also nests prior turns in `previousMessages`.
		if prev := arg.Get("previousMessages"); prev.IsArray() {
			prev.ForEach(func(_, m gjson.Result) bool {
				if t := m.Get("text"); t.Type == gjson.String && t.Str != "" {
					segments = append(segments, t.Str)
				}
				return true
			})
		}
		return true
	})

	meta := map[string]string{}
	if iid := gjson.GetBytes(body, "invocationId"); iid.Type == gjson.String && iid.Str != "" {
		meta["invocationId"] = iid.Str
	}
	if target := gjson.GetBytes(body, "target"); target.Type == gjson.String && target.Str != "" {
		meta["target"] = target.Str
	}

	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
		Extra:    traffic.CollectExtra(body, requestKnownKeys),
	}, nil
}

// ExtractResponse handles a buffered (non-streaming) response. Mostly
// covers JSON error envelopes since successful responses stream.
func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, nil
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
	// OpenAI-compat happy-path JSON response (rare on copilot.microsoft.com
	// but covered for the REST API path).
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
		meta := map[string]string{}
		if m := gjson.GetBytes(body, "model"); m.Type == gjson.String {
			meta["model"] = m.Str
		}
		return traffic.NormalizedContent{
			Segments:         segments,
			ToolCallSegments: toolCalls,
			Metadata:         meta,
		}, nil
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// ExtractStreamChunk parses one chunk. Handles three plausible shapes:
//
//   - OpenAI-compat delta: {"choices":[{"delta":{"content":"<text>"}}]}
//   - Sydney-style update: {"type":1,"target":"update","arguments":[{"messages":[{"text":"<delta>"}]}]}
//   - Plain text chunk: passes through as Segments
//
// Fail-open: unrecognised shapes return empty content rather than errors.
func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	if len(chunk) == 0 {
		return traffic.NormalizedContent{}, nil
	}
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, nil
	}

	var segments, toolCalls []string

	// (1) OpenAI-compat delta.
	delta := gjson.GetBytes(chunk, "choices.0.delta")
	if delta.Exists() && delta.IsObject() {
		if c := delta.Get("content"); c.Type == gjson.String && c.Str != "" {
			segments = append(segments, c.Str)
		}
		if tc := delta.Get("tool_calls"); tc.IsArray() {
			tc.ForEach(func(_, call gjson.Result) bool {
				toolCalls = append(toolCalls, call.Raw)
				return true
			})
		}
		var meta map[string]string
		if fr := gjson.GetBytes(chunk, "choices.0.finish_reason"); fr.Type == gjson.String && fr.Str != "" {
			meta = map[string]string{"finish_reason": fr.Str}
		}
		return traffic.NormalizedContent{
			Segments:         segments,
			ToolCallSegments: toolCalls,
			Metadata:         meta,
		}, nil
	}

	// (2) Sydney-style update: type=1 with arguments array carrying
	// messages with delta text.
	if gjson.GetBytes(chunk, "type").Int() == 1 {
		gjson.GetBytes(chunk, "arguments").ForEach(func(_, arg gjson.Result) bool {
			arg.Get("messages").ForEach(func(_, m gjson.Result) bool {
				if t := m.Get("text"); t.Type == gjson.String && t.Str != "" {
					segments = append(segments, t.Str)
				}
				// Sydney's adaptiveCards / suggestedResponses are not
				// audit-relevant content; skip.
				return true
			})
			return true
		})
		return traffic.NormalizedContent{Segments: segments}, nil
	}

	// (3) Plain text passthrough — some Copilot streams emit raw text
	// chunks; check for a top-level text field.
	if t := gjson.GetBytes(chunk, "text"); t.Type == gjson.String && t.Str != "" {
		return traffic.NormalizedContent{Segments: []string{t.Str}}, nil
	}

	return traffic.NormalizedContent{}, nil
}

// DetectRequestMeta returns Provider="copilot-ms-web". Authentication
// uses cookies (Microsoft account session), not Bearer tokens.
func (a *Adapter) DetectRequestMeta(_ *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "copilot-ms-web"}
	if !gjson.ValidBytes(body) {
		return meta
	}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
		meta.Model = model.Str
	}
	return meta
}

// DetectResponseUsage returns the non-LLM-tier sentinel.
func (a *Adapter) DetectResponseUsage(_ *http.Response, _ []byte) traffic.UsageMeta {
	return traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
}

// RewriteRequestBody is unsupported. Copilot bodies carry signed
// session tokens we cannot regenerate.
func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}

// RewriteResponseBody is unsupported.
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
