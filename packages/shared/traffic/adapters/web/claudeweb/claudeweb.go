// Package claudeweb implements the claude-web traffic adapter for
// browser-side traffic to the claude.ai consumer client. This is
// distinct from the public Anthropic Messages API (handled by the
// anthropic adapter): claude.ai posts to
// /api/organizations/<org>/chat_conversations/<conversation>/completion
// and streams responses as SSE.
//
// The wire format on the response side has historically alternated
// between two shapes as claude.ai migrates its backend:
//
//  1. Legacy `completion` event shape:
//     event: completion
//     data: {"completion":"<delta text>","stop_reason":null,"model":"..."}
//
//  2. Anthropic-Messages-API-style event shape:
//     event: content_block_delta
//     data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<delta>"}}
//
// The adapter parses both shapes from a single ExtractStreamChunk
// implementation. The streaming pipeline strips the `event:` /
// `data:` envelope before calling this; we receive one parsed JSON
// payload per call.
//
// Capabilities:
//   - ExtractRequest: pulls the user's `prompt` (text) plus any
//     attachments / files text from the JSON body.
//   - ExtractStreamChunk: parses both legacy and Anthropic-style
//     deltas; handles tool_use / thinking / input_json_delta if the
//     wire migrates further.
//   - ExtractResponse: handles buffered error envelopes (typical for
//     auth / quota errors that return JSON instead of SSE).
//   - DetectRequestMeta: Provider="claude-web"; ApiKey fields stay
//     empty (claude.ai authenticates via session cookies).
//   - DetectResponseUsage: returns Status=non_llm because claude.ai
//     does not surface token counts on the wire (consumer product).
//   - Rewrite{Request,Response}Body: not supported.
package claudeweb

import (
	"context"
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "claude-web"

// requestKnownKeys lists fields in the claude.ai conversation completion
// request body that the adapter recognises. Anything else lands in
// Extra so a future protocol revision (a brand-new field carrying user
// data) reaches compliance hooks.
var requestKnownKeys = []string{
	"prompt", "parent_message_uuid", "timezone", "personalized_styles",
	"locale", "tools", "attachments", "files", "sync_sources",
	"rendering_mode", "render_to_format", "max_tokens",
	"completion", "model", "conversation_uuid", "organization_uuid",
	"client_metadata", "stream", "stop_sequences",
	"streaming_mode", "input_metadata",
}

// Adapter implements claude-web extraction.
type Adapter struct{}

// ID returns the canonical adapter identifier.
func (a *Adapter) ID() string { return adapterID }

// Configure is a no-op — claude-web has no per-domain configuration.
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses the claude.ai conversation completion request
// body. The user's new prompt lives in the `prompt` field; attachments
// and files carry text content the user uploaded inline.
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	// Require either the prompt (current shape) or messages (if claude.ai
	// migrates to Anthropic-Messages-API-style requests). Bodies missing
	// both look like account / metadata calls we do not audit at the
	// claude-web layer.
	prompt := gjson.GetBytes(body, "prompt")
	messages := gjson.GetBytes(body, "messages")
	if !prompt.Exists() && !messages.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments []string

	if prompt.Type == gjson.String && prompt.Str != "" {
		segments = append(segments, prompt.Str)
	}

	// Attachments: each entry typically has {file_name, file_size,
	// file_type, extracted_content} where extracted_content is the
	// text the user uploaded. Compliance scans this for PII / secrets.
	if att := gjson.GetBytes(body, "attachments"); att.IsArray() {
		att.ForEach(func(_, item gjson.Result) bool {
			if t := item.Get("extracted_content"); t.Exists() && t.Type == gjson.String && t.Str != "" {
				segments = append(segments, t.Str)
			}
			return true
		})
	}

	// Files: similar shape — { file_name, file_uuid, ... extracted_content? }.
	if files := gjson.GetBytes(body, "files"); files.IsArray() {
		files.ForEach(func(_, item gjson.Result) bool {
			if t := item.Get("extracted_content"); t.Exists() && t.Type == gjson.String && t.Str != "" {
				segments = append(segments, t.Str)
			}
			return true
		})
	}

	// If a future claude.ai posts Anthropic-Messages-API-style messages,
	// extract them too so the adapter does not regress when the wire
	// migrates.
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
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
			return true
		})
	}

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Exists() && model.Type == gjson.String {
		meta["model"] = model.Str
	}
	if cu := gjson.GetBytes(body, "conversation_uuid"); cu.Exists() && cu.Type == gjson.String {
		meta["conversation_uuid"] = cu.Str
	}
	if pmu := gjson.GetBytes(body, "parent_message_uuid"); pmu.Exists() && pmu.Type == gjson.String {
		meta["parent_message_uuid"] = pmu.Str
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		meta["tools"] = tools.Raw
	}

	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
		Extra:    traffic.CollectExtra(body, requestKnownKeys),
	}, nil
}

