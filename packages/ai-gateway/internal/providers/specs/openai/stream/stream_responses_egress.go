// Package stream — stream_responses_egress.go is the response-side content
// detector for the /v1/responses ingress. The request may be sent upstream as
// Responses-shape (a target whose capability says it serves /v1/responses) yet
// the upstream can still answer with chat.completion.chunk frames (an
// OpenAI-compatible endpoint that only implements /v1/chat/completions, or a
// provider that mis-declares its capability). Trusting the declared Format
// would forward those chat frames straight to a /v1/responses client with no
// terminal event — the egress bug this detector exists to kill.
//
// The session performs exactly ONE classification, lazily on the first decoded
// SSE frame at the raw-byte boundary (reusing the shared SSEScanner buffer; one
// per-stream hold, zero per-chunk allocation):
//
//	first frame is Responses-shape (event: response.* / data {"type":"response.*"})
//	    → copier mode: forward every upstream frame VERBATIM (Chunk.Verbatim +
//	      RawBytes) so built-in-tool / audio events survive, AND decode the
//	      canonical Delta / tool-call / reasoning / usage fields onto the same
//	      chunk so the enforcement (buffer / Model A) lane — which ignores
//	      Verbatim and reads canonical — can still redact and account.
//	first frame is chat.completion.chunk, or unclassifiable
//	    → chat mode: decode each frame as canonical chat (fail CLOSED — never
//	      verbatim) so the proxy's Responses encoder re-shapes it into the
//	      response.* event grammar with a terminal response.completed.
package stream

import (
	"bytes"
	"context"
	"io"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/responses"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/tidwall/gjson"
)

// egressMode is the per-stream decode mode resolved from the first frame.
type egressMode int

const (
	egressModeUnresolved egressMode = iota
	egressModeCopier                // genuine Responses upstream → verbatim + canonical tee
	egressModeChat                  // chat upstream / unclassifiable → canonical chat decode (fail closed)
)

// responsesEgressSession is the content-sniffing session returned for a
// /v1/responses ingress. It owns one SSEScanner and resolves egressMode once.
type responsesEgressSession struct {
	scanner *specutil.SSEScanner
	log     *slog.Logger
	mode    egressMode
	done    bool
	// sawToolCall records whether any function_call_arguments.delta was seen so
	// the terminal response.completed reports finish_reason "tool_calls" (parity
	// with the chat decoder), which a downstream re-encoder preserves.
	sawToolCall bool
}

func (s *responsesEgressSession) Next(ctx context.Context) (provcore.Chunk, error) {
	if s.done {
		return provcore.Chunk{}, io.EOF
	}
	for {
		if err := ctx.Err(); err != nil {
			return provcore.Chunk{}, err
		}
		ev, err := s.scanner.Next()
		if err != nil {
			return provcore.Chunk{}, err
		}
		// Responses-API never sends "[DONE]"; chat-completions does. Match it
		// defensively for either wire and close the stream.
		if bytes.Equal(bytes.TrimSpace(ev.Data), []byte("[DONE]")) {
			s.done = true
			return provcore.Chunk{
				Done:        true,
				RawBytes:    []byte("data: [DONE]\n\n"),
				NativeEvent: ev.Event,
				Verbatim:    s.mode == egressModeCopier,
			}, nil
		}
		if len(ev.Data) == 0 {
			// Keep-alive / comment frame — skipped by the scanner contract; it
			// never participates in classification (fail-safe on garbage).
			continue
		}

		if s.mode == egressModeUnresolved {
			evType := ev.Event
			if evType == "" {
				evType = gjson.GetBytes(ev.Data, "type").String()
			}
			switch responses.ClassifyFirstSSEFrame(evType, ev.Data) {
			case responses.ClassResponses:
				s.mode = egressModeCopier
			default:
				// ClassChat AND ClassUnknown both fail closed to the canonical
				// chat lane — never verbatim. An unclassifiable first frame must
				// not leak the wrong wire shape; the Responses encoder downstream
				// produces a valid response.created → response.completed envelope.
				s.mode = egressModeChat
			}
		}

		if s.mode == egressModeCopier {
			chunk := s.copierChunk(ev)
			if chunk.Done {
				s.done = true
			}
			return chunk, nil
		}
		return chatChunkFromFrame(ev), nil
	}
}

func (s *responsesEgressSession) Close() error {
	s.done = true
	return s.scanner.Close()
}

// copierChunk turns one Responses-API SSE frame into a chunk that is BOTH a
// verbatim copy (RawBytes + Verbatim, so the non-enforced live relay forwards
// the original bytes with zero loss of built-in-tool / audio events) AND a
// canonical decode of the delta / tool-call / reasoning / usage fields (so the
// enforcement lane, which reads canonical and ignores Verbatim, can still
// redact and account). The usage tee fires on response.completed /
// response.incomplete only — the same terminal frames that carry the token
// totals.
func (s *responsesEgressSession) copierChunk(ev specutil.SSEEvent) provcore.Chunk {
	evType := ev.Event
	if evType == "" {
		evType = gjson.GetBytes(ev.Data, "type").String()
	}
	chunk := provcore.Chunk{
		RawBytes:    formatSSE(ev.Event, ev.Data),
		NativeEvent: evType,
		Verbatim:    true,
	}
	switch evType {
	case "response.output_text.delta", "response.refusal.delta":
		chunk.Delta = gjson.GetBytes(ev.Data, "delta").String()
	case "response.function_call_arguments.delta":
		s.sawToolCall = true
		chunk.ToolCallDeltas = []provcore.ToolCallDelta{{
			Index:     int(gjson.GetBytes(ev.Data, "output_index").Int()),
			ID:        gjson.GetBytes(ev.Data, "item_id").String(),
			Arguments: gjson.GetBytes(ev.Data, "delta").String(),
		}}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		chunk.ReasoningDelta = gjson.GetBytes(ev.Data, "delta").String()
	case "response.completed", "response.incomplete":
		chunk.Done = true
		usage := specutil.ExtractOpenAIUsage(gjson.GetBytes(ev.Data, "response.usage"))
		chunk.Usage = usagePtrOrNil(usage)
		chunk.FinishReason = s.responsesTerminalFinishReason(evType, ev.Data)
	case "response.failed", "response.error":
		chunk.Done = true
	}
	return chunk
}

// responsesTerminalFinishReason maps a Responses terminal event to the
// canonical OpenAI finish_reason so a downstream re-encoder preserves it.
// completed → stop (or tool_calls when function calls were seen);
// incomplete → length / content_filter per incomplete_details.reason
// (mirrors mapResponsesStatusToFinishReason).
func (s *responsesEgressSession) responsesTerminalFinishReason(evType string, data []byte) string {
	if evType == "response.incomplete" {
		if gjson.GetBytes(data, "response.incomplete_details.reason").String() == "content_filter" {
			return "content_filter"
		}
		return "length"
	}
	if s.sawToolCall {
		return "tool_calls"
	}
	return "stop"
}
