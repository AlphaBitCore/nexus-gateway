package proxy

// proxy_cache_buffer_accumulator.go — the canonical accumulation substrate for
// ai-gateway streaming BUFFER mode (split from proxy_cache_buffer.go along the
// "fold chunks → canonical body" seam). canonicalStreamAccumulator folds the
// canonical ChunkSubscription into a single canonical OpenAI chat-completion
// body the response pipeline redacts on; the bytes-tally + synthetic-chunk
// helpers bound and re-emit that accumulation. The delivery wrapper
// (runCanonicalBufferStream) and the wire forward-encode (emitCanonicalStream)
// stay in proxy_cache_buffer.go.

import (
	"strings"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// defaultCanonicalBufferMaxBytes bounds canonical-buffer accumulation when the
// admin streaming policy resolved no explicit cap (streamMaxBufferBytes == 0).
// Mirrors the shared streaming BufferConfig default (8MB).
const defaultCanonicalBufferMaxBytes = 8 * 1024 * 1024

// canonicalChunkSize estimates the canonical content bytes a chunk contributes
// to the buffered accumulation — assistant text, reasoning text, and tool-call
// id/name/arguments. Used to bound buffer-mode memory; it need not be exact,
// only monotonic in the real payload size.
func canonicalChunkSize(c provcore.Chunk) int {
	n := len(c.Delta) + len(c.ReasoningDelta)
	for _, d := range c.ToolCallDeltas {
		n += len(d.ID) + len(d.Name) + len(d.Arguments)
	}
	return n
}

// canonicalStreamAccumulator folds canonical provider chunks into a single
// canonical OpenAI chat-completion body for the buffer-mode response pipeline.
// It is per-call state (one instance per stream), so concurrent buffered streams
// never share accumulation — no package global, race-free under -race.
type canonicalStreamAccumulator struct {
	model     string
	content   strings.Builder
	reasoning strings.Builder
	toolOrder []int
	tools     map[int]*toolCallAccum
	// finishReason holds the last non-empty canonical finish_reason observed
	// across the chunk timeline (decoders stamp it on the frame that carries
	// the wire's stop token — often NOT the terminal Done frame). Re-emitted
	// on the synthetic terminal chunk so buffer mode preserves the real value
	// instead of collapsing to "stop"; empty until the stream reports one.
	finishReason string
}

// toolCallAccum accumulates the streamed deltas of one tool call slot. The wire
// `arguments` JSON arrives fragmented across ToolCallDelta.Arguments; it is
// concatenated verbatim and lands as the canonical message.tool_calls[].function
// .arguments string the OpenAI rewriter masks (BUG-toolcall canonical path).
type toolCallAccum struct {
	id   string
	name string
	args strings.Builder
}

func newCanonicalStreamAccumulator(model string) *canonicalStreamAccumulator {
	return &canonicalStreamAccumulator{model: model, tools: map[int]*toolCallAccum{}}
}

// add merges one canonical chunk's deltas into the accumulation.
//
// The tool-call-delta merge here (id/name set-once, arguments concatenated by
// slot Index) is the SAME contract the provider stream decoders own when they
// fold fragmented wire frames into canonical provcore.ToolCallDelta values
// (e.g. openai/stream/stream.go, anthropic/stream/stream.go). Keep the two in
// lockstep: a change to how a decoder fragments tool-call arguments must be
// reflected here, or buffer-mode tool-arg redaction will reassemble wrong.
func (a *canonicalStreamAccumulator) add(c provcore.Chunk) {
	a.content.WriteString(c.Delta)
	a.reasoning.WriteString(c.ReasoningDelta)
	if c.FinishReason != "" {
		a.finishReason = c.FinishReason
	}
	for _, d := range c.ToolCallDeltas {
		t := a.tools[d.Index]
		if t == nil {
			t = &toolCallAccum{}
			a.tools[d.Index] = t
			a.toolOrder = append(a.toolOrder, d.Index)
		}
		if d.ID != "" {
			t.id = d.ID
		}
		if d.Name != "" {
			t.name = d.Name
		}
		t.args.WriteString(d.Arguments)
	}
}

// canonicalBody renders the accumulation as a canonical OpenAI chat-completion
// JSON body — the exact shape extractChatResponse / RewriteResponseBody expect
// (choices[0].message.content + message.tool_calls[].function.arguments). The
// reasoning channel is intentionally omitted: extractChatResponse does not scan
// reasoning_content, so the non-stream canonical path never redacts it; the
// buffer path preserves the same coverage and re-emits reasoning unredacted.
//
// finish_reason carries the real observed value (defaulting to "stop" when the
// stream never reported one) so the response-hook input — extractResponseForHooks
// reads choices.0.finish_reason — sees the true terminal reason, not a fabricated
// "stop".
func (a *canonicalStreamAccumulator) canonicalBody() []byte {
	msg := map[string]any{
		"role":    "assistant",
		"content": a.content.String(),
	}
	if len(a.toolOrder) > 0 {
		calls := make([]any, 0, len(a.toolOrder))
		for _, idx := range a.toolOrder {
			t := a.tools[idx]
			args := t.args.String()
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":   t.id,
				"type": "function",
				"function": map[string]any{
					"name":      t.name,
					"arguments": args,
				},
			})
		}
		msg["tool_calls"] = calls
	}
	finish := a.finishReason
	if finish == "" {
		finish = "stop"
	}
	body := map[string]any{
		"id":     "chatcmpl-buffer",
		"object": "chat.completion",
		"model":  a.model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msg,
				"finish_reason": finish,
			},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

// syntheticChunkFromCanonical decomposes a (redacted) canonical chat-completion
// body back into a single canonical chunk for forward-encoding to the wire. The
// masked content + masked tool-call arguments are read back from the rewritten
// body; reasoning is carried through unchanged (it was never scanned, mirroring
// the non-stream path). One synthetic chunk is sufficient: buffer mode delivers
// the whole response after the end-of-stream checkpoint, and every stream
// encoder produces a complete, valid wire stream from a single combined chunk.
func syntheticChunkFromCanonical(body []byte, reasoning string) provcore.Chunk {
	msg := gjson.GetBytes(body, "choices.0.message")
	ch := provcore.Chunk{
		Delta:          msg.Get("content").String(),
		ReasoningDelta: reasoning,
		FinishReason:   gjson.GetBytes(body, "choices.0.finish_reason").String(),
	}
	msg.Get("tool_calls").ForEach(func(i, call gjson.Result) bool {
		ch.ToolCallDeltas = append(ch.ToolCallDeltas, provcore.ToolCallDelta{
			Index:     int(i.Int()),
			ID:        call.Get("id").String(),
			Name:      call.Get("function.name").String(),
			Arguments: call.Get("function.arguments").String(),
		})
		return true
	})
	return ch
}

// usageHasAny reports whether any usage field was populated, so the buffer path
// can pass a nil *Usage (no usage frame) when the provider emitted none —
// matching the non-stream / live behaviour of not synthesising an empty usage.
func usageHasAny(u provcore.Usage) bool {
	return u.PromptTokens != nil || u.CompletionTokens != nil || u.TotalTokens != nil ||
		u.CacheReadTokens != nil || u.CacheCreationTokens != nil || u.ReasoningTokens != nil
}