// ExtractResponse handles a buffered (non-streaming) response. claude.ai
// streams successful conversations, so this path mostly covers JSON
// error envelopes:
//
//	{"error":{"type":"...","message":"..."}}
//	{"detail":"..."}
//	{"message":"..."}
//
// Successful streamed bodies should be fed via ExtractStreamChunk.
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
	if detail := gjson.GetBytes(body, "detail"); detail.Type == gjson.String && detail.Str != "" {
		return traffic.NormalizedContent{
			Segments: []string{detail.Str},
			Metadata: map[string]string{"error": "true"},
		}, nil
	}
	if msg := gjson.GetBytes(body, "message"); msg.Type == gjson.String && msg.Str != "" {
		return traffic.NormalizedContent{
			Segments: []string{msg.Str},
			Metadata: map[string]string{"error": "true"},
		}, nil
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// ExtractStreamChunk parses one SSE event from claude.ai. Two shapes
// are supported:
//
// (1) Legacy `completion` event:
//
//	{"completion":"<delta>","stop_reason":null,"model":"...","truncated":false,"log_id":"..."}
//
// (2) Anthropic-Messages-API-style event (newer):
//
//	{"type":"content_block_delta","delta":{"type":"text_delta","text":"<delta>"}}
//	{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"<delta>"}}
//	{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"..."}}
//	{"type":"content_block_start","content_block":{"type":"tool_use","id":"...","name":"...","input":{}}}
//	{"type":"message_delta","delta":{"stop_reason":"end_turn"}}
//
// Plus filtered-out keep-alive / metadata events:
//
//	{"type":"ping"} / {"type":"message_start"} / {"type":"message_stop"} /
//	{"type":"content_block_stop"} / {"type":"error"}
//
// The fail-open posture: unrecognised JSON shapes do not error.
func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	if len(chunk) == 0 {
		return traffic.NormalizedContent{}, nil
	}
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, nil
	}

	var segments, reasoning, toolCalls []string
	meta := map[string]string{}

	// Shape (1): legacy `completion` field present (no `type` discriminator).
	if comp := gjson.GetBytes(chunk, "completion"); comp.Exists() && comp.Type == gjson.String && comp.Str != "" {
		segments = append(segments, comp.Str)
		if sr := gjson.GetBytes(chunk, "stop_reason"); sr.Exists() && sr.Type == gjson.String && sr.Str != "" {
			meta["stop_reason"] = sr.Str
		}
		if m := gjson.GetBytes(chunk, "model"); m.Exists() && m.Type == gjson.String && m.Str != "" {
			meta["model"] = m.Str
		}
		var outMeta map[string]string
		if len(meta) > 0 {
			outMeta = meta
		}
		return traffic.NormalizedContent{Segments: segments, Metadata: outMeta}, nil
	}

	// Shape (2): Anthropic-Messages-API-style event with `type` discriminator.
	switch gjson.GetBytes(chunk, "type").Str {
	case "content_block_start":
		cb := gjson.GetBytes(chunk, "content_block")
		if cb.Get("type").Str == "tool_use" {
			toolCalls = append(toolCalls, cb.Raw)
		}
	case "content_block_delta":
		delta := gjson.GetBytes(chunk, "delta")
		switch delta.Get("type").Str {
		case "text_delta":
			if text := delta.Get("text").Str; text != "" {
				segments = append(segments, text)
			}
		case "thinking_delta":
			if text := delta.Get("thinking").Str; text != "" {
				reasoning = append(reasoning, text)
			}
		case "input_json_delta":
			toolCalls = append(toolCalls, delta.Raw)
		}
	case "message_delta":
		if sr := gjson.GetBytes(chunk, "delta.stop_reason"); sr.Exists() && sr.Type == gjson.String && sr.Str != "" {
			meta["stop_reason"] = sr.Str
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

// DetectRequestMeta returns the claude-web RequestMeta. Provider is set
// to "claude-web" so traffic_event.provider_name disambiguates from
// "anthropic" (the public API). Authentication is via session cookies,
// not Bearer tokens, so ApiKeyClass / ApiKeyFingerprint stay empty.
func (a *Adapter) DetectRequestMeta(_ *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "claude-web"}
	if !gjson.ValidBytes(body) {
		return meta
	}
	if model := gjson.GetBytes(body, "model"); model.Exists() && model.Type == gjson.String {
		meta.Model = model.Str
	}
	return meta
}

// DetectResponseUsage returns the non-LLM-tier sentinel because
// claude.ai (consumer product) does not return token usage on the
// wire. Audit pipelines that record usage counts will see the non-LLM
// status and skip cost calculation rather than infer fake counts.
func (a *Adapter) DetectResponseUsage(_ *http.Response, _ []byte) traffic.UsageMeta {
	return traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
}

// RewriteRequestBody is unsupported. The claude.ai client side has tight
// integrity expectations (parent_message_uuid linkage, conversation_uuid
// references, attachment binary references) that NormalizedContent does
// not capture.
func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}

// RewriteResponseBody is unsupported. The SSE event stream is not safely
// reconstructable from a NormalizedContent snapshot.
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
