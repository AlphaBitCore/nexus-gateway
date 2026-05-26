// Package chatgptweb implements the chatgpt-web traffic adapter for
// browser-side traffic to the chatgpt.com consumer client. This is
// distinct from the public OpenAI API (handled by the openai-compat
// adapter): chatgpt.com posts to /backend-api/f/conversation and
// streams JSON-Patch-style deltas inside SSE frames, plus a handful of
// routing/telemetry frames that should be filtered out of compliance
// audit content.
//
// Wire-format reference comes from packages/shared/transport/streaming/extract/
// chatgpt_web.go which captured the protocol shape against a real 2026
// session. The streaming extractor lives at the streaming layer (one
// frame at a time during accumulation); this traffic adapter implements
// the full Adapter interface so InterceptionDomain rows pointing at
// chatgpt.com can drive the audit + hook pipeline.
//
// Capabilities:
//   - ExtractRequest: pulls user prompt text from messages.[].content
//     parts (current shape) or content.text (older shape).
//   - ExtractStreamChunk: applies one JSON-Patch frame (or one
//     {o:"patch", v:[...]} batch); content/parts text → Segments;
//     object-typed parts whose `type` is "tool_use" or "tool_call" →
//     ToolCallSegments (best-effort — chatgpt.com tool emission is
//     undocumented and may evolve).
//   - ExtractResponse: chatgpt.com always streams; this path only
//     handles buffered error responses.
//   - DetectRequestMeta: minimal — chatgpt.com authenticates via
//     session cookies, not API keys, so ApiKeyFingerprint stays empty.
//   - DetectResponseUsage: chatgpt.com does not return token usage
//     (consumer product, not metered API). Returns Status=non_llm sentinel.
//   - Rewrite{Request,Response}Body: not supported. The chatgpt.com
//     client has tight integrity expectations on the request body
//     shape and the JSON-Patch SSE response is not safely
//     reconstructable from NormalizedContent.
package chatgptweb

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// adapterID is the canonical adapter identifier. Must match
// InterceptionDomain.adapter_id rows seeded for chatgpt.com.
const adapterID = "chatgpt-web"

// requestKnownKeys lists the chatgpt.com /backend-api/f/conversation
// request fields the adapter recognises. Anything else lands in
// NormalizedContent.Extra so a future protocol revision (a brand-new
// field carrying user data) reaches compliance hooks instead of being
// silently dropped.
var requestKnownKeys = []string{
	"messages", "model", "model_slug", "action", "conversation_id",
	"parent_message_id", "timezone", "history_and_training_disabled",
	"conversation_mode", "force_paragen", "force_rate_limit",
	"force_use_sse", "force_nulligen", "system_hints",
	"supported_encodings", "client_contextual_info",
	"reset_rate_limits", "websocket_request_id", "supports_buffering",
	"variant_purpose",
}

// Adapter implements the chatgpt-web extraction.
type Adapter struct{}

// ID returns the canonical adapter identifier.
func (a *Adapter) ID() string { return adapterID }

// Configure is a no-op — chatgpt-web has no per-domain configuration.
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses the chatgpt.com /backend-api/f/conversation
// request body. Returns ErrUnknownSchema when no `messages` field is
// present (e.g. account-info or moderation endpoints which we do not
// audit at the chatgpt-web layer).
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments []string
	messages.ForEach(func(_, msg gjson.Result) bool {
		// Newer shape: content.parts[] of strings.
		msg.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
			if t := part.String(); t != "" {
				segments = append(segments, t)
			}
			return true
		})
		// Older shape: content.text (single string).
		if t := msg.Get("content.text").String(); t != "" {
			segments = append(segments, t)
		}
		return true
	})

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Exists() && model.Type == gjson.String {
		meta["model"] = model.Str
	}
	if slug := gjson.GetBytes(body, "model_slug"); slug.Exists() && slug.Type == gjson.String {
		meta["model_slug"] = slug.Str
	}
	if conv := gjson.GetBytes(body, "conversation_id"); conv.Exists() && conv.Type == gjson.String {
		meta["conversation_id"] = conv.Str
	}
	if action := gjson.GetBytes(body, "action"); action.Exists() && action.Type == gjson.String {
		meta["action"] = action.Str
	}

	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
		Extra:    traffic.CollectExtra(body, requestKnownKeys),
	}, nil
}

