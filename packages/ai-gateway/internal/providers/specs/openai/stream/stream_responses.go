// Package stream — stream_responses.go parses OpenAI Responses-API
// streaming responses (SSE event grammar) into canonical Chunks. Used by
// the auto-upgrade path when the gateway has rewritten an inbound
// /v1/chat/completions stream onto OpenAI's /v1/responses endpoint.
//
// Event grammar covered (per the openai-python Responses event types):
//
//	response.created                          → role assistant + record id/model
//	response.in_progress / .queued            → informational; no canonical emit
//	response.output_item.added                → track item index; classify type
//	response.content_part.added               → track content index
//	response.output_text.delta                → Chunk.Delta
//	response.output_text.done                 → bookkeeping
//	response.function_call_arguments.delta    → Chunk.ToolCallDeltas
//	response.function_call_arguments.done     → bookkeeping
//	response.reasoning_summary_text.delta     → Chunk.ReasoningDelta
//	response.reasoning_summary_text.done      → bookkeeping
//	response.reasoning_text.delta             → Chunk.ReasoningDelta (newer event)
//	response.reasoning_text.done              → bookkeeping
//	response.content_part.done /
//	response.output_item.done                 → bookkeeping (block boundary)
//	response.refusal.delta / .done            → Chunk.Delta (canonical has no refusal CB)
//	response.completed                        → emit final Usage + Done=true
//	response.incomplete                       → emit Done=true (incomplete: length/cf)
//	response.failed / .error                  → emit Done=true (caller surfaces error
//	                                            via the separate error_envelope path)
//	response.web_search_call.* / .file_search_call.* / .image_generation_call.* /
//	response.mcp_call_arguments.* / .code_interpreter_call_code.* /
//	response.computer_call.*                  → informational built-in tool events;
//	                                            no canonical emission (these only fire
//	                                            on same-shape passthrough, which uses
//	                                            the raw byte copier, not this session)
//
// Unknown event types are logged once (slog WARN) and skipped — the
// stream MUST never abort on a new event type the upstream introduces.
package stream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// ResponsesStreamDecoder builds responsesStreamSession instances. Mirrors
// the StreamDecoder used for chat-completions; kept separate so callers
// can choose explicitly which session to instantiate (the wire shape is
// different — embeddable as a switch on the upstream URL path).
type ResponsesStreamDecoder struct {
	log *slog.Logger
}

// NewResponsesStreamDecoder returns a ResponsesStreamDecoder.
func NewResponsesStreamDecoder(log *slog.Logger) *ResponsesStreamDecoder {
	if log == nil {
		log = slog.Default()
	}
	return &ResponsesStreamDecoder{log: log}
}

// Open wraps body in a responsesStreamSession. The endpoint parameter is
// accepted for interface symmetry with chat-completions StreamDecoder.Open
// but is unused — Responses-API has only one streaming endpoint shape.
func (d *ResponsesStreamDecoder) Open(body io.ReadCloser, _ typology.WireShape) (provcore.StreamSession, error) {
	if body == nil {
		return nil, fmt.Errorf("openai-responses: nil stream body")
	}
	return &responsesStreamSession{
		scanner: specutil.NewSSEScanner(body),
		log:     d.log,
	}, nil
}

// responsesStreamSession is the SSE → canonical Chunk pump.
type responsesStreamSession struct {
	scanner *specutil.SSEScanner
	log     *slog.Logger
	done    bool

	// Per-stream state: the current open output item type drives how we
	// classify subsequent delta events without re-reading the item JSON
	// on every chunk. Initialized to "" before any output_item.added.
	currentItemType string
}

