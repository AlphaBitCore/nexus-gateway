package canonicalbridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// openAIStreamEncoder converts canonical chunks to OpenAI chat.completion.chunk
// SSE frames. Used when ingress is OpenAI-like but the upstream provider speaks
// a different wire format (e.g. openai → anthropic, deepseek → gemini).
// LivePipeline appends data:[DONE] for OpenAI-shape ingress via EmitOpenAIDone,
// so this encoder emits nothing for chunk.Done after the finish_reason frame.
//
// Every emitted SSE frame carries the full OpenAI chunk envelope (id, object,
// created, model) so downstream clients can parse the stream with standard
// OpenAI SDKs.
type openAIStreamEncoder struct {
	id         string
	created    int64
	model      string
	headerSent bool
}

// NewChatCompletionsStreamEncoder returns an encoder that converts canonical
// provider.Chunk values into OpenAI chat-completions SSE frames. Exported for
// the auto-upgrade path (handler) which feeds upstream Responses-SSE-derived
// chunks back to a chat-completions client regardless of the (ingress, target)
// pair the bridge's NewStreamTranscoder would otherwise pick.
func NewChatCompletionsStreamEncoder(model string) StreamTranscoder {
	return newOpenAIStreamEncoder(model)
}

// NewResponsesStreamEncoder returns an encoder that converts canonical
// provider.Chunk values into OpenAI /v1/responses SSE event grammar.
// Exported for the cross-ingress cache-HIT path: when a stream-HIT
// entry's origin shape differs from the current ingress (e.g. cached
// chat.completion SSE replayed for a /v1/responses caller), the
// standard [Bridge.NewStreamTranscoder] passthrough rule short-circuits
// before the right encoder is picked; the handler bypasses that gate
// by calling this directly.
func NewResponsesStreamEncoder(model string) StreamTranscoder {
	return newResponsesStreamEncoder(model)
}

func newOpenAIStreamEncoder(model string) *openAIStreamEncoder {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return &openAIStreamEncoder{
		id:      "chatcmpl-" + hex.EncodeToString(b),
		created: time.Now().Unix(),
		model:   model,
	}
}

// oaiEnvelope wraps a choices slice in the full OpenAI chat.completion.chunk envelope.
func (e *openAIStreamEncoder) oaiEnvelope(choices []any) []byte {
	return oaiDeltaSSE(map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": choices,
	})
}

func (e *openAIStreamEncoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	var buf bytes.Buffer

	// Emit role-assignment chunk before any content.
	if !e.headerSent {
		e.headerSent = true
		buf.Write(e.oaiEnvelope([]any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{"role": "assistant", "content": ""},
				"finish_reason": nil,
			},
		}))
	}

	// Check content before Done: providers like Gemini 2.5 combine text,
	// finishReason, and usageMetadata into a single SSE frame, so chunk.Delta
	// can be non-empty even when chunk.Done is also true.
	if chunk.Delta != "" {
		buf.Write(e.oaiEnvelope([]any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{"content": chunk.Delta},
				"finish_reason": nil,
			},
		}))
	}
	if len(chunk.ToolCallDeltas) > 0 {
		buf.Write(e.oaiToolCallEnvelope(chunk.ToolCallDeltas))
	}
	if chunk.ReasoningDelta != "" {
		buf.Write(e.oaiEnvelope([]any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{"reasoning_content": chunk.ReasoningDelta},
				"finish_reason": nil,
			},
		}))
	}
	if chunk.Done {
		// Emit finish_reason=stop chunk before [DONE].
		finishChunk := map[string]any{
			"id":      e.id,
			"object":  "chat.completion.chunk",
			"created": e.created,
			"model":   e.model,
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": "stop",
				},
			},
		}
		if chunk.Usage != nil {
			u := map[string]any{}
			if chunk.Usage.PromptTokens != nil {
				u["prompt_tokens"] = *chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens != nil {
				u["completion_tokens"] = *chunk.Usage.CompletionTokens
			}
			if chunk.Usage.TotalTokens != nil {
				u["total_tokens"] = *chunk.Usage.TotalTokens
			}
			// Project the same detail sub-blocks the non-stream projector
			// emits (openai_projection.go projectUsage), so stream clients
			// can read cache-read + reasoning splits the same way as on a
			// non-stream response. Without these, gemini-2.5-pro / o1 /
			// kimi-k2.6 stream responses look reasoning_tokens=0 even when
			// thoughtsTokenCount /
			// completion_tokens_details.reasoning_tokens / etc. were
			// populated on the canonical chunk.
			if chunk.Usage.CacheReadTokens != nil && *chunk.Usage.CacheReadTokens > 0 {
				u["prompt_tokens_details"] = map[string]any{
					"cached_tokens": *chunk.Usage.CacheReadTokens,
				}
			}
			if chunk.Usage.ReasoningTokens != nil && *chunk.Usage.ReasoningTokens > 0 {
				u["completion_tokens_details"] = map[string]any{
					"reasoning_tokens": *chunk.Usage.ReasoningTokens,
				}
			}
			finishChunk["usage"] = u
		}
		buf.Write(oaiDeltaSSE(finishChunk))
		// [DONE] appended by LivePipeline.EmitOpenAIDone — return nil after.
	}

	if buf.Len() == 0 {
		return nil, nil
	}
	return buf.Bytes(), nil
}

