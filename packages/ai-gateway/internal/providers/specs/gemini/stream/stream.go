package stream

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// StreamDecoder parses Gemini's `streamGenerateContent` SSE stream.
// Each `data:` frame is a candidate chunk JSON with the same shape as
// the non-streaming response (candidates + optional usageMetadata).
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

// Open wraps body in a geminiStreamSession.
func (d *StreamDecoder) Open(body io.ReadCloser, _ typology.WireShape) (provcore.StreamSession, error) {
	if body == nil {
		return nil, fmt.Errorf("gemini: nil stream body")
	}
	return &geminiStreamSession{scanner: specutil.NewSSEScanner(body), log: d.log}, nil
}

type geminiStreamSession struct {
	scanner *specutil.SSEScanner
	log     *slog.Logger
	done    bool
	// finishSeen is set after a candidate reports a non-empty finishReason.
	// Trailing usage-only frames may follow; Done is emitted only on the
	// frame that follows finishReason (usage trailer or synthesized at EOF).
	finishSeen bool
	// dataSeen is set after the first non-empty SSE frame is processed.
	// An empty stream (EOF before any data frame) indicates an upstream
	// anomaly (e.g. Gemini implicit-cache empty-body response) and is
	// surfaced as a ProviderError rather than a silent EOF.
	dataSeen bool
	// Gemini frames carry cumulative snapshots. The candidate/Part coordinate
	// owns continuity; a native id is payload, not a global merge key, because
	// a malformed response can repeat one id at two distinct Part positions.
	toolIndexByPosition map[string]int
	nextToolIndex       int
	// Gemini functionCall args are cumulative snapshots; emit only the latest
	// snapshot for each slot when the candidate finishes.
	pendingTools         map[int]provcore.ToolCallDelta
	toolCandidateByIndex map[int]int
}