func (s *responsesStreamSession) Next(ctx context.Context) (provcore.Chunk, error) {
	if s.done {
		return provcore.Chunk{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return provcore.Chunk{}, err
	}

	for {
		ev, err := s.scanner.Next()
		if err != nil {
			return provcore.Chunk{}, err
		}
		// Responses-API never sends "[DONE]" — the terminal signal is
		// response.completed / .failed / .incomplete. Match defensively in
		// case a future SDK adds it.
		if bytes.Equal(bytes.TrimSpace(ev.Data), []byte("[DONE]")) {
			s.done = true
			return provcore.Chunk{Done: true, RawBytes: []byte("data: [DONE]\n\n"), NativeEvent: ev.Event}, nil
		}
		if len(ev.Data) == 0 {
			continue
		}

		evType := ev.Event
		if evType == "" {
			// Fall back to the inline "type" field when the SSE event
			// header is absent (some intermediaries strip it).
			evType = gjson.GetBytes(ev.Data, "type").String()
		}

		switch evType {
		case "response.created":
			// No canonical emit; just record currentItemType reset.
			s.currentItemType = ""
			continue
		case "response.in_progress", "response.queued":
			continue
		case "response.output_item.added":
			s.currentItemType = gjson.GetBytes(ev.Data, "item.type").String()
			continue
		case "response.content_part.added", "response.content_part.done",
			"response.output_item.done",
			"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
			"response.output_text.done",
			"response.function_call_arguments.done",
			"response.reasoning_summary_text.done", "response.reasoning_text.done",
			"response.refusal.done",
			"response.output_text.annotation.added":
			// Bookkeeping events — no canonical emit.
			continue
		case "response.output_text.delta":
			delta := gjson.GetBytes(ev.Data, "delta").String()
			if delta == "" {
				continue
			}
			return provcore.Chunk{
				Delta:       delta,
				RawBytes:    formatSSE(evType, ev.Data),
				NativeEvent: evType,
			}, nil
		case "response.function_call_arguments.delta":
			delta := gjson.GetBytes(ev.Data, "delta").String()
			// item_id + output_index identify which function_call item this
			// delta belongs to. Use output_index as ToolCallDelta.Index.
			idx := int(gjson.GetBytes(ev.Data, "output_index").Int())
			return provcore.Chunk{
				ToolCallDeltas: []provcore.ToolCallDelta{{
					Index:     idx,
					ID:        gjson.GetBytes(ev.Data, "item_id").String(),
					Arguments: delta,
				}},
				RawBytes:    formatSSE(evType, ev.Data),
				NativeEvent: evType,
			}, nil
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			delta := gjson.GetBytes(ev.Data, "delta").String()
			if delta == "" {
				continue
			}
			return provcore.Chunk{
				ReasoningDelta: delta,
				RawBytes:       formatSSE(evType, ev.Data),
				NativeEvent:    evType,
			}, nil
		case "response.refusal.delta":
			// Canonical has no refusal content block; surface as Delta with
			// a one-time WarnOnce so audit sees the bytes.
			refusalSurfacedAsDeltaOnce.Do(func() {
				s.log.Warn("nexus: surfacing Responses-API refusal.delta as canonical Delta",
					slog.String("event", "responses_refusal_as_delta"))
			})
			return provcore.Chunk{
				Delta:       gjson.GetBytes(ev.Data, "delta").String(),
				RawBytes:    formatSSE(evType, ev.Data),
				NativeEvent: evType,
			}, nil
		case "response.completed", "response.incomplete":
			s.done = true
			usage := specutil.ExtractOpenAIUsage(gjson.GetBytes(ev.Data, "response.usage"))
			return provcore.Chunk{
				Done:        true,
				Usage:       usagePtrOrNil(usage),
				RawBytes:    formatSSE(evType, ev.Data),
				NativeEvent: evType,
			}, nil
		case "response.failed", "response.error":
			s.done = true
			return provcore.Chunk{
				Done:        true,
				RawBytes:    formatSSE(evType, ev.Data),
				NativeEvent: evType,
			}, nil
		default:
			// Built-in tool events + unknown future events: silently drop
			// from canonical emit (built-ins only fire on same-shape
			// passthrough which doesn't go through this session). Log
			// unknowns once per stream so operators see drift early.
			if !isBuiltinToolEvent(evType) {
				unknownResponsesEventOnce.Do(func() {
					s.log.Warn("nexus: unknown Responses-API SSE event type — pass through",
						slog.String("event", evType))
				})
			}
			continue
		}
	}
}

func (s *responsesStreamSession) Close() error {
	s.done = true
	return s.scanner.Close()
}

// usagePtrOrNil returns a pointer to u when u has at least one populated
// field, else nil — preserves the "absent" semantics callers depend on.
func usagePtrOrNil(u provcore.Usage) *provcore.Usage {
	if u.PromptTokens == nil && u.CompletionTokens == nil && u.TotalTokens == nil &&
		u.CacheReadTokens == nil && u.ReasoningTokens == nil {
		return nil
	}
	return &u
}

// isBuiltinToolEvent reports whether an event name belongs to one of the
// OpenAI-native built-in tools (web_search, file_search, image_gen,
// computer, mcp, code_interpreter). These fire only on same-shape
// passthrough; the stream session is invoked only on the auto-upgrade /
// cross-format paths, so seeing one here is a no-op (not an error).
func isBuiltinToolEvent(evType string) bool {
	prefixes := []string{
		"response.web_search_call.",
		"response.file_search_call.",
		"response.image_generation_call.",
		"response.image_gen_call.",
		"response.mcp_call",
		"response.mcp_list_tools.",
		"response.computer_call.",
		"response.code_interpreter_call_code.",
		"response.code_interpreter_call.",
		"response.custom_tool_call.",
		"response.apply_patch_tool_call.",
		"response.function_shell_tool_call.",
		"response.tool_search_tool_call.",
		"response.audio.",
		"response.audio_transcript.",
	}
	for _, p := range prefixes {
		if len(evType) >= len(p) && evType[:len(p)] == p {
			return true
		}
	}
	return false
}

var (
	unknownResponsesEventOnce  sync.Once
	refusalSurfacedAsDeltaOnce sync.Once
)
