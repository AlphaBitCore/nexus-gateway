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
//   - EndpointResponsesAPI → responsesStreamSession (Responses-API SSE grammar)
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
		return &responsesStreamSession{
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
	if err := ctx.Err(); err != nil {
		return provcore.Chunk{}, err
	}

	ev, err := s.scanner.Next()
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
		return s.Next(ctx)
	}

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
	}

	if usage := gjson.GetBytes(ev.Data, "usage"); usage.Exists() {
		// Streaming Usage extraction via provcore.ExtractUsage (same parser
		// path as the non-streaming codec, compliance proxy, agent, and Hub
		// audit). The full SSE chunk JSON is passed; shared/normalize finds the
		// usage block and applies the canonical alias chain (Kimi flat /
		// DeepSeek / Moonshot / OpenAI Responses-shape fallbacks).
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

	return chunk, nil
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
