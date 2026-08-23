// Package stream_test — stream_error_frame_test.go covers §3a Rule 10 on
// the Gemini SSE decoder: a Google API error envelope arriving as a data
// frame must raise a typed ProviderError.
//
// Gemini is the sharpest case of the rule. Its SSE carries no terminal
// marker at all — no [DONE], no sentinel frame — so a stream that simply
// stops is indistinguishable from one that finished. Before this arm, an
// error frame fell through the candidates walk (which found none) and
// produced a chunk with no content and no error; the stream then ended and
// the caller kept a truncated answer with nothing to signal the failure.
package stream_test

import (
	"context"
	"errors"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

func TestGeminiStream_ErrorFrameAfterContent_RaisesProviderError(t *testing.T) {
	body := sseData(`{"candidates":[{"content":{"parts":[{"text":"partial"}],"role":"model"},"index":0}]}`) +
		sseData(`{"error":{"code":400,"message":"invalid argument here","status":"INVALID_ARGUMENT"}}`)
	sess := openSession(t, body)
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
		t.Fatalf("error frame decoded to a chunk instead of raising: %+v", second)
	}
	if second.Done {
		t.Error("a failed stream must not be reported as Done")
	}

	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *provcore.ProviderError, got %T: %v", err, err)
	}
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("Code=%q want %q — bytes are already committed at 200, so the error must not be retryable-classified",
			pe.Code, provcore.CodeUpstreamError)
	}
	if pe.Message != "invalid argument here" {
		t.Errorf("Message=%q want the upstream's own message", pe.Message)
	}
	if pe.Type != "INVALID_ARGUMENT" {
		t.Errorf("Type=%q want Google's status string preserved for triage", pe.Type)
	}
}

// A frame with no message still raises; the absence of detail is not
// evidence the stream succeeded.
func TestGeminiStream_ErrorFrameWithoutMessage_StillRaises(t *testing.T) {
	sess := openSession(t, sseData(`{"error":{"status":"UNKNOWN"}}`))
	defer sess.Close() //nolint:errcheck

	_, err := sess.Next(context.Background())
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *provcore.ProviderError, got %T: %v", err, err)
	}
	if pe.Message == "" {
		t.Error("Message must be populated so the caller sees something actionable")
	}
}

// An ordinary candidates frame that happens to carry a null `error` must
// not be mistaken for a failure — the guard is IsObject, not Exists.
func TestGeminiStream_NullErrorField_IsNotAFailure(t *testing.T) {
	sess := openSession(t, sseData(`{"error":null,"candidates":[{"content":{"parts":[{"text":"fine"}],"role":"model"}}]}`))
	defer sess.Close() //nolint:errcheck

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("a null error field must not raise: %v", err)
	}
	if chunk.Delta != "fine" {
		t.Errorf("Delta=%q want fine", chunk.Delta)
	}
}

// After raising, the session is finished — a caller that keeps polling must
// not get a second decode of the frames behind the failure.
func TestGeminiStream_AfterErrorFrame_SessionIsDone(t *testing.T) {
	body := sseData(`{"error":{"code":500,"message":"boom","status":"INTERNAL"}}`) +
		sseData(`{"candidates":[{"content":{"parts":[{"text":"must not be reached"}],"role":"model"}}]}`)
	sess := openSession(t, body)
	defer sess.Close() //nolint:errcheck

	if _, err := sess.Next(context.Background()); err == nil {
		t.Fatal("expected the error frame to raise")
	}
	chunk, err := sess.Next(context.Background())
	if err == nil {
		t.Fatalf("expected the session to be finished, got chunk=%+v", chunk)
	}
}
