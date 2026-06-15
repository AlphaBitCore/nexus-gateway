// Package cursor implements the cursor traffic adapter for Cursor IDE
// backend traffic to api2.cursor.sh, api3.cursor.sh, and cursor.com.
//
// Cursor's wire format is Connect-RPC (Buf's Connect protocol) with
// protobuf payloads. Chat endpoints use GetChatRequest / StreamChatResponse
// whose field layout was reverse-engineered from the cursor-agent bundle:
//
//	GetChatRequest (request to /aiserver.v1.AiService/Stream*):
//	  field 2  repeated ConversationMessage  conversation
//	  field 7  ModelDetails                  model_details
//	  field 9  string                        request_id
//	  field 15 string                        conversation_id
//
//	ConversationMessage:
//	  field 1  string  text
//	  field 2  enum    type  (1=user 2=assistant)
//
//	StreamChatResponse (each Connect-RPC frame from the server):
//	  field 1  string  text   (token delta)
//	  field 22 string  server_bubble_id
//
// JSON envelopes (rare relay paths) are handled with the legacy gjson path.
package cursor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "cursor"

// requestKnownKeys lists JSON top-level fields the adapter recognises;
// anything else lands in Extra.
var requestKnownKeys = []string{
	"messages", "prompt", "query", "text", "input", "model",
	"stream", "session_id", "conversation_id", "request_id",
	"workspace_root_path", "context", "model_details",
}

// Adapter implements cursor extraction.
type Adapter struct{}

// ID returns the canonical adapter identifier.
func (a *Adapter) ID() string { return adapterID }

// Configure is a no-op.
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses a Cursor request body.
//
// For Connect-RPC chat paths (/aiserver.v1.AiService/Stream*) the body is
// a raw protobuf GetChatRequest. We decode the conversation history using
// protowire and emit every message as a "[role] text" segment.
//
// For JSON envelopes (rare relay paths) the legacy gjson path is used.
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	if !looksLikeJSON(body) {
		// Binary protobuf — try to parse as GetChatRequest when the path
		// indicates a streaming chat endpoint.
		if isChatPath(path) {
			return extractGetChatRequest(body)
		}
		return traffic.NormalizedContent{
			Extra: map[string]string{"binary_preview": preview(body)},
		}, traffic.ErrUnknownSchema
	}
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	var segments, toolCalls []string

	// (1) OpenAI-compat messages array (Cursor sometimes proxies
	// upstream chat completions wrapped in a JSON envelope).
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
			// Cursor sometimes uses {author, text} like Copilot.
			if t := msg.Get("text"); t.Type == gjson.String && t.Str != "" {
				segments = append(segments, t.Str)
			}
			return true
		})
	}

	// (2) Top-level prompt-style fields.
	for _, key := range []string{"prompt", "query", "text", "input"} {
		if v := gjson.GetBytes(body, key); v.Type == gjson.String && v.Str != "" {
			segments = append(segments, v.Str)
		}
	}

	if len(segments) == 0 && len(toolCalls) == 0 {
		// JSON without recognisable AI content fields — surface as
		// unknown so the policy can branch on it.
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

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
		Extra:            traffic.CollectExtra(body, requestKnownKeys),
	}, nil
}

// ExtractResponse handles a buffered (non-streaming) response. JSON
// error envelopes are extracted; protobuf bodies are opaque.
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
	// OpenAI-compat happy-path JSON response (rare but covered).
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

// ExtractStreamChunk handles a single streaming frame.
//
// For Connect-RPC chat paths the caller (streaming.PassthroughWithConnectRPCExtract)
// passes the raw protobuf payload of each Connect-RPC frame AFTER stripping the
// 5-byte envelope header. We decode it as StreamChatResponse and return the
// text delta (field 1).
//
// For JSON chunks (legacy relay path) the OpenAI-compat delta shape is tried.
func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, path string) (traffic.NormalizedContent, error) {
	if len(chunk) == 0 {
		return traffic.NormalizedContent{}, nil
	}
	if !looksLikeJSON(chunk) {
		// Raw protobuf frame payload. The frame schema differs by service:
		//   - /aiserver.v1.AiService/Stream* : StreamChatResponse, field 1 = text
		//     delta (the legacy chat path).
		//   - /agent.v1.AgentService/*       : the agent service embeds its
		//     conversation as OpenAI-compat {"role","content":[{type,text}]} JSON
		//     (and Lexical {"root":...} blocks for the typed message) inside
		//     protobuf string fields — decode those, not field 1.
		// On any other protobuf path emit nothing (a StreamChatResponse decode of
		// a foreign frame yields a coincidental field-1 byte — the stray "j").
		switch {
		case isAgentRunPath(path):
			return extractCursorAgentFrame(chunk), nil
		case isChatPath(path):
			text := parseStreamChatResponseText(chunk)
			if text == "" {
				return traffic.NormalizedContent{}, nil
			}
			return traffic.NormalizedContent{Segments: []string{text}}, nil
		default:
			return traffic.NormalizedContent{}, nil
		}
	}
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, nil
	}

	var segments, toolCalls []string

	// OpenAI-compat delta.
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

	// Plain text chunk.
	if t := gjson.GetBytes(chunk, "text"); t.Type == gjson.String && t.Str != "" {
		segments = append(segments, t.Str)
	}
	if c := gjson.GetBytes(chunk, "content"); c.Type == gjson.String && c.Str != "" {
		segments = append(segments, c.Str)
	}
	return traffic.NormalizedContent{Segments: segments}, nil
}

