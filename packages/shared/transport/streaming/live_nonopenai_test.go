package streaming

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// These tests pin the C-17 contract: the live (chunked_async) relay must run
// the response hook pipeline at least once for ANY stream that carried
// content, regardless of whether the wire shape is one the inline
// delta extractor models.
//
// Why this matters: compliance-proxy and agent are transparent MITM
// interceptors carrying arbitrary provider wires. extractDeltaText models only
// OpenAI chat (choices[0].delta.content), so for Anthropic Messages, Gemini
// generateContent, the OpenAI Responses API, Cohere, and tool-call-only OpenAI
// streams it yields "". The checkpoint cadence was driven purely by that
// extracted length, so pendingLen stayed 0 for the whole stream, every
// checkpoint gate failed, and Execute was never reached — the stream was
// audited as an approve with zero hook executions.
//
// The PreHook is what makes a checkpoint meaningful for these shapes: it
// re-normalizes the cumulative RAW bytes through the Tier 1+2+3 registry and
// overwrites the checkpoint input's Normalized payload, so hooks see real
// content even when the inline extractor returned nothing. That is exactly how
// the buffer pipeline already behaves.

// makeAnthropicSSE builds an Anthropic Messages stream: valid JSON, carries
// assistant text, and has no `choices` key — so extractDeltaText returns "".
func makeAnthropicSSE(deltas ...string) string {
	var sb strings.Builder
	sb.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n")
	for _, d := range deltas {
		fmt.Fprintf(&sb, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"%s\"}}\n\n", d)
	}
	sb.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return sb.String()
}

// makeResponsesAPISSE builds an OpenAI Responses-API stream — also `choices`-less.
func makeResponsesAPISSE(deltas ...string) string {
	var sb strings.Builder
	for _, d := range deltas {
		fmt.Fprintf(&sb, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"%s\"}\n\n", d)
	}
	sb.WriteString("data: {\"type\":\"response.completed\"}\n\n")
	return sb.String()
}

// stubPreHook mimics what tlsbump installs in production: it ignores the
// flat-text fallback and stamps a Normalized payload derived from the raw
// wire bytes, recording what it was handed so the test can assert the
// checkpoint actually saw the whole body.
func stubPreHook(seen *[][]byte) PreHookCallback {
	return func(raw []byte, ci *core.HookInput) {
		cp := make([]byte, len(raw))
		copy(cp, raw)
		*seen = append(*seen, cp)
		ci.Normalized = core.PayloadFromTextSegments([]string{"normalized-from-raw"})
	}
}

// TestLivePipeline_NonOpenAIShape_RunsHooks is the C-17 regression test. Before
// the fix it failed with zero Execute calls.
func TestLivePipeline_NonOpenAIShape_RunsHooks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"anthropic_messages", makeAnthropicSSE("Hello", " world", "!")},
		{"openai_responses_api", makeResponsesAPISSE("Hello", " world", "!")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mp := &mockPipeline{}
			var seenRaw [][]byte
			lp := NewLivePipeline(LiveConfig{CheckpointChars: 500}, mp, slog.Default()).
				WithPreHook(stubPreHook(&seenRaw))

			var output bytes.Buffer
			result, err := lp.Process(context.Background(), strings.NewReader(tc.input), &output, &core.HookInput{Stage: "response"})
			if err != nil {
				t.Fatalf("Process: %v", err)
			}

			// The relay must still be lossless — delivery is never gated on hooks.
			if got := output.String(); !strings.Contains(got, "Hello") || !strings.Contains(got, "world") {
				t.Fatalf("relay dropped content; output=%q", got)
			}

			// THE C-17 CONTRACT: the pipeline must have executed at least once.
			if len(mp.calls) == 0 {
				t.Fatal("response hook pipeline NEVER executed for a non-OpenAI stream " +
					"— C-17: pendingLen stayed 0 so every checkpoint gate failed")
			}
			// And the checkpoint must have been handed the raw wire bytes, which is
			// what lets the pre-hook recover real content for an unmodeled shape.
			if len(seenRaw) == 0 {
				t.Fatal("pre-hook never received raw bytes: the checkpoint could not have recovered content")
			}
			if !bytes.Contains(seenRaw[len(seenRaw)-1], []byte("Hello")) {
				t.Fatalf("pre-hook did not see the streamed content; last snapshot=%q", seenRaw[len(seenRaw)-1])
			}
			if result == nil {
				t.Fatal("nil result")
			}
		})
	}
}

// TestLivePipeline_OpenAIShape_CadenceUnchanged pins that the fix does not
// change the existing OpenAI behavior: the delta-driven cadence still governs,
// so a stream whose extracted text stays under CheckpointChars produces
// exactly ONE checkpoint (the mandatory final one), not one per frame.
func TestLivePipeline_OpenAIShape_CadenceUnchanged(t *testing.T) {
	mp := &mockPipeline{}
	var seenRaw [][]byte
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 500}, mp, slog.Default()).
		WithPreHook(stubPreHook(&seenRaw))

	// 4 short deltas — well under the 500-char cadence.
	input := makeOpenAISSE("Hello", " ", "World", "!")
	var output bytes.Buffer
	if _, err := lp.Process(context.Background(), strings.NewReader(input), &output, &core.HookInput{Stage: "response"}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(mp.calls) != 1 {
		t.Fatalf("expected exactly 1 (final) checkpoint for a short OpenAI stream, got %d "+
			"— a raw-byte-driven cadence would fire far more often and regress throughput", len(mp.calls))
	}
}

// TestLivePipeline_EmptyStream_NoCheckpoint pins the other edge: a stream that
// carried no content at all must NOT synthesize a checkpoint. Firing one would
// record a hook execution that scanned nothing.
func TestLivePipeline_EmptyStream_NoCheckpoint(t *testing.T) {
	mp := &mockPipeline{}
	var seenRaw [][]byte
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 500}, mp, slog.Default()).
		WithPreHook(stubPreHook(&seenRaw))

	var output bytes.Buffer
	if _, err := lp.Process(context.Background(), strings.NewReader("data: [DONE]\n\n"), &output, &core.HookInput{Stage: "response"}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(mp.calls) != 0 {
		t.Fatalf("a content-free stream must not fire a checkpoint, got %d", len(mp.calls))
	}
}

// TestLivePipeline_NonOpenAI_NoPreHook_NoSyntheticCheckpoint pins the fail-open
// boundary: without a PreHook there is nothing that can recover content for an
// unmodeled shape, so firing a checkpoint would execute hooks against empty
// text and record a misleading "scanned, found nothing" result. The raw-byte
// fallback must therefore be gated on the pre-hook being installed.
func TestLivePipeline_NonOpenAI_NoPreHook_NoSyntheticCheckpoint(t *testing.T) {
	mp := &mockPipeline{}
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 500}, mp, slog.Default())

	var output bytes.Buffer
	if _, err := lp.Process(context.Background(), strings.NewReader(makeAnthropicSSE("Hello")), &output, &core.HookInput{Stage: "response"}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(mp.calls) != 0 {
		t.Fatalf("without a pre-hook there is no recoverable content; expected 0 checkpoints, got %d", len(mp.calls))
	}
	// Delivery must still be lossless.
	if !strings.Contains(output.String(), "Hello") {
		t.Fatal("relay dropped content on the no-pre-hook path")
	}
}