// ExtractResponse handles a buffered (non-streaming) response. ChatGPT.com
// always streams successful responses, so this path mostly covers error
// shapes:
//
//	{"detail":"<text>"}
//	{"detail":{"message":"<text>", "code":"<...>"}}
//	{"error":{"message":"<text>", ...}}
//
// Successful streamed bodies should be fed via ExtractStreamChunk; if a
// caller buffers the SSE response into a single body, this path returns
// ErrUnknownSchema rather than trying to re-parse the SSE wrapper.
func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, nil
	}
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	// Plain string detail.
	if detail := gjson.GetBytes(body, "detail"); detail.Type == gjson.String && detail.Str != "" {
		return traffic.NormalizedContent{
			Segments: []string{detail.Str},
			Metadata: map[string]string{"error": "true"},
		}, nil
	}
	// Object detail with message.
	if detailMsg := gjson.GetBytes(body, "detail.message"); detailMsg.Type == gjson.String && detailMsg.Str != "" {
		return traffic.NormalizedContent{
			Segments: []string{detailMsg.Str},
			Metadata: map[string]string{"error": "true"},
		}, nil
	}
	// OpenAI-style error envelope.
	if errMsg := gjson.GetBytes(body, "error.message"); errMsg.Type == gjson.String && errMsg.Str != "" {
		return traffic.NormalizedContent{
			Segments: []string{errMsg.Str},
			Metadata: map[string]string{"error": "true"},
		}, nil
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// ExtractStreamChunk parses one SSE frame from chatgpt.com. The
// streaming pipeline strips the `data: ` prefix and the `event:` line
// before calling this. Frame disposition follows
// streaming/extract/chatgpt_web.go:
//
//	"v1" / [DONE]                          → no content (markers)
//	{"type":"resume_conversation_token"}    → skip (routing JWT)
//	{"type":"message_marker"}               → skip (telemetry)
//	{"type":"server_ste_metadata"}          → skip
//	{"type":"conversation_detail_metadata"} → skip
//	{"type":"message_stream_complete"}      → skip
//	{"type":"input_message", ...}           → user prompt echo → Segments
//	{"o":"append","p":"/...","v":"<txt>"}   → assistant delta → Segments
//	{"o":"replace","p":"/...","v":"<txt>"}  → assistant delta → Segments
//	{"o":"add","p":"","v":{message:{...}}}  → initial assistant frame
//	{"o":"patch","v":[<sub-op>, ...]}       → list of sub-ops
//
// Object-typed `v` whose `type` is "tool_use" or "tool_call" lands on
// ToolCallSegments. The chatgpt.com tool-emission shape is undocumented
// and may not always match these names — best-effort capture, no parse
// errors propagate.
func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	chunk = bytes.TrimSpace(chunk)
	if len(chunk) == 0 {
		return traffic.NormalizedContent{}, nil
	}
	// String markers like `"v1"` or sentinel `[DONE]`.
	if bytes.Equal(chunk, []byte("[DONE]")) || chunk[0] == '"' {
		return traffic.NormalizedContent{}, nil
	}
	if !gjson.ValidBytes(chunk) {
		// Unexpected non-JSON frame: do not error, just skip — the
		// proxy fail-open posture for unknown wire shapes.
		return traffic.NormalizedContent{}, nil
	}

	// Filtered-out telemetry / routing frame types.
	switch gjson.GetBytes(chunk, "type").String() {
	case "resume_conversation_token", "message_marker",
		"server_ste_metadata", "conversation_detail_metadata",
		"message_stream_complete", "moderation":
		return traffic.NormalizedContent{}, nil
	case "input_message":
		var segments []string
		gjson.GetBytes(chunk, "input_message.content.parts").ForEach(func(_, p gjson.Result) bool {
			if t := p.String(); t != "" {
				segments = append(segments, t)
			}
			return true
		})
		return traffic.NormalizedContent{Segments: segments}, nil
	}

	// JSON-Patch shapes.
	op := gjson.GetBytes(chunk, "o").String()
	if op == "" {
		return traffic.NormalizedContent{}, nil
	}

	var segments, toolCalls []string
	if op == "patch" {
		gjson.GetBytes(chunk, "v").ForEach(func(_, sub gjson.Result) bool {
			collectPatch(sub.Get("p").String(), sub.Get("o").String(), sub.Get("v"), &segments, &toolCalls)
			return true
		})
	} else {
		path := gjson.GetBytes(chunk, "p").String()
		val := gjson.GetBytes(chunk, "v")
		collectPatch(path, op, val, &segments, &toolCalls)
	}

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
	}, nil
}

