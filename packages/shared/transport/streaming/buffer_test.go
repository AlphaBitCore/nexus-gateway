package streaming

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestBufferPipeline_AllApproved(t *testing.T) {
	mp := &mockPipeline{}
	logger := slog.Default()

	bp := NewBufferPipeline(BufferConfig{}, mp, logger)

	input := makeOpenAISSE("Hello", " World")
	baseTx := &core.HookInput{
		Stage:       "response",
		SourceIP:    "127.0.0.1",
		TargetHost:  "api.openai.com",
		IngressType: "COMPLIANCE_PROXY",
	}

	var output bytes.Buffer
	result, err := bp.Process(context.Background(), strings.NewReader(input), &output, baseTx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
		return
	}
	if result.Decision != core.Approve {
		t.Errorf("expected APPROVE, got %s", result.Decision)
	}

	// Verify the pipeline was called once with full content.
	if len(mp.calls) != 1 {
		t.Fatalf("expected 1 pipeline call, got %d", len(mp.calls))
	}
	if mp.calls[0][0] != "Hello World" {
		t.Errorf("expected content='Hello World', got %q", mp.calls[0][0])
	}

	// Verify output contains replayed events.
	outputStr := output.String()
	if !strings.Contains(outputStr, "Hello") {
		t.Error("expected output to contain 'Hello'")
	}
	if !strings.Contains(outputStr, "[DONE]") {
		t.Error("expected output to contain [DONE]")
	}
}