func (s *geminiStreamSession) Next(ctx context.Context) (provcore.Chunk, error) {
	if s.done {
		return provcore.Chunk{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return provcore.Chunk{}, err
	}
	ev, err := s.scanner.Next()
	if err != nil {
		if errors.Is(err, io.EOF) && s.finishSeen {
			// Upstream finished without a trailing usage frame. Emit a
			// synthesized Done chunk now (with nil error so SSE consumers
			// process it) and surface EOF on the next call. Returning
			// (chunk, io.EOF) together would let consumers like
			// chunkSSEReader drop the chunk and skip "data: [DONE]\n\n".
			s.done = true
			return provcore.Chunk{Done: true}, nil
		}
		if errors.Is(err, io.EOF) && !s.dataSeen {
			// The upstream body was empty — no SSE data frames were received
			// before EOF. This is observed with Gemini's implicit prompt-cache
			// when the streaming endpoint returns Content-Length: 0 (END_STREAM
			// on the HTTP/2 HEADERS frame with no DATA frames). Surface as a
			// provider error so the broker broadcasts it to subscribers and the
			// client receives an explicit error event rather than a silent [DONE].
			return provcore.Chunk{}, &provcore.ProviderError{
				Status:  502,
				Code:    provcore.CodeUpstreamError,
				Message: "upstream returned empty SSE stream (no data frames received)",
			}
		}
		return provcore.Chunk{}, err
	}
	s.dataSeen = true
	chunk := provcore.Chunk{
		RawBytes:    FormatSSE(ev.Event, ev.Data),
		NativeEvent: ev.Event,
	}
	if len(ev.Data) == 0 {
		return chunk, nil
	}
	root := gjson.ParseBytes(ev.Data)

	// Gemini reports a mid-stream failure by sending its standard Google
	// API error envelope as a data frame. With no arm for it the frame fell
	// through to the candidates walk below, which found none, and produced a
	// chunk with no content and no error — the stream then ended cleanly and
	// the caller received a truncated answer that looked complete (§3a
	// Rule 10). Gemini's SSE carries no terminal marker of its own, so
	// nothing downstream could have caught it either.
	//
	// The canonical code is pinned to upstream_error rather than derived
	// from `error.status`: bytes are already committed at HTTP 200, so a
	// code the executor treats as retryable would reopen a decision that is
	// closed. The vendor's status survives on Type.
	if errObj := root.Get("error"); errObj.IsObject() {
		s.done = true
		msg := errObj.Get("message").String()
		if msg == "" {
			msg = "upstream sent an error frame mid-stream with no message"
		}
		return provcore.Chunk{}, &provcore.ProviderError{
			Status:  http.StatusBadGateway,
			Code:    provcore.CodeUpstreamError,
			Type:    errObj.Get("status").String(),
			Message: msg,
			Raw:     ev.Data,
		}
	}

	candidates := root.Get("candidates")
	candidates.ForEach(func(_, cand gjson.Result) bool {
		parts := cand.Get("content.parts")
		parts.ForEach(func(partIndex, p gjson.Result) bool {
			if t := p.Get("text"); t.Exists() {
				// Gemini 2.5+ tags thinking-summary parts with thought=true.
				// Route them to ReasoningDelta so downstream encoders surface
				// them as reasoning_content (OpenAI-spec) or thinking_delta
				// (Anthropic-spec), matching the non-stream codec path.
				if p.Get("thought").Bool() {
					chunk.ReasoningDelta += t.String()
				} else {
					chunk.Delta += t.String()
				}
			}
			if fc := p.Get("functionCall"); fc.Exists() {
				args := fc.Get("args").Raw
				if args == "" {
					args = "{}"
				}
				nativeID := fc.Get("id").String()
				id := nativeID
				if id == "" {
					h := sha1.Sum([]byte(fmt.Sprintf("%s\x00candidate:%d\x00part:%d", fc.Get("name").String(), cand.Get("index").Int(), partIndex.Int())))
					id = "call_" + fmt.Sprintf("%x", h)[:10]
				}
				if s.toolIndexByPosition == nil {
					s.toolIndexByPosition = make(map[string]int)
				}
				positionKey := fmt.Sprintf("candidate:%d\x00part:%d", cand.Get("index").Int(), partIndex.Int())
				toolIndex, ok := s.toolIndexByPosition[positionKey]
				if !ok {
					toolIndex = s.nextToolIndex
					s.nextToolIndex++
				}
				s.toolIndexByPosition[positionKey] = toolIndex
				if s.pendingTools == nil {
					s.pendingTools = make(map[int]provcore.ToolCallDelta)
				}
				if s.toolCandidateByIndex == nil {
					s.toolCandidateByIndex = make(map[int]int)
				}
				pending := s.pendingTools[toolIndex]
				pending.Index = toolIndex
				if id != "" {
					pending.ID = id
				}
				if name := fc.Get("name").String(); name != "" {
					pending.Name = name
				}
				pending.Arguments = args
				if sig := p.Get("thoughtSignature").String(); sig != "" {
					pending.ThoughtSignature = sig
				}
				s.pendingTools[toolIndex] = pending
				s.toolCandidateByIndex[toolIndex] = int(cand.Get("index").Int())
			}
			return true
		})
		if fr := cand.Get("finishReason"); fr.Exists() && fr.String() != "" {
			s.finishSeen = true
			for idx := range s.nextToolIndex {
				if s.toolCandidateByIndex[idx] != int(cand.Get("index").Int()) {
					continue
				}
				if pending, ok := s.pendingTools[idx]; ok {
					chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, pending)
					delete(s.pendingTools, idx)
					delete(s.toolCandidateByIndex, idx)
				}
			}
			// Map Gemini's finishReason enum into the canonical OpenAI
			// vocabulary so a re-encoder (buffer mode) preserves it instead
			// of collapsing to "stop".
			chunk.FinishReason = mapGeminiFinishToCanonical(fr.String())
		}
		return true
	})
	if u := root.Get("usageMetadata"); u.Exists() {
		// Per-chunk Usage extraction via shared/normcodecs.ExtractGeminiEventUsage.
		// CompletionTokens already includes thoughtsTokenCount per the canonical convention.
		if usage := normcodecs.ExtractGeminiEventUsage(ev.Data); usage != nil {
			chunk.Usage = usage
		}
		if s.finishSeen {
			chunk.Done = true
			s.done = true
		}
	}
	return chunk, nil
}

// mapGeminiFinishToCanonical maps a Gemini finishReason enum to the canonical
// OpenAI finish_reason vocabulary. Kept local (rather than importing
// gemini/codec.MapFinishReason) so the stream decoder carries no import edge
// into codec. Mirrors gemini/codec.MapFinishReason; the two must stay in lockstep.
func mapGeminiFinishToCanonical(r string) string {
	switch r {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "LANGUAGE", "PROHIBITED_CONTENT",
		"SPII", "BLOCKLIST", "IMAGE_SAFETY", "MODEL_ARMOR":
		return "content_filter"
	case "OTHER", "":
		return "stop"
	}
	// MALFORMED_FUNCTION_CALL / UNEXPECTED_TOOL_CALL pass through raw: both
	// mean no usable tool call was produced, so "tool_calls" sent an agent
	// loop hunting for an array that is empty.
	return r
}

func (s *geminiStreamSession) Close() error {
	s.done = true
	return s.scanner.Close()
}

// FormatSSE formats an SSE event line. Exported for test access.
func FormatSSE(event string, data []byte) []byte {
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
