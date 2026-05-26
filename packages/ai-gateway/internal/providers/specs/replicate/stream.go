package replicate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// StreamDecoder parses Replicate's SSE stream. When a prediction is
// created with `stream: true`, Replicate sends events:
//   - event: output, data: <text-chunk>
//   - event: done, data: {}
//   - event: logs, data: <metrics-text>     (optional, ignored)
//   - event: error, data: <error-message>   (terminal error)
type StreamDecoder struct {
	log *slog.Logger
}

// NewStreamDecoder constructs a Replicate StreamDecoder.
func NewStreamDecoder(log *slog.Logger) *StreamDecoder {
	if log == nil {
		log = slog.Default()
	}
	return &StreamDecoder{log: log}
}

// Open wraps body in a streamSession.
func (d *StreamDecoder) Open(body io.ReadCloser, _ typology.WireShape) (provcore.StreamSession, error) {
	if body == nil {
		return nil, fmt.Errorf("replicate: nil stream body")
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

	switch ev.Event {
	case "output":
		return provcore.Chunk{
			Delta:       string(ev.Data),
			RawBytes:    formatSSE(ev.Event, ev.Data),
			NativeEvent: ev.Event,
		}, nil
	case "done":
		s.done = true
		return provcore.Chunk{
			Done:        true,
			RawBytes:    []byte("data: [DONE]\n\n"),
			NativeEvent: ev.Event,
		}, nil
	case "error":
		s.done = true
		return provcore.Chunk{
			Delta:       string(ev.Data),
			Done:        true,
			RawBytes:    formatSSE(ev.Event, ev.Data),
			NativeEvent: ev.Event,
		}, nil
	default:
		// `logs` or unknown events: forward raw bytes for audit but no Delta.
		return provcore.Chunk{
			RawBytes:    formatSSE(ev.Event, ev.Data),
			NativeEvent: ev.Event,
		}, nil
	}
}

func (s *streamSession) Close() error {
	s.done = true
	return s.scanner.Close()
}

// formatSSE rebuilds a canonical SSE frame from event + data.
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
