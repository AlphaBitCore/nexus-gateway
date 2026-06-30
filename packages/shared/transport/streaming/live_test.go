package streaming

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// mockPipeline is a test double for PipelineExecutor.
type mockPipeline struct {
	// decideFn is called for each Execute; if nil, returns Approve.
	decideFn func(ctx context.Context, input *core.HookInput) *core.CompliancePipelineResult
	// calls records the text content of each Execute call.
	calls [][]string
}

func (m *mockPipeline) Execute(ctx context.Context, input *core.HookInput) *core.CompliancePipelineResult {
	m.calls = append(m.calls, input.TextSegments())

	if m.decideFn != nil {
		return m.decideFn(ctx, input)
	}
	return &core.CompliancePipelineResult{
		Decision: core.Approve,
	}
}

// makeOpenAISSE generates an SSE stream with OpenAI-compatible JSON chunks.
func makeOpenAISSE(deltas ...string) string {
	var sb strings.Builder
	for _, d := range deltas {
		fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", d)
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

func TestLivePipeline_NilExecutorAndNilBase_NoPanic(t *testing.T) {
	// A hookless live pipeline (nil executor) built for usage accumulation only, with a
	// nil base input, must not panic at the checkpoint — it skips checkpoint recording
	// and still delivers every event (audit-only write-through).
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 5}, nil, slog.Default())
	input := makeOpenAISSE("Hello", " ", "World", "!")
	var output bytes.Buffer
	result, err := lp.Process(context.Background(), strings.NewReader(input), &output, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || result.Decision != core.Approve {
		t.Fatalf("audit-only relay must return Approve, got %v", result)
	}
	if out := output.String(); !strings.Contains(out, "Hello") || !strings.Contains(out, "World") {
		t.Fatalf("audit-only relay must deliver every event, got %q", out)
	}
}

func TestLivePipeline_AllApproved(t *testing.T) {
	mp := &mockPipeline{}
	logger := slog.Default()

	lp := NewLivePipeline(LiveConfig{
		CheckpointChars: 10, // low threshold so checkpoints fire
	}, mp, logger)

	// Create a stream with short deltas that will cross the checkpoint.
	input := makeOpenAISSE("Hello", " ", "World", "!", " How", " are", " you?")
	baseTx := &core.HookInput{
		Stage:       "response",
		SourceIP:    "127.0.0.1",
		TargetHost:  "api.openai.com",
		IngressType: "COMPLIANCE_PROXY",
	}

	var output bytes.Buffer
	result, err := lp.Process(context.Background(), strings.NewReader(input), &output, baseTx)
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

	// Verify output contains SSE events.
	outputStr := output.String()
	if !strings.Contains(outputStr, "data:") {
		t.Error("expected output to contain SSE data fields")
	}
	if !strings.Contains(outputStr, "[DONE]") {
		t.Error("expected output to contain [DONE]")
	}

	// The pipeline should have been called at least once (checkpoints + final).
	if len(mp.calls) == 0 {
		t.Error("expected at least one pipeline call")
	}
}

// TestLivePipeline_RejectAtCheckpoint pins the AUDIT-ONLY contract: a checkpoint
// hook returning RejectHard on the live path NEVER blocks — every event is delivered
// write-through, no in-band error frame is emitted, and Process returns Approve. The
// live path carries only non-enforcing traffic (enforcing scopes route to buffer /
// Model A upstream), so a hook decision is observed for audit but never gates the wire.
func TestLivePipeline_RejectAtCheckpoint(t *testing.T) {
	callCount := 0
	mp := &mockPipeline{
		decideFn: func(ctx context.Context, input *core.HookInput) *core.CompliancePipelineResult {
			callCount++
			// Even an enforcing RejectHard must NOT block the audit-only live path.
			if callCount >= 2 {
				return &core.CompliancePipelineResult{Decision: core.RejectHard, Reason: "policy violation detected"}
			}
			return &core.CompliancePipelineResult{Decision: core.Approve}
		},
	}
	logger := slog.Default()

	lp := NewLivePipeline(LiveConfig{
		CheckpointChars: 5, // very low threshold to trigger checkpoints quickly
	}, mp, logger)

	input := makeOpenAISSE("Hello", " World", " this", " is", " a", " test", " of", " policy")
	baseTx := &core.HookInput{
		Stage:       "response",
		SourceIP:    "127.0.0.1",
		TargetHost:  "api.openai.com",
		IngressType: "COMPLIANCE_PROXY",
	}

	var output bytes.Buffer
	result, err := lp.Process(context.Background(), strings.NewReader(input), &output, baseTx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
		return
	}
	if result.Decision != core.Approve {
		t.Errorf("audit-only live path must always return Approve, got %s", result.Decision)
	}
	if callCount == 0 {
		t.Error("expected the audit checkpoint pipeline to run")
	}

	outputStr := output.String()
	if strings.Contains(outputStr, "blocked by policy") {
		t.Errorf("audit-only live path must NOT emit a block frame, got:\n%s", outputStr)
	}
	// All content is delivered write-through despite the RejectHard checkpoint.
	if !strings.Contains(outputStr, "Hello") || !strings.Contains(outputStr, "policy") {
		t.Errorf("expected the full stream delivered write-through, got:\n%s", outputStr)
	}
}

// TestLivePipeline_AuditOnly_PreHookAndCapture exercises the observe-only checkpoint
// with a PreHook installed (the raw-bytes tee) plus body capture: the PreHook fires
// at each checkpoint, the full stream is delivered write-through, and the captured
// body mirrors the delivered bytes. Decision is Approve (audit-only never enforces).
func TestLivePipeline_AuditOnly_PreHookAndCapture(t *testing.T) {
	preHookFired := 0
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 5}, &mockPipeline{}, slog.Default())
	lp.WithPreHook(func(_ []byte, _ *core.HookInput) { preHookFired++ })
	lp.WithBodyCapture(1 << 20)

	input := makeOpenAISSE("Hello", " World", " this", " is", " enough")
	var out bytes.Buffer
	res, err := lp.Process(context.Background(), strings.NewReader(input), &out, &core.HookInput{
		Stage:       "response",
		IngressType: "COMPLIANCE_PROXY",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.Decision != core.Approve {
		t.Errorf("audit-only must return Approve, got %+v", res)
	}
	if preHookFired == 0 {
		t.Error("PreHook never fired — the raw-bytes tee + checkpoint PreHook branch is unexercised")
	}
	if !strings.Contains(out.String(), "Hello") || !strings.Contains(out.String(), "enough") {
		t.Errorf("expected the full stream delivered write-through, got %q", out.String())
	}
	if cb := lp.CapturedBytes(); cb == nil || !strings.Contains(string(cb), "Hello") {
		t.Errorf("body capture did not record the delivered bytes, got %q", string(cb))
	}
}

func TestLivePipeline_FinalCheckpoint(t *testing.T) {
	// Use a high checkpoint threshold so only the final flush triggers.
	mp := &mockPipeline{}
	logger := slog.Default()

	lp := NewLivePipeline(LiveConfig{
		CheckpointChars: 10000, // very high — only final checkpoint fires
	}, mp, logger)

	input := makeOpenAISSE("Hi")
	baseTx := &core.HookInput{
		Stage:       "response",
		SourceIP:    "127.0.0.1",
		TargetHost:  "api.openai.com",
		IngressType: "COMPLIANCE_PROXY",
	}

	var output bytes.Buffer
	result, err := lp.Process(context.Background(), strings.NewReader(input), &output, baseTx)
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

	// Exactly one pipeline call for the final checkpoint.
	if len(mp.calls) != 1 {
		t.Errorf("expected exactly 1 pipeline call, got %d", len(mp.calls))
	}
	// Content should be "Hi".
	if len(mp.calls[0]) == 0 || mp.calls[0][0] != "Hi" {
		t.Errorf("expected checkpoint content='Hi', got %v", mp.calls[0])
	}

	// Output should contain the event.
	outputStr := output.String()
	if !strings.Contains(outputStr, "Hi") {
		t.Error("expected output to contain 'Hi'")
	}
}

func TestLivePipeline_ContextCancellation(t *testing.T) {
	mp := &mockPipeline{}
	logger := slog.Default()

	lp := NewLivePipeline(LiveConfig{
		CheckpointChars: 10000,
	}, mp, logger)

	// Use a large input that would take a while to process.
	var sb strings.Builder
	for i := range 100 {
		fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"content\":\"word%d \"}}]}\n\n", i)
	}
	sb.WriteString("data: [DONE]\n\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	baseTx := &core.HookInput{
		Stage:       "response",
		IngressType: "COMPLIANCE_PROXY",
	}
	var output bytes.Buffer
	_, err := lp.Process(ctx, strings.NewReader(sb.String()), &output, baseTx)
	// Should not hang; error or nil result is acceptable.
	_ = err
}

