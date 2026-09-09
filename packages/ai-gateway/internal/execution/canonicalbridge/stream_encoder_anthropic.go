package canonicalbridge

import (
	"bytes"
	"context"
	"strings"

	"github.com/goccy/go-json"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	anthropicingress "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/ingress"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// stream_encoder_anthropic.go — the canonical→Anthropic Messages SSE encoder.

// Emits, statefully across Write calls:
//
//	message_start → content_block_start → content_block_delta × N
//	→ content_block_stop → message_delta → message_stop
type anthropicStreamEncoder struct {
	headerSent bool

	// Only one content block may be active at a time. A canonical buffered
	// chunk can contain several logical blocks (thinking, text, and tools), so
	// closeActiveBlock enforces Anthropic's start → delta(s) → stop lifecycle
	// when the next block begins.
	activeBlockIdx  int
	activeBlockKind anthropicBlockKind
	activeBlockKey  int
	nextBlockIdx    int
	// Latest non-nil usage on ANY chunk: OpenAI-family upstreams send usage in
	// its own final chunk, which need not be the Done chunk. Reading Done alone
	// answered message_start{input_tokens:0,output_tokens:0} and
	// message_delta{output_tokens:0} for a request the non-streaming arm
	// reported as 2717 input tokens — a client billing off the stream saw a
	// free request.
	lastUsage *normalize.Usage
	// pendingReasoning buffers universal (unsigned) reasoning text until it is
	// known whether a signed carrier will supersede it, and sawSignedThinking
	// records that one did.
	//
	// Both are needed because the same thinking text reaches this encoder twice
	// on the same-format path: the Anthropic decoder puts each thinking_delta on
	// ReasoningDelta AND, when signature_delta closes the block, republishes the
	// whole block through NexusThinking. Emitting both would send the reasoning
	// twice. Emitting neither — which is what this encoder used to do for
	// unsigned text — drops the reasoning entirely on a cross-format turn, where
	// no signature ever arrives.
	pendingReasoning  strings.Builder
	sawSignedThinking bool
}

// flushUnsignedReasoning emits buffered universal reasoning as an Anthropic
// thinking block, unless a signed carrier already carried it.
//
// The block is UNSIGNED, matching what the non-streaming egress already does
// (specs/anthropic/ingress/hub_ingress.go): an Anthropic-shaped client that
// cross-routed to gpt-5 or DeepSeek still sees the model's reasoning. That is a
// different act from forging a signature on the REQUEST leg, which the request
// codec rightly refuses — a fabricated signature would fail Anthropic's own
// verification when the client replays the turn upstream. Here the block is
// going to a client, and it carries no signature to be believed.
//
// Called at the point the stream reveals which case it is: Anthropic finishes a
// thinking block (and its signature_delta) before any text or tool block opens,
// so by the time non-reasoning content arrives, a signed carrier either came or
// never will.
func (e *anthropicStreamEncoder) flushUnsignedReasoning(buf *bytes.Buffer) {
	if e.sawSignedThinking || e.pendingReasoning.Len() == 0 {
		e.pendingReasoning.Reset()
		return
	}
	text := e.pendingReasoning.String()
	e.pendingReasoning.Reset()
	idx := e.ensureBlock(buf, anthropicBlockThinking, -1, map[string]any{"type": "thinking", "thinking": ""})
	writeAnthropicEvent(buf, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

type anthropicBlockKind uint8

const (
	anthropicBlockNone anthropicBlockKind = iota
	anthropicBlockText
	anthropicBlockTool
	anthropicBlockThinking
)

// Usage arrives once, near the end, and every event after it must report it.
func (e *anthropicStreamEncoder) rememberUsage(u *normalize.Usage) {
	if u != nil {
		e.lastUsage = u
	}
}

// The conversion belongs to the Anthropic ingress package, which the
// non-streaming egress also calls: one unit convention, one implementation.
func (e *anthropicStreamEncoder) anthropicUsage() map[string]any {
	out := map[string]any{"input_tokens": 0, "output_tokens": 0}
	u := e.lastUsage
	if u == nil {
		return out
	}
	var prompt, cacheRead, cacheCreation int64
	if u.PromptTokens != nil {
		prompt = int64(*u.PromptTokens)
	}
	if u.CacheReadTokens != nil {
		cacheRead = int64(*u.CacheReadTokens)
	}
	if u.CacheCreationTokens != nil {
		cacheCreation = int64(*u.CacheCreationTokens)
	}
	c := anthropicingress.ToAnthropicCounters(prompt, cacheRead, cacheCreation)
	if c.Contradictory {
		// The transcoding encoder does not carry the model name, so the arm
		// is the key.
		anthropicingress.WarnContradictoryUsage("(anthropic streaming transcode)",
			prompt, cacheRead, cacheCreation)
	}
	out["input_tokens"] = c.InputTokens
	if u.CompletionTokens != nil {
		out["output_tokens"] = int64(*u.CompletionTokens)
	}
	if c.CacheReadTokens > 0 {
		out["cache_read_input_tokens"] = c.CacheReadTokens
	}
	if c.CacheCreationTokens > 0 {
		out["cache_creation_input_tokens"] = c.CacheCreationTokens
	}
	return out
}

func newAnthropicStreamEncoder() *anthropicStreamEncoder {
	return &anthropicStreamEncoder{activeBlockIdx: -1, activeBlockKey: -1}
}

// closeActiveBlock emits the stop marker for the current content block, if
// any, and clears state so a later block cannot reuse its index.
func (e *anthropicStreamEncoder) closeActiveBlock(buf *bytes.Buffer) {
	if e.activeBlockKind == anthropicBlockNone {
		return
	}
	writeAnthropicEvent(buf, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": e.activeBlockIdx,
	})
	e.activeBlockIdx = -1
	e.activeBlockKind = anthropicBlockNone
	e.activeBlockKey = -1
}

// ensureBlock makes kind/key the only active content block, closing the
// previous block first when a buffered chunk switches content types or tool
// slots. A matching active block remains open across Write calls so streamed
// deltas for one logical block stay together.
func (e *anthropicStreamEncoder) ensureBlock(buf *bytes.Buffer, kind anthropicBlockKind, key int, content map[string]any) int {
	if e.activeBlockKind == kind && e.activeBlockKey == key {
		return e.activeBlockIdx
	}
	e.closeActiveBlock(buf)
	idx := e.nextBlockIdx
	e.nextBlockIdx++
	e.activeBlockIdx = idx
	e.activeBlockKind = kind
	e.activeBlockKey = key
	writeAnthropicEvent(buf, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": content,
	})
	return idx
}

func (e *anthropicStreamEncoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	var buf bytes.Buffer
	e.rememberUsage(chunk.Usage)

	// Emit message_start before the first content.
	if !e.headerSent {
		e.headerSent = true
		// Nothing yet for an OpenAI-family upstream; message_delta below
		// carries the authoritative counts, as a real upstream does.
		startUsage := e.anthropicUsage()
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
				"usage":         startUsage,
			},
		})
		writeAnthropicEvent(&buf, "ping", map[string]any{"type": "ping"})
	}

	if chunk.ReasoningDelta != "" {
		e.pendingReasoning.WriteString(chunk.ReasoningDelta)
	}
	if len(chunk.NexusThinking) > 0 {
		// The signed carrier republishes the same text this stream already put
		// on ReasoningDelta, so the buffer is dropped rather than emitted.
		e.sawSignedThinking = true
		e.pendingReasoning.Reset()
	}

	// Provider-native thinking is replayed from the signed opaque carrier; the
	// unsigned buffer above covers the cross-format turn where none arrives.
	for _, block := range chunk.NexusThinking {
		if block.RedactedData != "" {
			// Redacted blocks are complete in one chunk. Close any preceding
			// block first and stop before the next block begins.
			e.closeActiveBlock(&buf)
			idx := e.nextBlockIdx
			e.nextBlockIdx++
			writeAnthropicEvent(&buf, "content_block_start", map[string]any{
				"type": "content_block_start", "index": idx,
				"content_block": map[string]any{"type": "redacted_thinking", "data": block.RedactedData},
			})
			writeAnthropicEvent(&buf, "content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
			continue
		}
		idx := e.ensureBlock(&buf, anthropicBlockThinking, block.Index, map[string]any{"type": "thinking", "thinking": ""})
		if block.Thinking != "" {
			writeAnthropicEvent(&buf, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": idx,
				"delta": map[string]any{"type": "thinking_delta", "thinking": block.Thinking},
			})
		}
		writeAnthropicEvent(&buf, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "signature_delta", "signature": block.Signature},
		})
		// A signed carrier is a complete thinking block. This closes it before
		// any text or tool block in the same synthetic chunk starts.
		e.closeActiveBlock(&buf)
	}

	// Text follows provider-native thinking so buffered replay preserves the
	// Anthropic assistant block order.
	// Any unsigned reasoning must land before the first text block: Anthropic
	// orders thinking ahead of the answer, and a client rendering blocks in
	// arrival order would otherwise show the reasoning after it.
	if chunk.Delta != "" || chunk.RefusalDelta != "" || len(chunk.ToolCallDeltas) > 0 || chunk.Done {
		e.flushUnsignedReasoning(&buf)
	}

	// A refusal joins the text block: Anthropic has no refusal channel, and
	// dropping the decline would hand an Anthropic-shaped client an EMPTY
	// assistant turn where the model actually refused. Unlike thinking — which
	// this encoder deliberately will not synthesise without the provider's
	// signature — a refusal IS ordinary prose on this wire; only its
	// classification is lost, not its text.
	for _, text := range [...]string{chunk.Delta, chunk.RefusalDelta} {
		if text == "" {
			continue
		}
		idx := e.ensureBlock(&buf, anthropicBlockText, 0, map[string]any{"type": "text", "text": ""})
		writeAnthropicEvent(&buf, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	}

	// Tool calls follow text. Distinct tool slots are separate Anthropic
	// blocks, so ensureBlock closes one slot before opening the next.
	for _, d := range chunk.ToolCallDeltas {
		idx := e.ensureBlock(&buf, anthropicBlockTool, d.Index, map[string]any{
			"type":  "tool_use",
			"id":    d.ID,
			"name":  d.Name,
			"input": map[string]any{},
		})
		if d.Arguments != "" {
			writeAnthropicEvent(&buf, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": d.Arguments},
			})
		}
	}

	// Done: close open blocks, emit message_delta + message_stop.
	if chunk.Done {
		e.closeActiveBlock(&buf)
		// The full triple, not just output_tokens — captured from the
		// passthrough leg: message_delta usage {"input_tokens":11,
		// "cache_creation_input_tokens":0,"cache_read_input_tokens":2402,
		// "output_tokens":4}.
		writeAnthropicEvent(&buf, "message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": canonicalFinishToAnthropicStop(chunk.FinishReason), "stop_sequence": nil},
			"usage": e.anthropicUsage(),
		})
		writeAnthropicEvent(&buf, "message_stop", map[string]any{"type": "message_stop"})
	}

	if buf.Len() == 0 {
		return nil, nil
	}
	return buf.Bytes(), nil
}

func writeAnthropicEvent(buf *bytes.Buffer, event string, payload map[string]any) {
	data, _ := json.Marshal(payload)
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\ndata: ")
	buf.Write(data)
	buf.WriteString("\n\n")
}
