// Package anthropic implements the traffic adapter for the Anthropic
// Messages API. Extracts content from system prompts, messages,
// responses, streaming deltas, plus tool_use invocations and
// extended-thinking traces.
package anthropic

import (
	"context"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Adapter implements the Anthropic content extraction.
type Adapter struct{}

// ID returns the adapter identifier.
func (a *Adapter) ID() string { return "anthropic" }

// Configure is a no-op for the anthropic adapter (no custom config needed).
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses an Anthropic Messages API request body.
//
// Surfaces every audit-relevant field:
//   - System prompt (string or array of {type:"text"} blocks) → Segments
//   - User messages and tool_result text → Segments (compliance scans
//     mid-conversation tool returns for PII)
//   - Assistant tool_use blocks echoed in conversation history →
//     ToolCallSegments (so prior tool arguments / MCP invocations
//     reach hooks)
//   - Top-level `tools` and `mcp_servers` definitions → Metadata
//   - Top-level `model`, `service_tier`, `anthropic_beta` → Metadata
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments, toolCalls []string

	// Extract system prompt: string OR array of {type:"text", text:"..."}.
	sys := gjson.GetBytes(body, "system")
	if sys.Exists() {
		if sys.Type == gjson.String {
			segments = append(segments, sys.Str)
		} else if sys.IsArray() {
			sys.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").Str == "text" {
					segments = append(segments, part.Get("text").Str)
				}
				return true
			})
		}
	}

	// Extract messages: content is string OR array of content blocks.
	// Block types we recognise:
	//   - text         → Segments
	//   - tool_use     → ToolCallSegments (assistant invocation echoed in history)
	//   - tool_result  → Segments (text fragments from prior tool returns;
	//                    compliance scans mid-conversation tool returns
	//                    for PII because tool returns may carry customer data)
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.Exists() {
			return true
		}
		if content.Type == gjson.String {
			segments = append(segments, content.Str)
		} else if content.IsArray() {
			content.ForEach(func(_, part gjson.Result) bool {
				switch part.Get("type").Str {
				case "text":
					segments = append(segments, part.Get("text").Str)
				case "tool_use":
					toolCalls = append(toolCalls, part.Raw)
				case "tool_result":
					tc := part.Get("content")
					switch {
					case tc.Type == gjson.String:
						segments = append(segments, tc.Str)
					case tc.IsArray():
						tc.ForEach(func(_, sub gjson.Result) bool {
							if sub.Get("type").Str == "text" {
								segments = append(segments, sub.Get("text").Str)
							}
							return true
						})
					}
				}
				return true
			})
		}
		return true
	})

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Exists() && model.Type == gjson.String {
		meta["model"] = model.Str
	}
	// Tool definitions: list of tools the assistant is authorized to invoke.
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		meta["tools"] = tools.Raw
	}
	// MCP server definitions: list of MCP servers connected to this request.
	// Anthropic's native MCP integration carries the server config in the
	// request body so audit can record the MCP scope of each conversation.
	if mcp := gjson.GetBytes(body, "mcp_servers"); mcp.IsArray() {
		meta["mcp_servers"] = mcp.Raw
	}
	if st := gjson.GetBytes(body, "service_tier"); st.Exists() && st.Type == gjson.String && st.Str != "" {
		meta["service_tier"] = st.Str
	}
	if av := gjson.GetBytes(body, "anthropic_version"); av.Exists() && av.Type == gjson.String && av.Str != "" {
		meta["anthropic_version"] = av.Str
	}

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
	}, nil
}

// ExtractResponse parses an Anthropic Messages API response body.
//
//   - text blocks      → Segments
//   - thinking blocks  → ReasoningSegments (kept off Segments so audit
//     transcripts and rewrite targeting do not mix
//     reasoning into the assistant's user-visible answer)
//   - tool_use blocks  → ToolCallSegments
//
// Response-wide metadata (model, stop_reason, stop_sequence, message id)
// is captured into Metadata.
func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	content := gjson.GetBytes(body, "content")
	if !content.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments, reasoning, toolCalls []string
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").Str {
		case "text":
			segments = append(segments, part.Get("text").Str)
		case "thinking":
			if t := part.Get("thinking"); t.Exists() && t.Type == gjson.String {
				reasoning = append(reasoning, t.Str)
			}
		case "tool_use":
			toolCalls = append(toolCalls, part.Raw)
		}
		return true
	})

	meta := map[string]string{}
	if id := gjson.GetBytes(body, "id"); id.Exists() && id.Type == gjson.String && id.Str != "" {
		meta["id"] = id.Str
	}
	if m := gjson.GetBytes(body, "model"); m.Exists() && m.Type == gjson.String && m.Str != "" {
		meta["model"] = m.Str
	}
	if sr := gjson.GetBytes(body, "stop_reason"); sr.Exists() && sr.Type == gjson.String && sr.Str != "" {
		meta["stop_reason"] = sr.Str
	}
	if ss := gjson.GetBytes(body, "stop_sequence"); ss.Exists() && ss.Type == gjson.String && ss.Str != "" {
		meta["stop_sequence"] = ss.Str
	}

	return traffic.NormalizedContent{
		Segments:          segments,
		ReasoningSegments: reasoning,
		ToolCallSegments:  toolCalls,
		Metadata:          meta,
	}, nil
}

// ExtractStreamChunk parses a single SSE event from the Anthropic
// streaming API. The streaming pipeline strips the `event:` /
// `data:` lines before calling this; we receive one parsed JSON
// event payload per call.
//
// Events we extract content from:
//   - content_block_start with content_block.type=tool_use → emits the
//     initial tool_use block (id + name + initial input) on
//     ToolCallSegments so the pipeline knows a tool call is starting.
//   - content_block_delta with delta.type=text_delta     → Segments
//   - content_block_delta with delta.type=thinking_delta → ReasoningSegments
//   - content_block_delta with delta.type=input_json_delta → tool
//     argument fragment on ToolCallSegments (pipeline accumulates).
//   - message_delta carrying delta.stop_reason / stop_sequence →
//     Metadata.
//
// Other event types — message_start / message_stop /
// content_block_stop / ping / error — carry no canonical content and
// return empty.
func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	var segments, reasoning, toolCalls []string
	meta := map[string]string{}

	switch gjson.GetBytes(chunk, "type").Str {
	case "content_block_start":
		// Initial frame for a tool_use content block carries the id +
		// name + (possibly empty) input. Emit it so audit captures the
		// invocation start, even if subsequent input_json_delta frames
		// add the full arguments.
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
			// Tool argument fragment streamed as a JSON-text partial.
			// Forward the raw delta object so downstream accumulation
			// has the full context (`partial_json`, `index`).
			toolCalls = append(toolCalls, delta.Raw)
		}
	case "message_delta":
		// Carries the terminal stop_reason / stop_sequence. usage
		// numbers also live here but are extracted by DetectResponseUsage
		// path; we surface stop_reason for hook visibility.
		if sr := gjson.GetBytes(chunk, "delta.stop_reason"); sr.Exists() && sr.Type == gjson.String && sr.Str != "" {
			meta["stop_reason"] = sr.Str
		}
		if ss := gjson.GetBytes(chunk, "delta.stop_sequence"); ss.Exists() && ss.Type == gjson.String && ss.Str != "" {
			meta["stop_sequence"] = ss.Str
		}
	}

	// Empty-meta optimisation: do not allocate a map when nothing was set.
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
