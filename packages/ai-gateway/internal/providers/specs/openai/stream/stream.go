package stream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// StreamDecoder parses OpenAI-format SSE streams (shape also used by
// DeepSeek, Moonshot, SiliconFlow, Together, Groq, OpenRouter, Fireworks,
// Ollama, GLM's OpenAI-compat endpoint, and Azure OpenAI deployments).
type StreamDecoder struct {
	log *slog.Logger
}

// NewStreamDecoder returns a StreamDecoder.
func NewStreamDecoder(log *slog.Logger) *StreamDecoder {
	if log == nil {
		log = slog.Default()
	}
	return &StreamDecoder{log: log}
}

// Open wraps body in the right session for the endpoint:
//   - EndpointResponsesAPI → responsesEgressSession (content-detecting: forwards
//     genuine Responses frames verbatim, decodes a chat.completion upstream into
//     canonical so the proxy re-shapes it to Responses events — never leaks the
//     wrong wire shape to a /v1/responses client)
//   - everything else      → openaiStreamSession (chat-completions SSE)
//
// The dispatch is necessary because /v1/responses sends a completely different
// SSE event shape (event: response.output_text.delta + nested `delta` field)
// vs /v1/chat/completions (data: {choices:[{delta}]}). Feeding Responses bytes
// into openaiStreamSession would silently parse no content.
func (d *StreamDecoder) Open(body io.ReadCloser, endpoint typology.WireShape) (provcore.StreamSession, error) {
	if body == nil {
		return nil, fmt.Errorf("openai: nil stream body")
	}
	if endpoint == typology.WireShapeOpenAIResponses {
		return &responsesEgressSession{
			scanner: specutil.NewSSEScanner(body),
			log:     d.log,
		}, nil
	}
	return &openaiStreamSession{
		scanner: specutil.NewSSEScanner(body),
		log:     d.log,
	}, nil
}

type openaiStreamSession struct {
	scanner *specutil.SSEScanner
	log     *slog.Logger
	done    bool
}

func (s *openaiStreamSession) Next(ctx context.Context) (provcore.Chunk, error) {
	if s.done {
		return provcore.Chunk{}, io.EOF
	}

	// Loop until a frame with a non-empty `data:` payload arrives. Empty
	// frames (SSE keep-alives / comments) are skipped via `continue`
	// rather than a tail-recursive `return s.Next(ctx)`: Go has no tail-call
	// optimisation, so a hostile or broken upstream flooding empty `data:`
	// frames would otherwise grow the goroutine stack without bound.
	var ev specutil.SSEEvent
	for {
		if err := ctx.Err(); err != nil {
			return provcore.Chunk{}, err
		}
		var err error
		ev, err = s.scanner.Next()
		if err != nil {
			return provcore.Chunk{}, err
		}
		if bytes.Equal(bytes.TrimSpace(ev.Data), []byte("[DONE]")) {
			s.done = true
			// Re-emit the canonical "data: [DONE]\n\n" SSE frame so the
			// handler can forward it verbatim to OpenAI-compat clients.
			return provcore.Chunk{
				Done:        true,
				RawBytes:    []byte("data: [DONE]\n\n"),
				NativeEvent: ev.Event,
			}, nil
		}
		if len(ev.Data) == 0 {
			continue
		}
		break
	}

	return chatChunkFromFrame(ev), nil
}

