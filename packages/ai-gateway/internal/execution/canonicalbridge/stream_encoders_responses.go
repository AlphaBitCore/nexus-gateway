package canonicalbridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/goccy/go-json"
	"sync/atomic"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// responsesStreamEncoder converts canonical Chunks (the in-process SSE
// shape every adapter emits) into the OpenAI Responses-API SSE event
// grammar. Used by canonicalbridge on the cross-format streaming path
// when ingress is FormatOpenAIResponses but the routed target speaks a
// different wire format (Anthropic, Gemini, ...).
//
// State machine:
//
//	INIT
//	  on first chunk (any non-empty kind):
//	    emit response.created  (response={id, status:in_progress, model})
//	    emit response.in_progress
//	    transition → ITEM_NONE
//	ITEM_NONE
//	  on ReasoningDelta:
//	    emit response.output_item.added (item={type:"reasoning"})
//	    emit response.reasoning_summary_part.added
//	    emit response.reasoning_summary_text.delta
//	    transition → REASONING_OPEN
//	  on Delta:
//	    emit response.output_item.added (item={type:"message"})
//	    emit response.content_part.added (part={type:"output_text"})
//	    emit response.output_text.delta
//	    transition → MESSAGE_OPEN
//	  on ToolCallDelta:
//	    emit response.output_item.added (item={type:"function_call"})
//	    emit response.function_call_arguments.delta
//	    transition → FCALL_OPEN
//	REASONING_OPEN / MESSAGE_OPEN / FCALL_OPEN
//	  on matching kind: emit corresponding .delta event
//	  on switch (kind change): close the current item, transition through
//	                           ITEM_NONE, then re-enter the new state
//	  on Done: close current item, transition → DONE
//	DONE
//	  on Done chunk: emit response.completed (or response.incomplete)
//
// `sequence_number` is monotonic across the whole stream; counter held on
// the encoder state.
type responsesStreamEncoder struct {
	id      string
	created int64
	model   string

	seq         atomic.Int64
	headerSent  bool
	currentItem string // "" | "message" | "reasoning" | "function_call"
	outputIndex int    // index of the open output item (incremented per item)

	// Per-item bookkeeping. For a "message" item we track content_index
	// for output_text parts; for "function_call" we track per-tool indices.
	messageContentIndex int
	functionCallByIndex map[int]int // ToolCallDelta.Index → outputIndex assigned
}

func newResponsesStreamEncoder(model string) *responsesStreamEncoder {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return &responsesStreamEncoder{
		id:                  "resp_" + hex.EncodeToString(b),
		created:             time.Now().Unix(),
		model:               model,
		functionCallByIndex: make(map[int]int),
	}
}

func (e *responsesStreamEncoder) nextSeq() int64 {
	return e.seq.Add(1) - 1
}

func (e *responsesStreamEncoder) writeEvent(buf *bytes.Buffer, evType string, payload map[string]any) {
	// Stamp sequence_number on every event (OpenAI's SDK requires
	// monotonic seq across the whole stream).
	if _, ok := payload["sequence_number"]; !ok {
		payload["sequence_number"] = e.nextSeq()
	}
	if _, ok := payload["type"]; !ok {
		payload["type"] = evType
	}
	data, _ := json.Marshal(payload)
	buf.WriteString("event: ")
	buf.WriteString(evType)
	buf.WriteString("\ndata: ")
	buf.Write(data)
	buf.WriteString("\n\n")
}

func (e *responsesStreamEncoder) ensureHeader(buf *bytes.Buffer) {
	if e.headerSent {
		return
	}
	e.headerSent = true
	e.writeEvent(buf, "response.created", map[string]any{
		"response": map[string]any{
			"id":         e.id,
			"object":     "response",
			"status":     "in_progress",
			"model":      e.model,
			"created_at": e.created,
		},
	})
	e.writeEvent(buf, "response.in_progress", map[string]any{
		"response": map[string]any{
			"id":     e.id,
			"object": "response",
			"status": "in_progress",
			"model":  e.model,
		},
	})
}