// collectPatch applies one (path, op, value) tuple. content/parts
// string values feed Segments; object values whose `type` is a known
// tool variant feed ToolCallSegments. Non-content paths and unknown ops
// are silently skipped — the chatgpt-web protocol carries many
// telemetry-only patches we do not audit.
func collectPatch(path, op string, val gjson.Result, segments, toolCalls *[]string) {
	switch op {
	case "append", "replace":
		if !isContentPartPath(path) {
			return
		}
		if val.Type == gjson.String {
			if val.Str != "" {
				*segments = append(*segments, val.Str)
			}
			return
		}
		if val.IsObject() {
			// Object-shaped content part — chatgpt.com occasionally
			// embeds tool_use / tool_call structures here. Best-effort
			// capture: any object whose `type` field hints at a tool
			// invocation goes to ToolCallSegments, otherwise the raw
			// text fallback (if any) goes to Segments.
			t := val.Get("type").Str
			if t == "tool_use" || t == "tool_call" || t == "tool_invocation" {
				*toolCalls = append(*toolCalls, val.Raw)
				return
			}
			if textVal := val.Get("text"); textVal.Type == gjson.String && textVal.Str != "" {
				*segments = append(*segments, textVal.Str)
			}
		}
	case "add":
		// Initial assistant message frame: value carries the seed
		// message object including content.parts (sometimes empty,
		// sometimes pre-populated).
		if path == "" && val.IsObject() {
			val.Get("message.content.parts").ForEach(func(_, p gjson.Result) bool {
				if t := p.String(); t != "" {
					*segments = append(*segments, t)
				}
				return true
			})
		}
	}
}

func isContentPartPath(p string) bool {
	return strings.HasPrefix(p, "/message/content/parts/")
}

// DetectRequestMeta returns the chatgpt-web RequestMeta. ChatGPT.com
// authenticates browser sessions via cookies, not Bearer tokens, so the
// adapter does not surface ApiKeyClass/ApiKeyFingerprint — those would
// be empty and could mislead audit consumers expecting an API key path.
// Provider is set to "chatgpt-web" so traffic_event.provider_name
// disambiguates this surface from "openai" (the public API).
func (a *Adapter) DetectRequestMeta(_ *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "chatgpt-web"}
	if !gjson.ValidBytes(body) {
		return meta
	}
	if model := gjson.GetBytes(body, "model"); model.Exists() && model.Type == gjson.String {
		meta.Model = model.Str
	} else if slug := gjson.GetBytes(body, "model_slug"); slug.Exists() && slug.Type == gjson.String {
		meta.Model = slug.Str
	}
	return meta
}

// DetectResponseUsage returns a non-LLM-tier sentinel because chatgpt.com
// does not return token usage in its wire format (it is a consumer
// product, not a metered API). Audit pipelines that record usage
// counts will see the non-LLM status and skip cost calculation rather
// than infer fake counts.
func (a *Adapter) DetectResponseUsage(_ *http.Response, _ []byte) traffic.UsageMeta {
	return traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
}

// RewriteRequestBody is unsupported. The chatgpt.com client side has
// tight integrity expectations on the request body shape (action types,
// parent_message_id linkage, websocket_request_id telemetry) that the
// NormalizedContent abstraction does not capture. Hooks that produce a
// Modify decision against chatgpt-web request traffic forward the
// original body unchanged plus a warn-level log.
func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}

// RewriteResponseBody is unsupported. SSE JSON-Patch streams are not
// safely reconstructable from a NormalizedContent snapshot — the
// patches reference path indices that depend on prior delta history.
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}

// Normalize implements normalize.Normalizer. Uses extract.NormalizeForAdapter
// with the chatgpt-web request and response specs so the audit normalize layer
// classifies captured bodies as KindAIChat rather than the verbatim catch-all.
// MinConfidence 0.5 — the caller has already routed to this adapter, so
// partial pattern hits are more reliable than a generic probe.
func (a *Adapter) Normalize(_ context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     adapterID,
		ReqSpecIDs:    []string{"chatgpt-web"},
		RespSpecIDs:   []string{"chatgpt-web"},
		MinConfidence: 0.5,
	})
}
