package cohere

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// StreamDecoder parses Cohere Chat v2 SSE streams. Event family:
//
//	message-start          {"type":"message-start","id":"...","delta":{"message":{"role":"assistant"}}}
//	content-start          {"type":"content-start","index":0,"delta":{"message":{"content":{"type":"text"}}}}
//	content-delta          {"type":"content-delta","index":0,"delta":{"message":{"content":{"text":"<delta>"}}}}
//	content-end            {"type":"content-end","index":0}
//	tool-plan-delta        {"type":"tool-plan-delta","delta":{"message":{"tool_plan":"<delta>"}}}
//	tool-call-start        {"type":"tool-call-start","index":N,"delta":{"message":{"tool_calls":[{...}]}}}
//	tool-call-delta        {"type":"tool-call-delta","index":N,"delta":{"message":{"tool_calls":[{"function":{"arguments":"<delta>"}}]}}}
//	tool-call-end          {"type":"tool-call-end","index":N}
//	message-end            {"type":"message-end","delta":{"finish_reason":"COMPLETE"},"usage":{"tokens":{...}}}
//
// The decoder maps each event to canonical OpenAI streaming chunk
// shape so the gateway client sees standard `data: {...}\n\n` SSE frames.
type StreamDecoder struct {
	log *slog.Logger
}

// NewStreamDecoder constructs a StreamDecoder.
func NewStreamDecoder(log *slog.Logger) *StreamDecoder {
	if log == nil {
		log = slog.Default()
	}
	return &StreamDecoder{log: log}
}

// Open wraps body in a streamSession.
func (d *StreamDecoder) Open(body io.ReadCloser, _ typology.WireShape) (provcore.StreamSession, error) {
	if body == nil {
		return nil, fmt.Errorf("cohere: nil stream body")
	}
	return &streamSession{
		scanner: specutil.NewSSEScanner(body),
		log:     d.log,
	}, nil
}

type streamSession struct {
	scanner *specutil.SSEScanner
	log     *slog.Logger
	done    bool
}

func (s *streamSession) Next(ctx context.Context) (provcore.Chunk, error) {
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

	// Cohere emits the `type` field inside the data payload; the SSE
	// `event:` header may or may not be present. Trust the JSON `type`.
	t := gjson.GetBytes(ev.Data, "type").Str
	chunk := provcore.Chunk{NativeEvent: t}

	switch t {
	case "content-delta":
		text := gjson.GetBytes(ev.Data, "delta.message.content.text").Str
		chunk.Delta = text
		chunk.RawBytes = canonicalDeltaSSE(map[string]any{
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{"content": text},
				},
			},
		})
	case "tool-plan-delta":
		text := gjson.GetBytes(ev.Data, "delta.message.tool_plan").Str
		chunk.ReasoningDelta = text
		chunk.RawBytes = canonicalDeltaSSE(map[string]any{
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{"reasoning_content": text},
				},
			},
		})
	case "tool-call-start", "tool-call-delta":
		idx := int(gjson.GetBytes(ev.Data, "index").Int())
		// Per Cohere, the delta.message.tool_calls is either an array
		// (start) or wraps the partial-arguments shape. Walk it.
		var oaiToolCalls []map[string]any
		gjson.GetBytes(ev.Data, "delta.message.tool_calls").ForEach(func(_, tc gjson.Result) bool {
			d := provcore.ToolCallDelta{
				Index:     idx,
				ID:        tc.Get("id").Str,
				Name:      tc.Get("function.name").Str,
				Arguments: tc.Get("function.arguments").Str,
			}
			chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, d)
			oaiTC := map[string]any{
				"index": idx,
			}
			if d.ID != "" {
				oaiTC["id"] = d.ID
			}
			fn := map[string]any{}
			if d.Name != "" {
				fn["name"] = d.Name
			}
			if d.Arguments != "" {
				fn["arguments"] = d.Arguments
			}
			if len(fn) > 0 {
				oaiTC["function"] = fn
				oaiTC["type"] = "function"
			}
			oaiToolCalls = append(oaiToolCalls, oaiTC)
			return true
		})
		chunk.RawBytes = canonicalDeltaSSE(map[string]any{
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": oaiToolCalls},
				},
			},
		})
	case "message-end":
		s.done = true
		finishReason := gjson.GetBytes(ev.Data, "delta.finish_reason").Str
		// Cohere finish_reason values: COMPLETE, MAX_TOKENS, STOP_SEQUENCE,
		// TOOL_CALL, ERROR. Normalise to OpenAI lowercase strings.
		finishReason = mapFinishReason(finishReason)
		usagePayload := gjson.GetBytes(ev.Data, "usage")
		var u *provcore.Usage
		if usagePayload.Exists() {
			u = &provcore.Usage{}
			if pt := usagePayload.Get("tokens.input_tokens"); pt.Exists() && pt.Type == gjson.Number {
				v := int(pt.Int())
				u.PromptTokens = &v
			}
			if ct := usagePayload.Get("tokens.output_tokens"); ct.Exists() && ct.Type == gjson.Number {
				v := int(ct.Int())
				u.CompletionTokens = &v
			}
			if u.PromptTokens != nil && u.CompletionTokens != nil {
				total := *u.PromptTokens + *u.CompletionTokens
				u.TotalTokens = &total
			}
		}
		chunk.Usage = u
		// Emit a final chunk carrying finish_reason + [DONE] sentinel.
		choice := map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finishReason,
		}
		body := map[string]any{"choices": []any{choice}}
		if u != nil {
			usageMap := map[string]any{}
			if u.PromptTokens != nil {
				usageMap["prompt_tokens"] = *u.PromptTokens
			}
			if u.CompletionTokens != nil {
				usageMap["completion_tokens"] = *u.CompletionTokens
			}
			if u.TotalTokens != nil {
				usageMap["total_tokens"] = *u.TotalTokens
			}
			body["usage"] = usageMap
		}
		chunk.Done = true
		chunk.RawBytes = append(canonicalDeltaSSE(body), []byte("data: [DONE]\n\n")...)
	default:
		// message-start, content-start, content-end, tool-call-end:
		// no canonical content. Emit raw bytes for audit but no Delta.
		chunk.RawBytes = canonicalDeltaSSE(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}}},
		})
	}

	return chunk, nil
}

func (s *streamSession) Close() error {
	s.done = true
	return s.scanner.Close()
}

// canonicalDeltaSSE wraps a JSON object as a single OpenAI-canonical
// SSE frame (`data: {...}\n\n`).
func canonicalDeltaSSE(payload map[string]any) []byte {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	buf := bytes.Buffer{}
	buf.WriteString("data: ")
	buf.Write(body)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

// mapFinishReason maps Cohere's finish_reason values to OpenAI's
// lowercase canonical set.
func mapFinishReason(cohere string) string {
	switch cohere {
	case "COMPLETE":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "STOP_SEQUENCE":
		return "stop"
	case "TOOL_CALL":
		return "tool_calls"
	case "ERROR":
		return "error"
	}
	return cohere
}