// blockingReader simulates a slow / silent upstream: the FIRST Read
// returns `first`; subsequent Read calls BLOCK until Close() is
// called. This IS the wedge scenario the production fix is meant
// to defend against — without CloseUpstreamOnExit firing on the
// error path, the reader goroutine would sit forever in
// parser.Next → upstream.Read. With the fix in place, Close
// unblocks the reader's pending Read with io.EOF and Process can
// return.
type blockingReader struct {
	first    []byte
	yielded  bool
	closed   chan struct{}
	closeMu  sync.Mutex
	closeN   int
	closeErr error
}

func newBlockingReader(first []byte) *blockingReader {
	return &blockingReader{first: first, closed: make(chan struct{})}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if !b.yielded {
		b.yielded = true
		return copy(p, b.first), nil
	}
	<-b.closed
	return 0, io.EOF
}

func (b *blockingReader) Close() error {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	b.closeN++
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return b.closeErr
}

func (b *blockingReader) closeCount() int {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	return b.closeN
}

// flakeyWriter returns an error after writing N bytes. Drives the
// LivePipeline writer-error path so we can assert the upstream
// Close-on-exit fix fires.
type flakeyWriter struct {
	wrote int
	limit int
	err   error
}

func (f *flakeyWriter) Write(p []byte) (int, error) {
	if f.wrote >= f.limit {
		return 0, f.err
	}
	n := len(p)
	if f.wrote+n > f.limit {
		n = f.limit - f.wrote
	}
	f.wrote += n
	if f.wrote >= f.limit {
		return n, f.err
	}
	return n, nil
}