// anthropicStreamEncoder converts canonical chunks to the Anthropic Messages
// streaming SSE event sequence:
//
//	message_start → content_block_start → content_block_delta × N
//	→ content_block_stop → message_delta → message_stop
//
// Stateful: maintains block indices across Write calls so the synthesised
// sequence is consistent with what a real Anthropic upstream would emit.
type anthropicStreamEncoder struct {
	headerSent   bool
	textBlockIdx int         // -1 when not yet opened
	toolBlockMap map[int]int // ToolCallDelta.Index → Anthropic content_block index
	nextBlockIdx int
}

func newAnthropicStreamEncoder() *anthropicStreamEncoder {
	return &anthropicStreamEncoder{textBlockIdx: -1}
}

func (e *anthropicStreamEncoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	var buf bytes.Buffer

	// Emit message_start before the first content.
	if !e.headerSent {
		e.headerSent = true
		var inputTokens, outputTokens int
		if chunk.Usage != nil {
			if chunk.Usage.PromptTokens != nil {
				inputTokens = *chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens != nil {
				outputTokens = *chunk.Usage.CompletionTokens
			}
		}
		writeAnthropicEvent(&buf, "message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            "msg_transcoded",
				"type":          "message",
				"role":          "assistant",
				"content":       []any{},
				"model":         "transcoded",
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":  inputTokens,
					"output_tokens": outputTokens,
				},
			},
		})
		writeAnthropicEvent(&buf, "ping", map[string]any{"type": "ping"})
	}

	// Tool call deltas: open a tool_use block on first delta for each tool index.
	for _, d := range chunk.ToolCallDeltas {
		if e.toolBlockMap == nil {
			e.toolBlockMap = make(map[int]int)
		}
		blockIdx, started := e.toolBlockMap[d.Index]
		if !started {
			blockIdx = e.nextBlockIdx
			e.nextBlockIdx++
			e.toolBlockMap[d.Index] = blockIdx
			writeAnthropicEvent(&buf, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": blockIdx,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    d.ID,
					"name":  d.Name,
					"input": map[string]any{},
				},
			})
		}
		if d.Arguments != "" {
			writeAnthropicEvent(&buf, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": blockIdx,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": d.Arguments,
				},
			})
		}
	}

	// Text delta: open text content_block on first occurrence.
	if chunk.Delta != "" {
		if e.textBlockIdx < 0 {
			e.textBlockIdx = e.nextBlockIdx
			e.nextBlockIdx++
			writeAnthropicEvent(&buf, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": e.textBlockIdx,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
		}
		writeAnthropicEvent(&buf, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": e.textBlockIdx,
			"delta": map[string]any{
				"type": "text_delta",
				"text": chunk.Delta,
			},
		})
	}

	// Reasoning delta (extended thinking): surface as thinking_delta.
	if chunk.ReasoningDelta != "" {
		if e.textBlockIdx < 0 {
			e.textBlockIdx = e.nextBlockIdx
			e.nextBlockIdx++
			writeAnthropicEvent(&buf, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         e.textBlockIdx,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			})
		}
		writeAnthropicEvent(&buf, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": e.textBlockIdx,
			"delta": map[string]any{
				"type":     "thinking_delta",
				"thinking": chunk.ReasoningDelta,
			},
		})
	}

	// Done: close open blocks, emit message_delta + message_stop.
	if chunk.Done {
		if e.textBlockIdx >= 0 {
			writeAnthropicEvent(&buf, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": e.textBlockIdx,
			})
		}
		for _, blockIdx := range e.toolBlockMap {
			writeAnthropicEvent(&buf, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": blockIdx,
			})
		}
		var outputTokens int
		if chunk.Usage != nil && chunk.Usage.CompletionTokens != nil {
			outputTokens = *chunk.Usage.CompletionTokens
		}
		writeAnthropicEvent(&buf, "message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": outputTokens},
		})
		writeAnthropicEvent(&buf, "message_stop", map[string]any{"type": "message_stop"})
	}

	if buf.Len() == 0 {
		return nil, nil
	}
	return buf.Bytes(), nil
}