// closeCurrentItem emits the "done" events for whatever item type is
// currently open and returns the encoder to ITEM_NONE.
func (e *responsesStreamEncoder) closeCurrentItem(buf *bytes.Buffer) {
	switch e.currentItem {
	case "message":
		e.writeEvent(buf, "response.content_part.done", map[string]any{
			"output_index":  e.outputIndex,
			"content_index": e.messageContentIndex,
		})
		e.writeEvent(buf, "response.output_item.done", map[string]any{
			"output_index": e.outputIndex,
			"item": map[string]any{
				"type":   "message",
				"id":     fmt.Sprintf("msg_%s_%d", e.id, e.outputIndex),
				"role":   "assistant",
				"status": "completed",
			},
		})
	case "reasoning":
		e.writeEvent(buf, "response.reasoning_summary_part.done", map[string]any{
			"output_index":  e.outputIndex,
			"summary_index": 0,
		})
		e.writeEvent(buf, "response.output_item.done", map[string]any{
			"output_index": e.outputIndex,
			"item": map[string]any{
				"type":   "reasoning",
				"id":     fmt.Sprintf("rs_%s_%d", e.id, e.outputIndex),
				"status": "completed",
			},
		})
	case "function_call":
		// function_call items: close every open call with .done events.
		for _, oIdx := range e.functionCallByIndex {
			e.writeEvent(buf, "response.function_call_arguments.done", map[string]any{
				"output_index": oIdx,
			})
			e.writeEvent(buf, "response.output_item.done", map[string]any{
				"output_index": oIdx,
				"item":         map[string]any{"type": "function_call", "status": "completed"},
			})
		}
		e.functionCallByIndex = make(map[int]int)
	}
	e.currentItem = ""
	e.messageContentIndex = 0
}

// openItem opens a new output item of the given type and emits the
// item.added (and content_part.added / summary_part.added) events.
// Advances outputIndex.
func (e *responsesStreamEncoder) openItem(buf *bytes.Buffer, itemType string) {
	e.outputIndex++
	e.currentItem = itemType
	switch itemType {
	case "message":
		e.writeEvent(buf, "response.output_item.added", map[string]any{
			"output_index": e.outputIndex,
			"item": map[string]any{
				"type":   "message",
				"id":     fmt.Sprintf("msg_%s_%d", e.id, e.outputIndex),
				"role":   "assistant",
				"status": "in_progress",
			},
		})
		e.writeEvent(buf, "response.content_part.added", map[string]any{
			"output_index":  e.outputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": ""},
		})
		e.messageContentIndex = 0
	case "reasoning":
		e.writeEvent(buf, "response.output_item.added", map[string]any{
			"output_index": e.outputIndex,
			"item": map[string]any{
				"type":   "reasoning",
				"id":     fmt.Sprintf("rs_%s_%d", e.id, e.outputIndex),
				"status": "in_progress",
			},
		})
		e.writeEvent(buf, "response.reasoning_summary_part.added", map[string]any{
			"output_index":  e.outputIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": ""},
		})
	}
}

