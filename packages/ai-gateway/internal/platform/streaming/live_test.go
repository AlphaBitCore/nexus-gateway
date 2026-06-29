package streaming

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

func approveStreamHook(_ context.Context, _ *goHooks.HookInput) *goHooks.CompliancePipelineResult {
	return &goHooks.CompliancePipelineResult{Decision: goHooks.Approve}
}

func rejectStreamHook(_ context.Context, _ *goHooks.HookInput) *goHooks.CompliancePipelineResult {
	return &goHooks.CompliancePipelineResult{Decision: goHooks.RejectHard}
}

func makeSSEStream(chunks ...string) string {
	var buf bytes.Buffer
	for _, c := range chunks {
		buf.WriteString("data: " + c + "\n\n")
	}
	buf.WriteString("data: [DONE]\n\n")
	return buf.String()
}

func TestLivePipeline_PassThrough(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"Hello "}}]}`,
		`{"choices":[{"delta":{"content":"world"}}]}`,
	)

	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 1000,
		EmitOpenAIDone:    true, // OpenAI-shape ingress test fixture
	}, approveStreamHook, nil, slog.Default())

	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("should not be blocked")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Hello ") {
		t.Error("missing Hello in output")
	}
	if !strings.Contains(body, "world") {
		t.Error("missing world in output")
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("missing [DONE]")
	}
}

// TestLivePipeline_NoDoneForAnthropicIngress pins the contract that
// Anthropic / Gemini ingress clients do NOT receive a stray
// `data: [DONE]\n\n` line. Strict SDKs (Anthropic JS v0.30+,
// anthropic-py >=0.40) dispatch unkeyed `data:` lines to a default
// "message" handler and choke on the non-JSON `[DONE]` payload —
// Claude Code's "blank assistant message" symptom on /v1/messages
// was this exact bug.
func TestLivePipeline_NoDoneForAnthropicIngress(t *testing.T) {
	// Simulate an Anthropic-shape upstream stream: typed event lines
	// terminated by message_stop, NO `[DONE]` from the upstream.
	input := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 1000,
		EmitOpenAIDone:    false, // Anthropic ingress
	}, approveStreamHook, nil, slog.Default())

	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/messages"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("should not be blocked")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("missing message_stop terminator: %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("Anthropic ingress must NOT receive `[DONE]` sentinel: %q", body)
	}
}

func TestLivePipeline_HoldBack(t *testing.T) {
	// Short firstInspectChars to trigger checkpoint.
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"Hello world, this is a long enough response"}}]}`,
	)

	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 5,
	}, approveStreamHook, nil, slog.Default())

	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("should not be blocked")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Hello world") {
		t.Error("held-back content should be released after checkpoint")
	}
}

func TestLivePipeline_Reject(t *testing.T) {
	// AUDIT-ONLY (B1): a RejectHard at a live checkpoint does NOT cut the wire —
	// the live path carries only non-enforcing traffic (block scopes route to
	// buffer upfront), so the decision is recorded for audit but the content is
	// delivered, never blocked.
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"content is delivered; block routes to buffer"}}]}`,
	)

	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 5,
	}, rejectStreamHook, nil, slog.Default())

	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("audit-only live must NOT block on RejectHard (block scopes route to buffer)")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "content is delivered") {
		t.Errorf("audit-only: content must be delivered, got %q", body)
	}
}

func TestLivePipeline_Transform(t *testing.T) {
	// Simulate a non-OpenAI provider chunk that needs transformation.
	input := makeSSEStream(`{"custom":"format"}`)

	transform := func(data []byte) ([]byte, error) {
		return []byte(`{"choices":[{"delta":{"content":"transformed"}}]}`), nil
	}

	lp := NewLivePipeline(LiveConfig{FirstInspectChars: 1000}, approveStreamHook, transform, slog.Default())

	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("should not be blocked")
	}
	if !strings.Contains(rec.Body.String(), "transformed") {
		t.Error("transformed content should appear in output")
	}
}

func TestLivePipeline_SkipNilTransform(t *testing.T) {
	// Transform returns nil — chunk should be skipped (e.g. Anthropic ping).
	input := makeSSEStream(`{"type":"ping"}`)

	transform := func(data []byte) ([]byte, error) {
		return nil, nil // skip
	}

	lp := NewLivePipeline(LiveConfig{FirstInspectChars: 1000}, approveStreamHook, transform, slog.Default())

	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("should not be blocked")
	}
	// Only [DONE] should be in output (ping was skipped).
	body := rec.Body.String()
	if strings.Contains(body, "ping") {
		t.Error("ping should be skipped")
	}
}

// (TestLivePipeline_ModifyRewritesHeldBuffer removed — the live path is audit-only
// (B1); a Modify never rewrites the wire. The audit-only Modify behavior is pinned
// by TestLivePipeline_ModifyDeliversOriginalAuditOnly in live_pipeline_decisions_test.go.)

// Ensure httptest.ResponseRecorder implements http.Flusher for our tests.
var _ http.Flusher = httptest.NewRecorder()