// DetectRequestMeta sets Provider="cursor" and derives the Model from the
// request path for Connect-RPC chat endpoints (model is inside the protobuf
// body which we don't fully decode here, so we use the path as a proxy).
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "cursor"}
	if r != nil {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			tok := strings.TrimSpace(auth[len("Bearer "):])
			if tok != "" {
				meta.ApiKeyClass = "cursor-bearer"
				meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(tok)
			}
		}
		// Derive a synthetic model label from the RPC path so audit rows
		// have a non-empty model field for Connect-RPC traffic.
		if r.URL != nil && isChatPath(r.URL.Path) {
			meta.Model = rpcPathToModel(r.URL.Path)
		}
	}
	if gjson.ValidBytes(body) {
		if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
			meta.Model = model.Str
		}
	}
	return meta
}

// DetectResponseUsage returns Status=non_llm by default. Cursor
// surfaces token usage to the IDE side-channel, not on the wire we
// can audit at proxy level.
func (a *Adapter) DetectResponseUsage(_ *http.Response, _ []byte) traffic.UsageMeta {
	return traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
}

// RewriteRequestBody is unsupported. Cursor request bodies often
// contain signed protobuf envelopes whose integrity is verified at the
// upstream, and JSON requests carry session tokens we cannot regenerate.
func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}

// RewriteResponseBody is unsupported.
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}

// looksLikeJSON does a cheap first-byte sniff to distinguish JSON from
// gRPC-Web / protobuf framing. Whitespace tolerant.
func looksLikeJSON(b []byte) bool {
	for _, c := range b {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		return c == '{' || c == '['
	}
	return false
}

// preview returns a truncated, ASCII-safe rendering of the body for
// the Extra["binary_preview"] safety net.
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

// isChatPath returns true for Connect-RPC paths that carry GetChatRequest bodies.
func isChatPath(path string) bool {
	return strings.HasPrefix(path, "/aiserver.v1.AiService/Stream") ||
		strings.HasPrefix(path, "/aiserver.v1.AiService/WarmChatCache") ||
		strings.HasPrefix(path, "/aiserver.v1.AiService/WarmComposerCache")
}

// rpcPathToModel converts a Connect-RPC path to a human-readable model label.
func rpcPathToModel(path string) string {
	switch {
	case strings.Contains(path, "StreamComposer"):
		return "cursor-composer"
	case strings.Contains(path, "StreamChat"):
		return "cursor-chat"
	default:
		// Extract the method name from the path as a fallback.
		if idx := strings.LastIndex(path, "/"); idx >= 0 {
			return "cursor-" + strings.ToLower(path[idx+1:])
		}
		return "cursor"
	}
}

// extractGetChatRequest parses a binary GetChatRequest protobuf and returns
// the conversation history as "[role] text" segments.
//
// Field layout (from cursor-agent bundle reverse engineering):
//
//	field 2 (repeated bytes) → ConversationMessage
//	field 9 (string)         → request_id
//	field 15 (string)        → conversation_id
func extractGetChatRequest(body []byte) (traffic.NormalizedContent, error) {
	var segments []string
	meta := map[string]string{}

	b := body
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]

		switch {
		case num == 2 && typ == protowire.BytesType:
			// repeated ConversationMessage conversation
			msgBytes, n := protowire.ConsumeBytes(b)
			if n < 0 {
				b = nil
				break
			}
			b = b[n:]
			role, text := parseConversationMessage(msgBytes)
			if text != "" {
				segments = append(segments, fmt.Sprintf("[%s] %s", role, text))
			}

		case num == 15 && typ == protowire.BytesType:
			// string conversation_id
			s, n := protowire.ConsumeBytes(b)
			if n < 0 {
				b = nil
				break
			}
			b = b[n:]
			if len(s) > 0 {
				meta["conversation_id"] = string(s)
			}

		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				b = nil
				break
			}
			b = b[n:]
		}
	}

	if len(segments) == 0 {
		return traffic.NormalizedContent{
			Extra: map[string]string{"binary_preview": preview(body)},
		}, traffic.ErrUnknownSchema
	}
	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
	}, nil
}

// parseConversationMessage decodes a single ConversationMessage protobuf.
//
//	field 1 (string) → text
//	field 2 (varint) → type  (1=user 2=assistant)
func parseConversationMessage(msg []byte) (role, text string) {
	role = "user" // default when type field is absent
	b := msg
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]

		switch {
		case num == 1 && typ == protowire.BytesType:
			s, n := protowire.ConsumeBytes(b)
			if n < 0 {
				b = nil
				break
			}
			b = b[n:]
			text = string(s)

		case num == 2 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				b = nil
				break
			}
			b = b[n:]
			switch v {
			case 1:
				role = "user"
			case 2:
				role = "assistant"
			default:
				role = "unknown"
			}

		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				b = nil
				break
			}
			b = b[n:]
		}
	}
	return role, text
}

// parseStreamChatResponseText extracts the text delta (field 1) from a raw
// StreamChatResponse protobuf payload (without the 5-byte Connect-RPC frame
// header — the caller strips it before passing the bytes here).
func parseStreamChatResponseText(frame []byte) string {
	b := frame
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]

		if num == 1 && typ == protowire.BytesType {
			s, n := protowire.ConsumeBytes(b)
			if n < 0 {
				break
			}
			return string(s)
		}
		// Skip all other fields.
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			break
		}
		b = b[n:]
	}
	return ""
}