// geminiStreamEncoder converts canonical chunks to Gemini streamGenerateContent
// SSE format. Vertex ingress uses the same wire shape, so this encoder serves
// both FormatGemini and FormatVertex.
//
// Each text delta becomes a candidate part; tool calls become functionCall parts.
// The Done chunk carries finishReason="STOP" and usageMetadata.
type geminiStreamEncoder struct{}

func (e *geminiStreamEncoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	if chunk.Done {
		candidate := map[string]any{
			"content":      map[string]any{"parts": []any{}, "role": "model"},
			"finishReason": "STOP",
			"index":        0,
		}
		resp := map[string]any{"candidates": []any{candidate}}
		if u := buildGeminiUsage(chunk.Usage); u != nil {
			resp["usageMetadata"] = u
		}
		return geminiSSEFrame(resp), nil
	}

	var parts []any
	if chunk.Delta != "" {
		parts = append(parts, map[string]any{"text": chunk.Delta})
	}
	for _, tc := range chunk.ToolCallDeltas {
		// Gemini sends function calls as complete (not streamed arguments),
		// so only emit when we have an ID or name (i.e. start of call).
		if tc.Name != "" {
			var args map[string]any
			if tc.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Arguments), &args)
			}
			if args == nil {
				args = map[string]any{}
			}
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{
					"id":   tc.ID,
					"name": tc.Name,
					"args": args,
				},
			})
		}
	}
	if chunk.ReasoningDelta != "" {
		// Tag reasoning as a Gemini thought part (`thought:true`) to match the
		// non-stream egress (spec_gemini/ingress/hub_ingress.go) AND to keep the
		// stream symmetric with the gemini stream DECODER, which routes
		// thought:true parts back to ReasoningDelta. Without the tag, a
		// cross-format reasoning delta (DeepSeek/OpenAI reasoning_content,
		// Anthropic thinking) leaks into the visible answer text instead of the
		// reasoning channel, and a gemini→…→gemini round-trip loses the thought
		// classification.
		parts = append(parts, map[string]any{"text": chunk.ReasoningDelta, "thought": true})
	}
	if len(parts) == 0 {
		return nil, nil
	}
	candidate := map[string]any{
		"content": map[string]any{"parts": parts, "role": "model"},
		"index":   0,
	}
	resp := map[string]any{"candidates": []any{candidate}}
	return geminiSSEFrame(resp), nil
}

// cohereStreamEncoder converts canonical chunks to Cohere Chat v2 streaming
// event format (message-start → content-start → content-delta → content-end
// → message-end).
type cohereStreamEncoder struct {
	headerSent    bool
	contentOpened bool
}

func (e *cohereStreamEncoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	var buf bytes.Buffer

	if !e.headerSent {
		e.headerSent = true
		writeCohereEvent(&buf, map[string]any{
			"type": "message-start",
			"id":   "transcoded",
			"delta": map[string]any{
				"message": map[string]any{"role": "assistant"},
			},
		})
	}

	if chunk.Delta != "" {
		if !e.contentOpened {
			e.contentOpened = true
			writeCohereEvent(&buf, map[string]any{
				"type":  "content-start",
				"index": 0,
				"delta": map[string]any{
					"message": map[string]any{
						"content": map[string]any{"type": "text"},
					},
				},
			})
		}
		writeCohereEvent(&buf, map[string]any{
			"type":  "content-delta",
			"index": 0,
			"delta": map[string]any{
				"message": map[string]any{
					"content": map[string]any{"text": chunk.Delta},
				},
			},
		})
	}

	for _, tc := range chunk.ToolCallDeltas {
		if tc.Name != "" {
			writeCohereEvent(&buf, map[string]any{
				"type":  "tool-call-start",
				"index": tc.Index,
				"delta": map[string]any{
					"message": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"id":   tc.ID,
								"type": "function",
								"function": map[string]any{
									"name":      tc.Name,
									"arguments": tc.Arguments,
								},
							},
						},
					},
				},
			})
		} else if tc.Arguments != "" {
			writeCohereEvent(&buf, map[string]any{
				"type":  "tool-call-delta",
				"index": tc.Index,
				"delta": map[string]any{
					"message": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"function": map[string]any{"arguments": tc.Arguments},
							},
						},
					},
				},
			})
		}
	}

	if chunk.Done {
		if e.contentOpened {
			writeCohereEvent(&buf, map[string]any{
				"type":  "content-end",
				"index": 0,
			})
		}
		msgEnd := map[string]any{
			"type":  "message-end",
			"delta": map[string]any{"finish_reason": "COMPLETE"},
		}
		if chunk.Usage != nil {
			tokens := map[string]any{}
			if chunk.Usage.PromptTokens != nil {
				tokens["input_tokens"] = *chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens != nil {
				tokens["output_tokens"] = *chunk.Usage.CompletionTokens
			}
			msgEnd["usage"] = map[string]any{"tokens": tokens}
		}
		writeCohereEvent(&buf, msgEnd)
	}

	if buf.Len() == 0 {
		return nil, nil
	}
	return buf.Bytes(), nil
}