// chatChunkFromFrame decodes one non-empty OpenAI chat-completions SSE data
// frame into a canonical Chunk. Extracted so the /v1/responses content copier
// can reuse the exact chat-decode path when an upstream tagged for Responses
// actually returns chat.completion.chunk frames (the bulletproof egress).
func chatChunkFromFrame(ev specutil.SSEEvent) provcore.Chunk {
	chunk := provcore.Chunk{
		RawBytes:    formatSSE(ev.Event, ev.Data),
		NativeEvent: ev.Event,
	}

	choice0 := gjson.GetBytes(ev.Data, "choices.0")
	if choice0.Exists() {
		if c := choice0.Get("delta.content"); c.Exists() {
			chunk.Delta = c.String()
		}
		// reasoning_content carries chain-of-thought tokens emitted by
		// thinking models (DeepSeek-R1/V4, Kimi K2, etc.) before the final
		// answer. Route it to the dedicated ReasoningDelta channel — NOT
		// Delta — so it stays separate from the answer through the canonical
		// hub. Every cross-format stream encoder maps ReasoningDelta to the
		// target's reasoning channel (Gemini `thought:true`, Anthropic
		// `thinking_delta`, OpenAI `delta.reasoning_content`); appending to
		// Delta instead leaked the chain-of-thought into the visible answer on
		// every cross-format transcode. This matches the openai-responses
		// stream decoder, which already routes reasoning to ReasoningDelta.
		// Native openai-family passthrough is unaffected — it forwards the
		// upstream RawBytes (which still carry reasoning_content), not Delta.
		if rc := choice0.Get("delta.reasoning_content"); rc.Exists() && rc.String() != "" {
			chunk.ReasoningDelta += rc.String()
		}
		if tools := choice0.Get("delta.tool_calls"); tools.IsArray() {
			tools.ForEach(func(_, v gjson.Result) bool {
				chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, provcore.ToolCallDelta{
					Index:     int(v.Get("index").Int()),
					ID:        v.Get("id").String(),
					Name:      v.Get("function.name").String(),
					Arguments: v.Get("function.arguments").String(),
				})
				return true
			})
		}
		// finish_reason rides a trailing chunk (delta empty) and is already in
		// the canonical OpenAI vocabulary (stop / length / tool_calls /
		// content_filter). Surface it on the canonical Chunk so a re-encoder
		// (buffer mode) preserves the real value instead of collapsing to "stop".
		if fr := choice0.Get("finish_reason"); fr.Type == gjson.String && fr.Str != "" {
			chunk.FinishReason = fr.Str
		}
	}

	if usage := gjson.GetBytes(ev.Data, "usage"); usage.IsObject() {
		// Streaming Usage extraction via provcore.ExtractUsage (same parser
		// path as the non-streaming codec, compliance proxy, agent, and Hub
		// audit). The full SSE chunk JSON is passed; shared/normalize finds the
		// usage block and applies the canonical alias chain (Kimi flat /
		// DeepSeek / Moonshot / OpenAI Responses-shape fallbacks).
		//
		// Guard on IsObject (not Exists): OpenAI-stream chunks carry
		// `"usage": null` on EVERY non-final delta, and gjson reports an
		// explicit null as Exists()==true. Running the full normalizer on each
		// null-usage chunk re-ran Tier-1 confidence scoring + canonical assembly
		// per delta — the dominant streaming allocator. A real usage block is
		// always a JSON object, so IsObject() runs ExtractUsage only on the
		// final include_usage chunk while staying correct for every alias shape.
		u := provcore.ExtractUsage(ev.Data, provcore.FormatOpenAI)
		// Zero-value Usage means the alias chain didn't recognise the
		// shape — fall back to nil so downstream stamping treats it as
		// "not reported" instead of "reported zero".
		if u.PromptTokens != nil || u.CompletionTokens != nil ||
			u.TotalTokens != nil || u.CacheReadTokens != nil ||
			u.CacheCreationTokens != nil || u.ReasoningTokens != nil {
			chunk.Usage = &u
		}
	}

	return chunk
}

func (s *openaiStreamSession) Close() error {
	s.done = true
	return s.scanner.Close()
}

// formatSSE rebuilds the canonical SSE frame bytes from the parsed
// event + data pair so the Chunk.RawBytes can be forwarded to the
// client verbatim.
func formatSSE(event string, data []byte) []byte {
	buf := bytes.Buffer{}
	if event != "" {
		buf.WriteString("event: ")
		buf.WriteString(event)
		buf.WriteByte('\n')
	}
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return buf.Bytes()
}
