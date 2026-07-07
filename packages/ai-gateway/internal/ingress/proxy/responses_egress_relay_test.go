// Package handler — responses_egress_relay_test.go covers the streaming egress
// pieces of the /v1/responses fix: the enforcement re-emit encoder must match
// the ingress shape (a /v1/responses stream emits response.* events, never
// chat.completion.chunk), and the live relay must forward a Verbatim chunk's
// RawBytes byte-for-byte only on the allowed passthrough lane.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// TestFallbackStreamEncoder_IngressShape proves the enforcement re-emit encoder
// (buffer / Model A nil-transcoder fallback) is ingress-shape-aware: a
// /v1/responses stream emits Responses typed events and NEVER chat.completion
// chunks, while every other ingress keeps the chat-completions encoder.
func TestFallbackStreamEncoder_IngressShape(t *testing.T) {
	ctx := context.Background()
	render := func(ingress provcore.Format) string {
		enc := fallbackStreamEncoder(ingress, "gpt-4o")
		var out bytes.Buffer
		b, _ := enc.Write(ctx, provcore.Chunk{Delta: "hi"})
		out.Write(b)
		b, _ = enc.Write(ctx, provcore.Chunk{Done: true, FinishReason: "stop"})
		out.Write(b)
		return out.String()
	}

	resp := render(provcore.FormatOpenAIResponses)
	if !bytes.Contains([]byte(resp), []byte("event: response.")) {
		t.Fatalf("Responses ingress must emit response.* events; got:\n%s", resp)
	}
	if bytes.Contains([]byte(resp), []byte("chat.completion.chunk")) {
		t.Fatalf("Responses ingress must NEVER emit chat.completion.chunk; got:\n%s", resp)
	}
	if !bytes.Contains([]byte(resp), []byte("response.completed")) {
		t.Fatalf("Responses ingress enforced stream must carry a terminal response.completed; got:\n%s", resp)
	}

	chat := render(provcore.FormatOpenAI)
	if !bytes.Contains([]byte(chat), []byte("chat.completion.chunk")) {
		t.Fatalf("OpenAI chat ingress must emit chat.completion.chunk; got:\n%s", chat)
	}
}

// verbatimSub is a minimal ChunkSubscription emitting a fixed chunk timeline.
type verbatimSub struct {
	chunks []provcore.Chunk
	i      int
}

func (s *verbatimSub) Next(_ context.Context) (provcore.Chunk, error) {
	if s.i >= len(s.chunks) {
		return provcore.Chunk{}, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}
func (s *verbatimSub) Close() error { return nil }

var _ streamcache.ChunkSubscription = (*verbatimSub)(nil)

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	var out bytes.Buffer
	buf := make([]byte, 256)
	for {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	return out.String()
}

// TestChunkSSEReader_VerbatimForwarding proves a Verbatim chunk is forwarded
// byte-for-byte on the allowed passthrough lane (built-in-tool events preserved)
// and re-encoded through the transcoder when verbatim is not allowed.
func TestChunkSSEReader_VerbatimForwarding(t *testing.T) {
	builtin := []byte("event: response.web_search_call.results\ndata: {\"type\":\"response.web_search_call.results\"}\n\n")
	chunksOf := func() []provcore.Chunk {
		return []provcore.Chunk{
			{Verbatim: true, RawBytes: builtin, NativeEvent: "response.web_search_call.results"},
			{Verbatim: true, RawBytes: []byte("event: response.completed\ndata: {}\n\n"), Done: true},
		}
	}

	// A Responses encoder is the transcoder on both lanes; the only difference
	// is whether Verbatim forwarding is allowed. The encoder, fed a built-in
	// chunk with no canonical content, never reproduces the web_search bytes —
	// so their presence proves the verbatim lane bypassed it.
	enc := func() canonicalbridge.StreamTranscoder { return canonicalbridge.NewResponsesStreamEncoder("gpt-4o") }

	// allowVerbatim=true → original bytes forwarded, including the built-in event.
	r := newChunkSSEReaderFromSubscription(context.Background(), &verbatimSub{chunks: chunksOf()}, enc(), provcore.FormatOpenAIResponses, true)
	r.usageSink = &chunkUsageHolder{}
	got := readAll(t, r)
	if !bytes.Contains([]byte(got), []byte("response.web_search_call.results")) {
		t.Fatalf("allowed verbatim lane must forward the built-in event byte-for-byte; got:\n%s", got)
	}

	// allowVerbatim=false → Verbatim ignored; the transcoder governs and drops
	// the built-in event (no canonical representation), proving the raw bytes
	// were NOT forwarded.
	r2 := newChunkSSEReaderFromSubscription(context.Background(), &verbatimSub{chunks: chunksOf()}, enc(), provcore.FormatOpenAIResponses, false)
	r2.usageSink = &chunkUsageHolder{}
	got2 := readAll(t, r2)
	if bytes.Contains([]byte(got2), []byte("response.web_search_call.results")) {
		t.Fatalf("verbatim must be ignored when not allowed; got:\n%s", got2)
	}
}
