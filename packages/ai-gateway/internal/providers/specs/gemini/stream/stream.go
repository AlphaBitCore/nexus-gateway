package stream

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"log/slog"

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

	candidates := root.Get("candidates")
	candidates.ForEach(func(_, cand gjson.Result) bool {
		parts := cand.Get("content.parts")
		parts.ForEach(func(_, p gjson.Result) bool {
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
				id := fc.Get("id").String()
				if id == "" {
					h := sha1.Sum([]byte(fc.Get("name").String() + "\x00" + args))
					id = "call_" + fmt.Sprintf("%x", h)[:10]
				}
				chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, provcore.ToolCallDelta{
					Index:     int(cand.Get("index").Int()),
					ID:        id,
					Name:      fc.Get("name").String(),
					Arguments: args,
				})
			}
			return true
		})
		if fr := cand.Get("finishReason"); fr.Exists() && fr.String() != "" {
			s.finishSeen = true
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