// TestLivePipeline_WriterError_ClosesUpstream pins the writer-error
// close fix: when the writer hits an error mid-stream,
// LivePipeline.Process must close the upstream io.Closer so the
// reader goroutine's blocking parser.Next returns and wg.Wait can
// complete. Without this, a writer error against a slow upstream
// would wedge the goroutine for the full upstream response duration.
//
// Using strings.Reader here would not actually test the wedge —
// strings.Reader returns EOF too fast for the regression-without-fix
// to manifest.
// Switched to blockingReader: first Read returns the seed event,
// subsequent Reads BLOCK until Close fires. Without the
// CloseUpstreamOnExit call on writer error, the test would time out
// at the 2-second deadline; with it, Process returns promptly and
// closeCount > 0.
func TestLivePipeline_WriterError_ClosesUpstream(t *testing.T) {
	mp := &mockPipeline{}
	// SSE wire fragment large enough for the first Read to yield a
	// complete event the parser can hand off; subsequent Reads will
	// block waiting on Close — that's the wedge scenario.
	upstream := newBlockingReader([]byte(makeOpenAISSE("a", "b", "c")))
	writer := &flakeyWriter{limit: 5, err: io.ErrShortWrite}
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 1000}, mp, slog.Default())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = lp.Process(context.Background(), upstream, writer, &core.HookInput{
			Stage:       "response",
			IngressType: "COMPLIANCE_PROXY",
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not return within 2s after writer error — wedge regression (CloseUpstreamOnExit not firing)")
	}
	if upstream.closeCount() == 0 {
		t.Errorf("expected upstream.Close to be called at least once on writer error (S1-code wedge fix); got 0")
	}
}

// TestLivePipeline_CancelDuringCheckpoint_NoDeadlock drives the
// cancel-mid-checkpoint scenario: a slow observe-only pipeline.Execute holds until
// ctx is cancelled WHILE the checkpoint runs inline on the (single) delivery
// goroutine. The test pins that Process returns promptly (no deadlock on wg.Wait,
// no hang on the cancelled Execute) and returns a sane decision shape. There is no
// cross-goroutine publish to race: runCheckpoint / allResults / the final result are
// all touched only by the main goroutine after the audit-only refactor — run under
// `go test -race` to confirm.
func TestLivePipeline_CancelDuringCheckpoint_NoDeadlock(t *testing.T) {
	// Slow pipeline that lets the test inject cancel during Execute.
	executing := make(chan struct{}, 1)
	mp := &mockPipeline{
		decideFn: func(ctx context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
			select {
			case executing <- struct{}{}:
			default:
			}
			// Hold the call until ctx cancel arrives; then return the
			// decision (still must publish finalResult safely).
			<-ctx.Done()
			return &core.CompliancePipelineResult{
				Decision: core.Approve,
				Reason:   "ctx cancelled during execute",
			}
		},
	}

	lp := NewLivePipeline(LiveConfig{
		CheckpointChars:    10,
		MinCheckpointChars: 10,
		MaxCheckpointChars: 100,
	}, mp, slog.Default())

	// Body large enough to trigger one in-loop checkpoint.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"long enough to cross\"}}]}\n\n" +
		"data: [DONE]\n\n"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Once we see Execute start, cancel from another goroutine.
	go func() {
		<-executing
		cancel()
	}()

	var output bytes.Buffer
	result, err := lp.Process(ctx, strings.NewReader(body), &output, &core.HookInput{
		Stage:       "response",
		IngressType: "AI_GATEWAY",
		RequestID:   "cancel-mid-checkpoint",
	})
	_ = err
	// finalResult may be nil (cancel raced past the publish) OR carry
	// the Approve decision — both are valid post-cancel outcomes.
	// What we assert: read does not race the write (race detector
	// would have aborted the test) AND the function returned
	// (didn't deadlock on wg.Wait).
	if result != nil && result.Decision != core.Approve && result.Decision != core.RejectHard && result.Decision != core.BlockSoft && result.Decision != core.Abstain {
		t.Errorf("unexpected decision shape after cancel-mid-checkpoint: %+v", result)
	}
}