// Write is the StreamTranscoder entry point. Translates a single
// canonical Chunk into 0+ Responses SSE events.
func (e *responsesStreamEncoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	var buf bytes.Buffer
	e.ensureHeader(&buf)

	// Reasoning delta: open / continue a reasoning item.
	if chunk.ReasoningDelta != "" {
		if e.currentItem != "reasoning" {
			if e.currentItem != "" {
				e.closeCurrentItem(&buf)
			}
			e.openItem(&buf, "reasoning")
		}
		e.writeEvent(&buf, "response.reasoning_summary_text.delta", map[string]any{
			"output_index":  e.outputIndex,
			"summary_index": 0,
			"delta":         chunk.ReasoningDelta,
		})
	}

	// Text delta: open / continue a message item.
	if chunk.Delta != "" {
		if e.currentItem != "message" {
			if e.currentItem != "" {
				e.closeCurrentItem(&buf)
			}
			e.openItem(&buf, "message")
		}
		e.writeEvent(&buf, "response.output_text.delta", map[string]any{
			"output_index":  e.outputIndex,
			"content_index": e.messageContentIndex,
			"delta":         chunk.Delta,
		})
	}

	// Tool call deltas: each tool call lives in its own function_call item
	// (Responses-API emits one output_item per tool call). We close any
	// currently-open non-function_call item before starting a function_call.
	if len(chunk.ToolCallDeltas) > 0 {
		if e.currentItem != "function_call" && e.currentItem != "" {
			e.closeCurrentItem(&buf)
		}
		if e.currentItem != "function_call" {
			e.currentItem = "function_call"
		}
		for _, tc := range chunk.ToolCallDeltas {
			oIdx, opened := e.functionCallByIndex[tc.Index]
			if !opened {
				e.outputIndex++
				oIdx = e.outputIndex
				e.functionCallByIndex[tc.Index] = oIdx
				e.writeEvent(&buf, "response.output_item.added", map[string]any{
					"output_index": oIdx,
					"item": map[string]any{
						"type":      "function_call",
						"id":        fmt.Sprintf("fc_%s_%d", e.id, oIdx),
						"call_id":   tc.ID,
						"name":      tc.Name,
						"arguments": "",
						"status":    "in_progress",
					},
				})
			}
			if tc.Arguments != "" {
				e.writeEvent(&buf, "response.function_call_arguments.delta", map[string]any{
					"output_index": oIdx,
					"delta":        tc.Arguments,
				})
			}
		}
	}

	// Done chunk: close current item + emit terminal event.
	if chunk.Done {
		if e.currentItem != "" {
			e.closeCurrentItem(&buf)
		}
		terminalEvent, terminalStatus := "response.completed", "completed"
		// Map the canonical finish_reason to the Responses terminal event:
		// length / content_filter become response.incomplete with an
		// incomplete_details.reason so a re-encoder (buffer mode) preserves the
		// real terminal status instead of always reporting completed. Empty /
		// stop / tool_calls stay completed.
		var incompleteReason string
		switch chunk.FinishReason {
		case "length":
			terminalEvent, terminalStatus, incompleteReason = "response.incomplete", "incomplete", "max_output_tokens"
		case "content_filter":
			terminalEvent, terminalStatus, incompleteReason = "response.incomplete", "incomplete", "content_filter"
		}
		respPayload := map[string]any{
			"id":     e.id,
			"object": "response",
			"status": terminalStatus,
			"model":  e.model,
		}
		if incompleteReason != "" {
			respPayload["incomplete_details"] = map[string]any{"reason": incompleteReason}
		}
		if chunk.Usage != nil {
			usage := map[string]any{}
			if chunk.Usage.PromptTokens != nil {
				usage["input_tokens"] = *chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens != nil {
				usage["output_tokens"] = *chunk.Usage.CompletionTokens
			}
			if chunk.Usage.TotalTokens != nil {
				usage["total_tokens"] = *chunk.Usage.TotalTokens
			}
			if chunk.Usage.CacheReadTokens != nil {
				usage["input_tokens_details"] = map[string]any{"cached_tokens": *chunk.Usage.CacheReadTokens}
			}
			if chunk.Usage.ReasoningTokens != nil {
				usage["output_tokens_details"] = map[string]any{"reasoning_tokens": *chunk.Usage.ReasoningTokens}
			}
			respPayload["usage"] = usage
		}
		e.writeEvent(&buf, terminalEvent, map[string]any{"response": respPayload})
	}

	if buf.Len() == 0 {
		return nil, nil
	}
	return buf.Bytes(), nil
}
