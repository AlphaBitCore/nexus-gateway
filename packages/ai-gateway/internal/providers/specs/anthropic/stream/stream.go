package stream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	specerrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// StreamDecoder parses Anthropic's SSE stream. The provider emits
// `event: <name>\ndata: {...}` frames where the event name drives
// interpretation (content_block_delta is the only frame that carries
// text). We forward the raw SSE bytes so native clients keep parity
// and at the same time surface a canonical Chunk.Delta.
type StreamDecoder struct {
	log *slog.Logger
}

// NewStreamDecoder builds a StreamDecoder.
func NewStreamDecoder(log *slog.Logger) *StreamDecoder {
	if log == nil {
		log = slog.Default()
	}
	return &StreamDecoder{log: log}
}

// Open wraps body in an anthropicStreamSession.
func (d *StreamDecoder) Open(body io.ReadCloser, _ typology.WireShape) (provcore.StreamSession, error) {
	if body == nil {
		return nil, fmt.Errorf("anthropic: nil stream body")
	}
	return &anthropicStreamSession{scanner: specutil.NewSSEScanner(body), log: d.log}, nil
}

type anthropicStreamSession struct {
	scanner *specutil.SSEScanner
	log     *slog.Logger
	done    bool
	// tools maps Anthropic content-block index → tool header for streaming
	// tool_use argument deltas (input_json_delta).
	tools map[int]struct{ id, name string }
	// usage accumulates the token usage Anthropic reports across the stream:
	// input_tokens on message_start, the final output_tokens on message_delta.
	// Anthropic carries usage on message_delta (not the terminal message_stop),
	// but every canonical egress encoder reads chunk.Usage only on the Done chunk,
	// so we stamp the accumulated total onto message_stop — without it the OpenAI /
	// Responses egress drops the trailing usage frame (include_usage shows nothing).
	usage *provcore.Usage
}

func (s *anthropicStreamSession) Next(ctx context.Context) (provcore.Chunk, error) {
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
	chunk := provcore.Chunk{
		RawBytes:    formatSSE(ev.Event, ev.Data),
		NativeEvent: ev.Event,
	}
	switch ev.Event {
	case "message_start":
		// Per-event Usage extraction via shared/normcodecs.MergeAnthropicEventUsage.
		// Same canonical normalization (PromptTokens = uncached + cache_read +
		// cache_creation) as the non-streaming codec. Accumulate so the final
		// total can ride the terminal message_stop chunk.
		if u := normcodecs.MergeAnthropicEventUsage(s.usage, ev.Data); u != nil {
			s.usage = u
			chunk.Usage = u
		}
	case "content_block_start":
		idx := int(gjson.GetBytes(ev.Data, "index").Int())
		cb := gjson.GetBytes(ev.Data, "content_block")
		if cb.Get("type").String() == "tool_use" {
			if s.tools == nil {
				s.tools = make(map[int]struct{ id, name string })
			}
			s.tools[idx] = struct{ id, name string }{
				id:   cb.Get("id").String(),
				name: cb.Get("name").String(),
			}
			chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, provcore.ToolCallDelta{
				Index: idx,
				ID:    cb.Get("id").String(),
				Name:  cb.Get("name").String(),
			})
		}
	case "content_block_delta":
		idx := int(gjson.GetBytes(ev.Data, "index").Int())
		delta := gjson.GetBytes(ev.Data, "delta")
		switch delta.Get("type").String() {
		case "text_delta":
			chunk.Delta = delta.Get("text").String()
		case "thinking_delta":
			// Extended-thinking text. Surface on the canonical
			// reasoning channel so it stays separate from assistant
			// content for audit / hooks; native passthrough still
			// carries the original frame.
			chunk.ReasoningDelta = delta.Get("thinking").String()
		case "signature_delta":
			// Cryptographic signature for verifying the thinking
			// block; opaque to canonical consumers. Carry on the
			// reasoning channel so it isn't lost on Delta-aggregating
			// consumers (canonical text aggregation skips it).
			chunk.ReasoningDelta = delta.Get("signature").String()
		case "input_json_delta":
			if tc, ok := s.tools[idx]; ok {
				chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, provcore.ToolCallDelta{
					Index:     idx,
					ID:        tc.id,
					Name:      tc.name,
					Arguments: delta.Get("partial_json").String(),
				})
			}
		}
	case "content_block_stop":
		// Block lifecycle marker; clear any per-block tool state so a
		// later block reusing the same index doesn't merge into the
		// previous tool call. Decoder emits no canonical signal — the
		// outer message_stop is what actually terminates the stream.
		idx := int(gjson.GetBytes(ev.Data, "index").Int())
		delete(s.tools, idx)
	case "ping":
		// Keep-alive heartbeat; no canonical signal. Native passthrough
		// already forwards the frame via chunk.RawBytes.
	case "error":
		// Stream-level error event (overloaded_error, api_error, etc).
		// Surface as a typed ProviderError so callers see a real
		// failure instead of a "successful" stream end.
		s.done = true
		errObj := gjson.GetBytes(ev.Data, "error")
		etype := errObj.Get("type").String()
		emsg := errObj.Get("message").String()
		return provcore.Chunk{}, MapAnthropicStreamError(etype, emsg)
	case "message_delta":
		// Same helper as message_start; tolerates message_delta's root-level
		// `usage` (vs message_start's nested message.usage). Anthropic / Bedrock
		// / Vertex translation layers may consolidate the entire usage snapshot
		// onto message_delta — the helper handles every field present. Merge onto
		// the accumulated usage (message_start's input_tokens + this output_tokens).
		if u := normcodecs.MergeAnthropicEventUsage(s.usage, ev.Data); u != nil {
			s.usage = u
			chunk.Usage = u
		}
	case "message_stop":
		// Terminal frame. Stamp the accumulated usage onto the Done chunk so the
		// canonical egress encoders (OpenAI / Responses) emit the trailing usage
		// frame; Anthropic reports usage on message_delta, not here, so without
		// this the cross-format stream loses token counts entirely.
		chunk.Done = true
		chunk.Usage = s.usage
		s.done = true
	}
	return chunk, nil
}

func (s *anthropicStreamSession) Close() error {
	s.done = true
	return s.scanner.Close()
}

// MapAnthropicStreamError translates an Anthropic stream-level `error`
// event payload into a canonical [provcore.ProviderError]. It delegates the
// type→(code, status) mapping to the shared specerrors.MapErrorType so the
// streaming path and the unary HTTP normaliser can never diverge on the
// same upstream error class. Unrecognised / empty types fall
// back to upstream_error / 502.
func MapAnthropicStreamError(etype, emsg string) error {
	code, status, ok := specerrors.MapErrorType(etype)
	if !ok {
		code = provcore.CodeUpstreamError
		status = http.StatusBadGateway
	}
	if emsg == "" {
		emsg = "anthropic stream error"
	}
	return &provcore.ProviderError{
		Status:  status,
		Code:    code,
		Type:    etype,
		Message: emsg,
	}
}

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
