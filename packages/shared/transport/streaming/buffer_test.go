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

func TestBufferPipeline_SoftReject(t *testing.T) {
	mp := &mockPipeline{
		decideFn: func(ctx context.Context, input *core.HookInput) *core.CompliancePipelineResult {
			return &core.CompliancePipelineResult{
				Decision: core.BlockSoft,
				Reason:   "low confidence PII",
			}
		},
	}
	logger := slog.Default()

	bp := NewBufferPipeline(BufferConfig{}, mp, logger)

	input := makeOpenAISSE("Sensitive-ish data")
	baseTx := &core.HookInput{
		Stage:       "response",
		IngressType: "COMPLIANCE_PROXY",
	}

	var output bytes.Buffer
	result, err := bp.Process(context.Background(), strings.NewReader(input), &output, baseTx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != core.BlockSoft {
		t.Errorf("expected BLOCK_SOFT, got %s", result.Decision)
	}

	// Soft reject in buffer mode also blocks replay (same as hard reject).
	outputStr := output.String()
	if !strings.Contains(outputStr, "blocked by policy") {
		t.Error("expected error message in output for soft reject")
	}
}

// TestBufferPipeline_ModifyDegradesToApprove — buffer mode has no
// Modify branch in Phase 3; the body must replay unchanged AND the
// pipeline must surface the degradation via WARN log +
// nexus_streaming_modify_degraded_total{reason="buffer_mode"} so
// admin sees the silent ignore (#115/R3 architect review). Three
// data planes share this pipeline, so this single test covers all.
func TestBufferPipeline_ModifyDegradesToApprove(t *testing.T) {
	mp := &mockPipeline{
		decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
			return &core.CompliancePipelineResult{
				Decision: core.Modify,
				Reason:   "rewrite requested",
				// Modified body is irrelevant — buffer mode ignores it.
			}
		},
	}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	bp := NewBufferPipeline(BufferConfig{}, mp, logger)

	// #115/S2 — reset the global counter timeseries before snapshot
	// so this test is stable under `go test -parallel`. modifyDegraded
	// Total is a promauto-registered package var (shared across all
	// tests in this package); without a Reset another test bumping the
	// same {reason=buffer_mode} label concurrently would race the
	// before/after delta. DeleteLabelValues removes the timeseries;
	// the next access via WithLabelValues creates a fresh zero entry,
	// scoping the +1 assertion to this test's call only.
	modifyDegradedTotal.DeleteLabelValues("buffer_mode")

	input := makeOpenAISSE("verbatim", " bytes")
	baseTx := &core.HookInput{
		Stage:       "response",
		IngressType: "COMPLIANCE_PROXY",
		RequestID:   "buf-modify-1",
	}

	var output bytes.Buffer
	result, err := bp.Process(context.Background(), strings.NewReader(input), &output, baseTx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || result.Decision != core.Modify {
		t.Fatalf("expected pipeline to return underlying Modify result; got %+v", result)
	}

	// Body MUST replay verbatim (Modify is ignored in buffer mode).
	if !strings.Contains(output.String(), "verbatim") || !strings.Contains(output.String(), "bytes") {
		t.Errorf("expected verbatim replay (Modify ignored), got: %q", output.String())
	}
	if !strings.Contains(output.String(), "[DONE]") {
		t.Error("expected [DONE] terminator in replayed output")
	}

	// WARN log MUST surface the degradation with requestId. This is an
	// intra-package contract assertion (the test owns the package that
	// owns the log), so substring coupling is local; the structured
	// requestId check below pins the load-bearing field.
	if !strings.Contains(logBuf.String(), "Modify decision degraded to Approve") {
		t.Errorf("expected WARN log about Modify degradation, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "buf-modify-1") {
		t.Errorf("expected requestId in degradation log, got: %s", logBuf.String())
	}

	// Counter MUST be exactly 1 after the single Modify decision (we
	// reset above, so absolute == delta).
	got := readCounter(t, "buffer_mode")
	if got != 1 {
		t.Errorf("expected nexus_streaming_modify_degraded_total{reason=buffer_mode} == 1 after reset+single-Modify, got %v", got)
	}
}

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
