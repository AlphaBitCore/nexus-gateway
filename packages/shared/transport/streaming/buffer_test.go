package streaming

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
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
