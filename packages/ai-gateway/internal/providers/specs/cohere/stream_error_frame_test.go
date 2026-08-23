// stream_error_frame_test.go covers §3a Rule 10 on the Cohere SSE decoder:
// finish_reason ERROR is a failure and must raise, not name a finish_reason.
//
// The previous behaviour mapped it to the string "error", which is not a
// member of OpenAI's finish_reason enum. That is better than claiming
// "stop", but the stream still ended cleanly: no client SDK raised, and the
// caller kept whatever partial text had arrived with no way to learn the
// generation had failed.
package cohere

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestCohereStream_ErrorFinishReasonRaises(t *testing.T) {
	s := NewSpec(slog.Default())
	raw := `data: {"type":"content-delta","delta":{"message":{"content":{"text":"partial"}}}}` + "\n\n" +
		`data: {"type":"message-end","delta":{"finish_reason":"ERROR"},"usage":{"tokens":{"input_tokens":5,"output_tokens":2}}}` + "\n\n"

	sess, err := s.StreamDecoder.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	first, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	if first.Delta != "partial" {
		t.Fatalf("Delta=%q want the content that streamed before the failure", first.Delta)
	}

	second, err := sess.Next(context.Background())
	if err == nil {
		t.Fatalf("finish_reason ERROR decoded to a chunk instead of raising: %+v", second)
	}
	if second.Done {
		t.Error("a failed stream must not be reported as Done")
	}
	if second.FinishReason != "" {
		t.Errorf("FinishReason=%q — a failure must not be given any finish_reason", second.FinishReason)
	}

	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *provcore.ProviderError, got %T: %v", err, err)
	}
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("Code=%q want %q", pe.Code, provcore.CodeUpstreamError)
	}
	if !strings.Contains(string(pe.Raw), "ERROR") {
		t.Errorf("Raw must carry the offending frame verbatim, got %s", pe.Raw)
	}
}

// Every non-ERROR terminal value still ends the stream normally — the arm
// must not swallow ordinary completions.
func TestCohereStream_NonErrorFinishReasonsStillComplete(t *testing.T) {
	for cohereValue, wantFinish := range map[string]string{
		"COMPLETE":      "stop",
		"MAX_TOKENS":    "length",
		"STOP_SEQUENCE": "stop",
		"TOOL_CALL":     "tool_calls",
	} {
		s := NewSpec(slog.Default())
		raw := `data: {"type":"message-end","delta":{"finish_reason":"` + cohereValue + `"}}` + "\n\n"
		sess, err := s.StreamDecoder.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
		if err != nil {
			t.Fatalf("%s: open: %v", cohereValue, err)
		}
		chunk, err := sess.Next(context.Background())
		if err != nil {
			t.Errorf("%s: must not raise: %v", cohereValue, err)
		} else if chunk.FinishReason != wantFinish {
			t.Errorf("%s: FinishReason=%q want %q", cohereValue, chunk.FinishReason, wantFinish)
		}
		_ = sess.Close()
	}
}
