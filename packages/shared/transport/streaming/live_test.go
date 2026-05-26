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

func TestLivePipeline_RejectAtCheckpoint(t *testing.T) {
	callCount := 0
	mp := &mockPipeline{
		decideFn: func(ctx context.Context, input *core.HookInput) *core.CompliancePipelineResult {
			callCount++
			// Reject on second checkpoint.
			if callCount >= 2 {
				return &core.CompliancePipelineResult{
					Decision: core.RejectHard,
					Reason:   "policy violation detected",
				}
			}
			return &core.CompliancePipelineResult{
				Decision: core.Approve,
			}
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
	}
	if result.Decision != core.RejectHard {
		t.Errorf("expected REJECT_HARD, got %s", result.Decision)
	}

	// Output should contain the error message.
	outputStr := output.String()
	if !strings.Contains(outputStr, "blocked by policy") {
		t.Errorf("expected output to contain error message, got:\n%s", outputStr)
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

// closableReader is an io.Reader + io.Closer that records whether
// Close was called. Used to pin that LivePipeline closes upstream
// on writer-error / overflow exits so the reader goroutine doesn't
// wedge on a slow upstream.Read.
type closableReader struct {
	r        *strings.Reader
	closeMu  sync.Mutex
	closeN   int
	closeErr error
}

func newClosableReader(body string) *closableReader {
	return &closableReader{r: strings.NewReader(body)}
}

func (c *closableReader) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *closableReader) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	c.closeN++
	return c.closeErr
}

func (c *closableReader) closeCount() int {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.closeN
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

// TestLivePipeline_WriterError_ClosesUpstream pins the PR #24
// follow-up S1-code fix: when the writer hits an error mid-stream,
// LivePipeline.Process must close the upstream io.Closer so the
// reader goroutine's blocking parser.Next returns and wg.Wait can
// complete. Without this, a writer error against a slow upstream
// would wedge the goroutine for the full upstream response duration.
//
// The 2nd-round architect review (R-7) flagged that using
// strings.Reader here doesn't actually test the wedge — strings.Reader
// returns EOF too fast for the regression-without-fix to manifest.
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

// TestLivePipeline_RejectHard_ClosesUpstream pins the PR #24
// 2nd-round follow-up R-1 fix: the RejectHard branch in the
// compliance goroutine also needs to call CloseUpstreamOnExit (the
// initial fix covered writer-error + overflow but missed RejectHard).
// Without this, a slow upstream + RejectHard mid-stream wedges
// indefinitely. blockingReader makes the test fail deterministically
// if the fix is reverted.
func TestLivePipeline_RejectHard_ClosesUpstream(t *testing.T) {
	rejectMP := &mockPipeline{
		decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
			return &core.CompliancePipelineResult{Decision: core.RejectHard, Reason: "test reject"}
		},
	}
	upstream := newBlockingReader([]byte(makeOpenAISSE("hello world is enough text to trip checkpoint")))
	var writer bytes.Buffer
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 5}, rejectMP, slog.Default())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = lp.Process(context.Background(), upstream, &writer, &core.HookInput{
			Stage:       "response",
			IngressType: "COMPLIANCE_PROXY",
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not return within 2s after RejectHard — wedge regression (R-1: CloseUpstreamOnExit missing at RejectHard branch)")
	}
	if upstream.closeCount() == 0 {
		t.Errorf("expected upstream.Close on RejectHard (R-1 fix); got 0")
	}
}

// TestLivePipeline_CancelDuringCheckpoint_FinalResultRaceFree drives
// the cancel-mid-checkpoint scenario. A slow pipeline.Execute simulates
// a hook taking time; the test cancels ctx WHILE Execute is in
// flight. The compliance goroutine may write `finalResult` after the
// cancel, then return; the main goroutine reads `finalResult` only
// after wg.Wait(), so happens-before via wg makes the publish safe.
//
// Run with `go test -race` (CI default). PR #24 architect review
// flagged a potential race on `finalResult`; this test pins that the
// path is race-free under the race detector. Any future refactor that
// reorders the publish before wg.Wait() will trip this test.
func TestLivePipeline_CancelDuringCheckpoint_FinalResultRaceFree(t *testing.T) {
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