// replicateStreamEncoder converts canonical chunks to Replicate SSE output
// events (event: output + event: done). Replicate output data is plain text,
// not JSON.
type replicateStreamEncoder struct{}

func (e *replicateStreamEncoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	if chunk.Done {
		return []byte("event: done\ndata: {}\n\n"), nil
	}
	if chunk.Delta != "" {
		// Replicate output event data is the raw token text — no JSON wrapping.
		return fmt.Appendf(nil, "event: output\ndata: %s\n\n", chunk.Delta), nil
	}
	return nil, nil
}

// --- shared helpers ---

func writeAnthropicEvent(buf *bytes.Buffer, event string, payload map[string]any) {
	data, _ := json.Marshal(payload)
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\ndata: ")
	buf.Write(data)
	buf.WriteString("\n\n")
}

func writeCohereEvent(buf *bytes.Buffer, payload map[string]any) {
	data, _ := json.Marshal(payload)
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
}

func geminiSSEFrame(payload map[string]any) []byte {
	data, _ := json.Marshal(payload)
	return fmt.Appendf(nil, "data: %s\n\n", data)
}

func buildGeminiUsage(u *provcore.Usage) map[string]any {
	if u == nil {
		return nil
	}
	out := map[string]any{}
	if u.PromptTokens != nil {
		out["promptTokenCount"] = *u.PromptTokens
	}
	if u.CompletionTokens != nil {
		out["candidatesTokenCount"] = *u.CompletionTokens
	}
	if u.TotalTokens != nil {
		out["totalTokenCount"] = *u.TotalTokens
	}
	// Cache + reasoning fields — mirror the non-stream egress translation
	// in spec_gemini/hub_ingress.go OpenAIChatCompletionToGenerateContentResponse.
	// These must be present in the streaming /v1beta egress path too;
	// omitting them causes cross-format callers (e.g. /v1beta SSE → claude
	// target) to see no cachedContentTokenCount in the final usage SSE frame
	// and incorrectly classify the response as a cache miss.
	if u.CacheReadTokens != nil && *u.CacheReadTokens > 0 {
		out["cachedContentTokenCount"] = *u.CacheReadTokens
	}
	if u.ReasoningTokens != nil && *u.ReasoningTokens > 0 {
		out["thoughtsTokenCount"] = *u.ReasoningTokens
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func oaiDeltaSSE(payload map[string]any) []byte {
	data, _ := json.Marshal(payload)
	return fmt.Appendf(nil, "data: %s\n\n", data)
}

func (e *openAIStreamEncoder) oaiToolCallEnvelope(deltas []provcore.ToolCallDelta) []byte {
	return oaiDeltaSSE(map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{"tool_calls": buildToolCallDeltas(deltas)},
				"finish_reason": nil,
			},
		},
	})
}

func buildToolCallDeltas(deltas []provcore.ToolCallDelta) []any {
	type fnCall struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	}
	type tcDelta struct {
		Index    int    `json:"index"`
		ID       string `json:"id,omitempty"`
		Type     string `json:"type,omitempty"`
		Function fnCall `json:"function"`
	}
	calls := make([]any, 0, len(deltas))
	for _, d := range deltas {
		tc := tcDelta{
			Index:    d.Index,
			Function: fnCall{Name: d.Name, Arguments: d.Arguments},
		}
		if d.ID != "" {
			tc.ID = d.ID
			tc.Type = "function"
		}
		calls = append(calls, tc)
	}
	return calls
}
