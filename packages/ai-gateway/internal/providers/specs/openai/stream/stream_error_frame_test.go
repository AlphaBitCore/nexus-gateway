// Package stream_test — stream_error_frame_test.go covers §3a Rule 10 on
// the OpenAI-family SSE decoders: an upstream that reports a mid-stream
// failure as a data frame must surface a typed ProviderError, never a
// chunk that lets the stream end as though it succeeded.
//
// The failure being pinned: chatChunkFromFrame reads only `choices` and
// `usage`, so an error envelope decoded to a chunk with no content and no
// error. The stream then terminated normally and the caller received a
// truncated answer indistinguishable from a short complete one.
package stream_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	ostream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// An error frame arriving after content has already streamed is the whole
// point of the rule: the caller holds partial text, so nothing but an
// explicit error can tell them the answer is not finished.
func TestChatStream_ErrorFrameAfterContent_RaisesProviderError(t *testing.T) {
	raw := `data: {"choices":[{"delta":{"content":"partial answer"}}]}` + "\n\n" +
		`data: {"error":{"type":"server_error","code":"internal","message":"upstream exploded"}}` + "\n\n"

	d := ostream.NewStreamDecoder(slog.Default())
	sess, err := d.Open(sseBody(raw), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	first, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	if first.Delta != "partial answer" {
		t.Fatalf("Delta=%q want the content that streamed before the failure", first.Delta)
	}

	second, err := sess.Next(context.Background())
	if err == nil {
		t.Fatalf("error frame decoded to a chunk instead of raising: %+v", second)
	}
	if second.Done {
		t.Error("a failed stream must not be reported as Done")
	}
	if second.FinishReason != "" {
		t.Errorf("FinishReason=%q — a failure must not carry any finish_reason", second.FinishReason)
	}

	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *provcore.ProviderError, got %T: %v", err, err)
	}
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("Code=%q want %q — bytes are already committed at 200, so the error must not be retryable-classified",
			pe.Code, provcore.CodeUpstreamError)
	}
	if pe.Message != "upstream exploded" {
		t.Errorf("Message=%q want the upstream's own message", pe.Message)
	}
	if pe.Type != "server_error" {
		t.Errorf("Type=%q want the vendor's own type preserved for triage", pe.Type)
	}
}

// Some OpenAI-compatible upstreams send `code` without `type`; the vendor
// string must still survive rather than being dropped.
func TestChatStream_ErrorFrameCodeOnly_KeepsVendorString(t *testing.T) {
	raw := `data: {"error":{"code":"model_overloaded","message":"try later"}}` + "\n\n"
	d := ostream.NewStreamDecoder(slog.Default())
	sess, err := d.Open(sseBody(raw), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	_, err = sess.Next(context.Background())
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *provcore.ProviderError, got %T: %v", err, err)
	}
	if pe.Type != "model_overloaded" {
		t.Errorf("Type=%q want the error.code when error.type is absent", pe.Type)
	}
}

// An error envelope with no message still has to raise — silence about the
// cause is not a reason to report success.
func TestChatStream_ErrorFrameWithoutMessage_StillRaises(t *testing.T) {
	raw := `data: {"error":{}}` + "\n\n"
	d := ostream.NewStreamDecoder(slog.Default())
	sess, err := d.Open(sseBody(raw), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	_, err = sess.Next(context.Background())
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *provcore.ProviderError, got %T: %v", err, err)
	}
	if pe.Message == "" {
		t.Error("Message must be populated so the caller sees something actionable")
	}
}

// `"usage": null` and `"error": null` both appear on ordinary chunks. The
// guard is IsObject, not Exists, so a null must not be mistaken for a
// failure — that would turn every normal stream into an error.
func TestChatStream_NullErrorField_IsNotAFailure(t *testing.T) {
	raw := `data: {"error":null,"usage":null,"choices":[{"delta":{"content":"fine"}}]}` + "\n\n"
	d := ostream.NewStreamDecoder(slog.Default())
	sess, err := d.Open(sseBody(raw), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("a null error field must not raise: %v", err)
	}
	if chunk.Delta != "fine" {
		t.Errorf("Delta=%q want fine", chunk.Delta)
	}
}

// After raising, the session must be finished: a caller that keeps polling
// gets EOF rather than a second decode of the same frame.
func TestChatStream_AfterErrorFrame_SessionIsDone(t *testing.T) {
	raw := `data: {"error":{"message":"boom"}}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"must not be reached"}}]}` + "\n\n"
	d := ostream.NewStreamDecoder(slog.Default())
	sess, err := d.Open(sseBody(raw), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	if _, err = sess.Next(context.Background()); err == nil {
		t.Fatal("expected the error frame to raise")
	}
	chunk, err := sess.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF after a raised stream, got chunk=%+v err=%v", chunk, err)
	}
}

// The /v1/responses egress session decodes upstream frames through its own
// loop, so it needs the same arm — a fix applied to one decode path and not
// the other is exactly the divergence Rule 10 exists to stop.
func TestResponsesEgressStream_ErrorFrame_RaisesProviderError(t *testing.T) {
	raw := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"error":{"type":"server_error","message":"egress exploded"}}` + "\n\n"

	d := ostream.NewStreamDecoder(slog.Default())
	sess, err := d.Open(sseBody(raw), typology.WireShapeOpenAIResponses)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	if _, err = sess.Next(context.Background()); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	_, err = sess.Next(context.Background())
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *provcore.ProviderError on the responses egress path, got %T: %v", err, err)
	}
	if pe.Message != "egress exploded" {
		t.Errorf("Message=%q want the upstream's own message", pe.Message)
	}
	if !strings.Contains(string(pe.Raw), "egress exploded") {
		t.Errorf("Raw must carry the offending frame verbatim, got %s", pe.Raw)
	}
}