func TestBufferPipeline_Rejected(t *testing.T) {
	mp := &mockPipeline{
		decideFn: func(ctx context.Context, input *core.HookInput) *core.CompliancePipelineResult {
			return &core.CompliancePipelineResult{
				Decision: core.RejectHard,
				Reason:   "contains PII",
			}
		},
	}
	logger := slog.Default()

	bp := NewBufferPipeline(BufferConfig{}, mp, logger)

	input := makeOpenAISSE("My SSN is 123-45-6789")
	baseTx := &core.HookInput{
		Stage:       "response",
		SourceIP:    "127.0.0.1",
		TargetHost:  "api.openai.com",
		IngressType: "COMPLIANCE_PROXY",
	}

	var output bytes.Buffer
	result, err := bp.Process(context.Background(), strings.NewReader(input), &output, baseTx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != core.RejectHard {
		t.Errorf("expected REJECT_HARD, got %s", result.Decision)
	}

	// Output should contain error, not the original content.
	outputStr := output.String()
	if !strings.Contains(outputStr, "blocked by policy") {
		t.Error("expected error message in output")
	}
	if strings.Contains(outputStr, "123-45-6789") {
		t.Error("rejected content should NOT appear in output")
	}
}

func TestBufferPipeline_MaxBufferExceeded(t *testing.T) {
	mp := &mockPipeline{}
	logger := slog.Default()

	bp := NewBufferPipeline(BufferConfig{
		MaxBufferSize: 50, // very small buffer
	}, mp, logger)

	// Create a stream that exceeds the buffer.
	input := makeOpenAISSE(
		"This is a long string that will exceed the buffer",
		" and cause an error to be returned",
	)
	baseTx := &core.HookInput{
		Stage:       "response",
		IngressType: "COMPLIANCE_PROXY",
	}

	var output bytes.Buffer
	_, err := bp.Process(context.Background(), strings.NewReader(input), &output, baseTx)
	if err == nil {
		t.Fatal("expected error for buffer overflow")
	}
	if !strings.Contains(err.Error(), "maximum buffer size") {
		t.Errorf("expected buffer overflow error, got: %v", err)
	}

	// Pipeline should not have been called.
	if len(mp.calls) != 0 {
		t.Errorf("expected 0 pipeline calls, got %d", len(mp.calls))
	}
}

// Modify-decision behavior in buffer mode is covered by the
// FrameRedactor tests in frame_redactor_test.go:
//   - ModifyWithRedactor_ReplaysMasked   (supported redaction)
//   - ModifyRedactorUnsupported_FailsClosed (fail-closed wire)
//   - ModifyNoRedactor_LegacyDegrade      (backward-compat degrade)

// readCounter reads the current value of
// modifyDegradedTotal{reason=label} for assertion deltas. testutil
// gives us a clean float64 without the dto round-trip.
func readCounter(_ *testing.T, reason string) float64 {
	return testutil.ToFloat64(modifyDegradedTotal.WithLabelValues(reason))
}

// TestRecordModelAEscalation_SplitsByCause pins #11: the Model A escalation counter
// increments under the bounded cause label, so a memory-pressure eviction (a buffer-ceiling
// tuning signal) is observably distinct from a confirmed enforcing hit.
func TestRecordModelAEscalation_SplitsByCause(t *testing.T) {
	modelAEscalationTotal.DeleteLabelValues(ModelAEscalationConfirmed)
	modelAEscalationTotal.DeleteLabelValues(ModelAEscalationMemoryPressure)

	RecordModelAEscalation(ModelAEscalationConfirmed)
	RecordModelAEscalation(ModelAEscalationConfirmed)
	RecordModelAEscalation(ModelAEscalationMemoryPressure)

	if got := testutil.ToFloat64(modelAEscalationTotal.WithLabelValues(ModelAEscalationConfirmed)); got != 2 {
		t.Errorf("confirmed escalations = %v, want 2", got)
	}
	if got := testutil.ToFloat64(modelAEscalationTotal.WithLabelValues(ModelAEscalationMemoryPressure)); got != 1 {
		t.Errorf("memory_pressure escalations = %v, want 1", got)
	}
}

// TestBufferPipeline_ApproveWebhookAuditSpansSoftBlock_NoOverBlock is the #14 fix at the
// layer the over-block actually occurs: on the strict appliance with NO frame redactor, a
// BlockSoft aggregate whose only spans are audit-only (RedactionApplicable=false — the
// approve-webhook+redactions shape) must NOT take the redaction arm (which would fail
// closed) — it soft-delivers the original via the default replay arm.
func TestBufferPipeline_ApproveWebhookAuditSpansSoftBlock_NoOverBlock(t *testing.T) {
	mp := &mockPipeline{
		decideFn: func(ctx context.Context, input *core.HookInput) *core.CompliancePipelineResult {
			return &core.CompliancePipelineResult{
				Decision:            core.BlockSoft,
				Reason:              "advisory soft flag",
				TransformSpans:      []normalize.TransformSpan{{Source: normalize.SourceHook, Action: normalize.ActionRedact, ContentAddress: normalize.AddressAuditOnlySentinel}},
				RedactionApplicable: false,
			}
		},
	}
	// Strict appliance + NO frame redactor: the pre-fix predicate (len(spans)>0) would
	// route here to the redaction arm and fail closed (over-block).
	bp := NewBufferPipeline(BufferConfig{}, mp, slog.Default()).WithStrictFailClosed(true)

	input := makeOpenAISSE("Hello", " World")
	baseTx := &core.HookInput{Stage: "response", IngressType: "COMPLIANCE_PROXY", TargetHost: "api.openai.com"}

	var output bytes.Buffer
	result, err := bp.Process(context.Background(), strings.NewReader(input), &output, baseTx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CarriesRedaction() {
		t.Fatal("audit-only spans under soft-block must NOT CarryRedaction")
	}
	outputStr := output.String()
	if strings.Contains(outputStr, "blocked by policy") {
		t.Fatal("appliance over-blocked a soft-deliver stream (the #14 bug) — must replay the original")
	}
	if !strings.Contains(outputStr, "Hello World") && !strings.Contains(outputStr, "Hello") {
		t.Fatalf("expected the original content soft-delivered, got %q", outputStr)
	}
}

// TestBufferPipeline_ApplicableRedactSoftBlock_FailsClosed is the contrast/leak-guard: a
// BlockSoft aggregate carrying an APPLICABLE redaction (RedactionApplicable=true) on the
// strict appliance WITHOUT a frame redactor still fails closed — the predicate correctly
// routes a real redaction to the protective arm (never replays the original raw).
func TestBufferPipeline_ApplicableRedactSoftBlock_FailsClosed(t *testing.T) {
	mp := &mockPipeline{
		decideFn: func(ctx context.Context, input *core.HookInput) *core.CompliancePipelineResult {
			return &core.CompliancePipelineResult{
				Decision:            core.BlockSoft,
				Reason:              "redact masked by soft-block",
				TransformSpans:      []normalize.TransformSpan{{Source: normalize.SourceHook, Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 0, End: 5}},
				RedactionApplicable: true,
			}
		},
	}
	bp := NewBufferPipeline(BufferConfig{}, mp, slog.Default()).WithStrictFailClosed(true)

	input := makeOpenAISSE("secret payload")
	baseTx := &core.HookInput{Stage: "response", IngressType: "COMPLIANCE_PROXY", TargetHost: "api.openai.com"}

	var output bytes.Buffer
	result, err := bp.Process(context.Background(), strings.NewReader(input), &output, baseTx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.CarriesRedaction() {
		t.Fatal("an applicable redaction masked by soft-block MUST CarryRedaction")
	}
	if strings.Contains(output.String(), "secret payload") {
		t.Fatal("strict appliance with no redactor must NOT replay the original (leak) — fail closed")
	}
}

func TestBufferPipeline_EmptyStream(t *testing.T) {
	mp := &mockPipeline{}
	logger := slog.Default()

	bp := NewBufferPipeline(BufferConfig{}, mp, logger)

	input := "data: [DONE]\n\n"
	baseTx := &core.HookInput{
		Stage:       "response",
		IngressType: "COMPLIANCE_PROXY",
	}

	var output bytes.Buffer
	result, err := bp.Process(context.Background(), strings.NewReader(input), &output, baseTx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != core.Approve {
		t.Errorf("expected APPROVE, got %s", result.Decision)
	}
}
