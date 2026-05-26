package streaming

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// recordingFlusherWriter records bytes + flush count, simulating an
// http.Flusher-capable client.
type recordingFlusherWriter struct {
	buf     bytes.Buffer
	flushes int
	mu      sync.Mutex
}

func (w *recordingFlusherWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
func (w *recordingFlusherWriter) Flush() {
	w.mu.Lock()
	w.flushes++
	w.mu.Unlock()
}

// failingClientWriter errors after writeCount successful writes.
type failingClientWriter struct {
	mu         sync.Mutex
	writes     int
	failAfter  int
	failureErr error
}

func (w *failingClientWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writes >= w.failAfter {
		return 0, w.failureErr
	}
	w.writes++
	return len(p), nil
}

// fakeAccumulator counts Feed calls + records all events seen.
type fakeAccumulator struct {
	mu     sync.Mutex
	events []*SSEEvent
}

func (f *fakeAccumulator) Feed(evt *SSEEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, evt)
}

func (f *fakeAccumulator) Finalize(_ context.Context) traffic.UsageMeta {
	return traffic.UsageMeta{Status: traffic.UsageStatusStreamingUnavailable}
}

// TestNewBufferPipeline_NilLogger_DefaultsToSlog — passing nil logger
// must not panic; slog.Default is substituted.
func TestNewBufferPipeline_NilLogger_DefaultsToSlog(t *testing.T) {
	bp := NewBufferPipeline(BufferConfig{}, &mockPipeline{}, nil)
	if bp.logger == nil {
		t.Error("logger nil after constructor; expected slog.Default")
	}
}

// TestBufferPipeline_WithUsageAccumulator_FeedsEveryFrame — every parsed
// event reaches the accumulator (including the [DONE] terminator).
func TestBufferPipeline_WithUsageAccumulator_FeedsEveryFrame(t *testing.T) {
	bp := NewBufferPipeline(BufferConfig{}, &mockPipeline{}, nil)
	acc := &fakeAccumulator{}
	bp.WithUsageAccumulator(acc)

	in := makeOpenAISSE("a", "b", "c")
	var out bytes.Buffer
	if _, err := bp.Process(context.Background(), strings.NewReader(in), &out, &core.HookInput{IngressType: "X"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(acc.events) < 4 { // 3 content frames + [DONE]
		t.Errorf("accumulator saw %d events, want >=4", len(acc.events))
	}
}

// TestBufferPipeline_WithBodyCapture_RecordsReplayedBytes — when capture is
// enabled, the bytes streamed to the client are also recorded.
func TestBufferPipeline_WithBodyCapture_RecordsReplayedBytes(t *testing.T) {
	bp := NewBufferPipeline(BufferConfig{}, &mockPipeline{}, nil).WithBodyCapture(1024)

	in := makeOpenAISSE("hello")
	var client bytes.Buffer
	if _, err := bp.Process(context.Background(), strings.NewReader(in), &client, &core.HookInput{IngressType: "X"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	cap := bp.CapturedBytes()
	if len(cap) == 0 {
		t.Fatal("CapturedBytes empty; expected replayed bytes")
	}
	if !bytes.Contains(cap, []byte("hello")) {
		t.Errorf("CapturedBytes %q missing 'hello'", cap)
	}
	if !bytes.Contains(client.Bytes(), []byte("hello")) {
		t.Error("client missed 'hello' — capture broke relay")
	}
	if bp.CapturedTruncated() {
		t.Error("CapturedTruncated true under 1024-byte cap with tiny stream")
	}
}

// TestBufferPipeline_WithBodyCapture_Truncation — replayed bytes exceed the
// per-buffer cap; CapturedTruncated flips true and CapturedBytes is a prefix.
func TestBufferPipeline_WithBodyCapture_Truncation(t *testing.T) {
	bp := NewBufferPipeline(BufferConfig{}, &mockPipeline{}, nil).WithBodyCapture(10)

	in := makeOpenAISSE("x") // makes ~50 bytes once serialized
	var client bytes.Buffer
	if _, err := bp.Process(context.Background(), strings.NewReader(in), &client, &core.HookInput{IngressType: "X"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !bp.CapturedTruncated() {
		t.Error("expected CapturedTruncated true after overflow")
	}
	if got := bp.CapturedBytes(); len(got) > 10 {
		t.Errorf("len(CapturedBytes) = %d, want <= cap 10", len(got))
	}
}

// TestBufferPipeline_NoCapture_CapturedBytesNil — without WithBodyCapture
// CapturedBytes returns nil and CapturedTruncated is false.
func TestBufferPipeline_NoCapture_CapturedBytesNil(t *testing.T) {
	bp := NewBufferPipeline(BufferConfig{}, &mockPipeline{}, nil)
	if bp.CapturedBytes() != nil {
		t.Error("CapturedBytes non-nil before WithBodyCapture")
	}
	if bp.CapturedTruncated() {
		t.Error("CapturedTruncated true before WithBodyCapture")
	}
}

// TestBufferPipeline_PreCancelledContext_ReturnsErr — ctx pre-cancelled
// causes Process to return ctx.Err() before reading the first event.
func TestBufferPipeline_PreCancelledContext_ReturnsErr(t *testing.T) {
	bp := NewBufferPipeline(BufferConfig{}, &mockPipeline{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := bp.Process(ctx, strings.NewReader(makeOpenAISSE("x")), io.Discard, &core.HookInput{IngressType: "X"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestBufferPipeline_NilPipelineResult_DefaultsToApprove — when the
// PipelineExecutor returns nil, the buffer pipeline must synthesize an
// Approve decision (no panic) and replay events.
func TestBufferPipeline_NilPipelineResult_DefaultsToApprove(t *testing.T) {
	mp := &mockPipeline{
		decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
			return nil
		},
	}
	bp := NewBufferPipeline(BufferConfig{}, mp, nil)
	var out bytes.Buffer
	res, err := bp.Process(context.Background(), strings.NewReader(makeOpenAISSE("hi")), &out, &core.HookInput{IngressType: "X"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Decision != core.Approve {
		t.Errorf("decision = %+v, want Approve", res)
	}
	if !strings.Contains(out.String(), "hi") {
		t.Error("client missed replay text 'hi'")
	}
}

// TestBufferPipeline_ParserError — corrupt upstream that exceeds the
// scanner line limit triggers a parser error, which becomes a wrapped
// "read error" returned from Process.
func TestBufferPipeline_ParserError(t *testing.T) {
	// Construct a single SSE data line longer than maxSSELineSize so the
	// scanner returns bufio.ErrTooLong.
	long := strings.Repeat("a", maxSSELineSize+10)
	in := "data: " + long + "\n\n"

	bp := NewBufferPipeline(BufferConfig{}, &mockPipeline{}, nil)
	var out bytes.Buffer
	_, err := bp.Process(context.Background(), strings.NewReader(in), &out, &core.HookInput{IngressType: "X"})
	if err == nil {
		t.Fatal("expected error from oversized SSE line")
	}
	if !strings.Contains(err.Error(), "read error") {
		t.Errorf("err = %v, want wrapped read error", err)
	}
}

// TestBufferPipeline_WriteEventError_PropagatedFromReplay — client write
// fails during replay; Process returns the wrapped error.
func TestBufferPipeline_WriteEventError_PropagatedFromReplay(t *testing.T) {
	bp := NewBufferPipeline(BufferConfig{}, &mockPipeline{}, nil)
	fc := &failingClientWriter{failAfter: 0, failureErr: errors.New("client gone")}
	_, err := bp.Process(context.Background(), strings.NewReader(makeOpenAISSE("a")), fc, &core.HookInput{IngressType: "X"})
	if err == nil {
		t.Fatal("expected error from client write failure")
	}
	if !strings.Contains(err.Error(), "write event") {
		t.Errorf("err = %v, want wrapped write event error", err)
	}
}

// TestBufferPipeline_WriteErrorAndDoneFails_OnRejection — when client.Write
// fails after a reject decision, Process returns a wrapped "write error
// response" error.
func TestBufferPipeline_WriteErrorAndDoneFails_OnRejection(t *testing.T) {
	mp := &mockPipeline{
		decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
			return &core.CompliancePipelineResult{Decision: core.RejectHard}
		},
	}
	bp := NewBufferPipeline(BufferConfig{}, mp, nil)
	fc := &failingClientWriter{failAfter: 0, failureErr: errors.New("client gone")}
	_, err := bp.Process(context.Background(), strings.NewReader(makeOpenAISSE("anything")), fc, &core.HookInput{IngressType: "X"})
	if err == nil {
		t.Fatal("expected write-error-response error")
	}
	if !strings.Contains(err.Error(), "write error response") {
		t.Errorf("err = %v, want wrapped write error response", err)
	}
}

// TestBufferPipeline_ReplayWithFlushableClient — Process calls Flush after
// every replayed event when the client implements http.Flusher.
func TestBufferPipeline_ReplayWithFlushableClient(t *testing.T) {
	bp := NewBufferPipeline(BufferConfig{}, &mockPipeline{}, nil)
	client := &recordingFlusherWriter{}
	if _, err := bp.Process(context.Background(), strings.NewReader(makeOpenAISSE("a", "b")), client, &core.HookInput{IngressType: "X"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if client.flushes == 0 {
		t.Error("Flush never called on flusher-capable client")
	}
}

// TestBufferPipeline_RejectWithFlushableClient_FlushesError — after a
// hard-reject, Process still calls Flush on the error event.
func TestBufferPipeline_RejectWithFlushableClient_FlushesError(t *testing.T) {
	mp := &mockPipeline{
		decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
			return &core.CompliancePipelineResult{Decision: core.RejectHard, Reason: "x"}
		},
	}
	bp := NewBufferPipeline(BufferConfig{}, mp, nil)
	client := &recordingFlusherWriter{}
	if _, err := bp.Process(context.Background(), strings.NewReader(makeOpenAISSE("a")), client, &core.HookInput{IngressType: "X"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if client.flushes == 0 {
		t.Error("Flush never called after reject path")
	}
	if !strings.Contains(client.buf.String(), "blocked by policy") {
		t.Errorf("client missed error envelope: %q", client.buf.String())
	}
}

// TestBufferPipeline_ContextCancelledMidReplay — cancel ctx in the middle
// of replay; Process returns ctx.Err.
func TestBufferPipeline_ContextCancelledMidReplay(t *testing.T) {
	var fired int
	mp := &mockPipeline{
		decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
			return &core.CompliancePipelineResult{Decision: core.Approve}
		},
	}
	bp := NewBufferPipeline(BufferConfig{}, mp, nil)

	// Wire ctx that cancels after first client write.
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancelOnWriteClient{cancel: cancel, after: 1, fired: &fired}

	in := makeOpenAISSE("a", "b", "c", "d", "e")
	_, err := bp.Process(ctx, strings.NewReader(in), client, &core.HookInput{IngressType: "X"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// cancelOnWriteClient calls cancel() after N writes.
type cancelOnWriteClient struct {
	mu     sync.Mutex
	writes int
	after  int
	cancel context.CancelFunc
	fired  *int
}

func (c *cancelOnWriteClient) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	if c.writes == c.after {
		if c.fired != nil {
			*c.fired = c.writes
		}
		c.cancel()
	}
	c.mu.Unlock()
	return len(p), nil
}

// TestNewLivePipeline_NilLogger_DefaultsToSlog — same nil-logger guard as
// the buffer pipeline.
func TestNewLivePipeline_NilLogger_DefaultsToSlog(t *testing.T) {
	lp := NewLivePipeline(LiveConfig{}, &mockPipeline{}, nil)
	if lp.logger == nil {
		t.Error("logger nil after constructor; expected slog.Default")
	}
}

// TestLivePipeline_WithUsageAccumulator_FeedsFrames — accumulator gets every
// frame parsed by the reader goroutine.
func TestLivePipeline_WithUsageAccumulator_FeedsFrames(t *testing.T) {
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 100000}, &mockPipeline{}, nil)
	acc := &fakeAccumulator{}
	lp.WithUsageAccumulator(acc)

	in := makeOpenAISSE("a", "b")
	var out bytes.Buffer
	if _, err := lp.Process(context.Background(), strings.NewReader(in), &out, &core.HookInput{IngressType: "X"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if len(acc.events) < 3 { // 2 content + [DONE]
		t.Errorf("accumulator saw %d events, want >=3", len(acc.events))
	}
}

// TestLivePipeline_WithBodyCapture_RecordsRelayedBytes — capture buffer
// receives bytes sent to client.
func TestLivePipeline_WithBodyCapture_RecordsRelayedBytes(t *testing.T) {
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 100000}, &mockPipeline{}, nil).WithBodyCapture(1024)

	in := makeOpenAISSE("hello")
	var client bytes.Buffer
	if _, err := lp.Process(context.Background(), strings.NewReader(in), &client, &core.HookInput{IngressType: "X"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Contains(lp.CapturedBytes(), []byte("hello")) {
		t.Errorf("CapturedBytes missing 'hello': %q", lp.CapturedBytes())
	}
	if lp.CapturedTruncated() {
		t.Error("CapturedTruncated unexpectedly true")
	}
}

// TestLivePipeline_WithBodyCapture_Truncation — small cap forces truncation.
func TestLivePipeline_WithBodyCapture_Truncation(t *testing.T) {
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 100000}, &mockPipeline{}, nil).WithBodyCapture(5)

	in := makeOpenAISSE("hello", "world")
	var client bytes.Buffer
	if _, err := lp.Process(context.Background(), strings.NewReader(in), &client, &core.HookInput{IngressType: "X"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !lp.CapturedTruncated() {
		t.Error("CapturedTruncated false after overflow")
	}
}

// TestLivePipeline_NoCapture_CapturedNilAndFalse — guards mirror the
// buffer pipeline.
func TestLivePipeline_NoCapture_CapturedNilAndFalse(t *testing.T) {
	lp := NewLivePipeline(LiveConfig{}, &mockPipeline{}, nil)
	if lp.CapturedBytes() != nil {
		t.Error("CapturedBytes non-nil before WithBodyCapture")
	}
	if lp.CapturedTruncated() {
		t.Error("CapturedTruncated true before WithBodyCapture")
	}
}

// TestLivePipeline_MaxBufferExceeded — totalBytes > MaxBufferSize aborts
// with an error event to the client and a non-nil writerErr.
func TestLivePipeline_MaxBufferExceeded(t *testing.T) {
	lp := NewLivePipeline(LiveConfig{
		CheckpointChars: 100000,
		MaxBufferSize:   30, // tiny
	}, &mockPipeline{}, nil)

	in := makeOpenAISSE(
		"this string easily exceeds the 30-byte buffer cap once accumulated across frames")
	var client bytes.Buffer
	res, err := lp.Process(context.Background(), strings.NewReader(in), &client, &core.HookInput{IngressType: "X"})
	_ = res
	if err != nil {
		t.Fatalf("Process err = %v (expect nil; chunk error converts to client write)", err)
	}
	if !strings.Contains(client.String(), "blocked by policy") {
		t.Errorf("expected error envelope on client; got %q", client.String())
	}
}

// TestLivePipeline_NilPipelineResult_NoPanic — when checkpoint returns nil,
// flushCheckpoint short-circuits with Approve. Observed behavior today:
// pendingEvents are NOT forwarded to approvedChan on the nil-result branch
// (the comment claims "treat as approve" but the code only returns the
// decision — see live.go:212-215). This test pins the no-panic + final
// decision == Approve guarantee, the only contract the call site relies on.
func TestLivePipeline_NilPipelineResult_NoPanic(t *testing.T) {
	mp := &mockPipeline{
		decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
			return nil
		},
	}
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 3}, mp, nil)

	in := makeOpenAISSE("abcdef", "ghijkl")
	var out bytes.Buffer
	res, err := lp.Process(context.Background(), strings.NewReader(in), &out, &core.HookInput{IngressType: "X"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Decision != core.Approve {
		t.Errorf("decision = %+v, want Approve", res)
	}
}

// TestLivePipeline_SoftReject_FinalDecisionPropagates — checkpoint returns
// BlockSoft; events still flow, final result decision = BlockSoft.
func TestLivePipeline_SoftReject_FinalDecisionPropagates(t *testing.T) {
	var n int
	mp := &mockPipeline{
		decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
			n++
			if n == 1 {
				return &core.CompliancePipelineResult{Decision: core.BlockSoft, Reason: "soft"}
			}
			return &core.CompliancePipelineResult{Decision: core.Approve}
		},
	}
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 3}, mp, nil)

	in := makeOpenAISSE("abc", "def", "ghi", "jkl")
	var out bytes.Buffer
	res, err := lp.Process(context.Background(), strings.NewReader(in), &out, &core.HookInput{IngressType: "X"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Decision != core.BlockSoft {
		t.Errorf("decision = %+v, want BlockSoft (sticky once seen)", res)
	}
	// Content should still have been streamed.
	if !strings.Contains(out.String(), "abc") {
		t.Error("output missed content; soft reject must still flush")
	}
}

// TestLivePipeline_ReaderError_PropagatesToProcess — non-EOF reader error
// surfaces from Process.
func TestLivePipeline_ReaderError_PropagatesToProcess(t *testing.T) {
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 100000}, &mockPipeline{}, nil)

	// Oversized line triggers scanner error.
	long := strings.Repeat("a", maxSSELineSize+10)
	in := "data: " + long + "\n\n"

	var out bytes.Buffer
	_, err := lp.Process(context.Background(), strings.NewReader(in), &out, &core.HookInput{IngressType: "X"})
	if err == nil {
		t.Fatal("expected scanner error to surface")
	}
}

// TestLivePipeline_ClientWriteFails_DuringApproveStream — client.Write
// failure during streaming → Process returns the wrapped error.
func TestLivePipeline_ClientWriteFails_DuringApproveStream(t *testing.T) {
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 100000}, &mockPipeline{}, nil)
	fc := &failingClientWriter{failAfter: 0, failureErr: errors.New("client gone")}

	_, err := lp.Process(context.Background(), strings.NewReader(makeOpenAISSE("a", "b")), fc, &core.HookInput{IngressType: "X"})
	if err == nil {
		t.Fatal("expected write error to surface")
	}
}

// TestLivePipeline_ReplayWithFlushableClient_FlushesEachChunk — confirms
// Flush gets called.
func TestLivePipeline_ReplayWithFlushableClient_FlushesEachChunk(t *testing.T) {
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 100000}, &mockPipeline{}, nil)
	client := &recordingFlusherWriter{}
	if _, err := lp.Process(context.Background(), strings.NewReader(makeOpenAISSE("a", "b", "c")), client, &core.HookInput{IngressType: "X"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if client.flushes == 0 {
		t.Error("Flush never called on flusher-capable client")
	}
}

// TestLivePipeline_HardReject_WriterErrorOnError — when the hard-reject
// path tries to write the error envelope but the client errors, Process
// returns that error.
func TestLivePipeline_HardReject_WriterErrorOnError(t *testing.T) {
	mp := &mockPipeline{
		decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
			return &core.CompliancePipelineResult{Decision: core.RejectHard}
		},
	}
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 1}, mp, nil)
	fc := &failingClientWriter{failAfter: 0, failureErr: errors.New("client gone")}

	in := makeOpenAISSE("trigger")
	_, err := lp.Process(context.Background(), strings.NewReader(in), fc, &core.HookInput{IngressType: "X"})
	if err == nil {
		t.Fatal("expected write error from reject path")
	}
}

// TestSSEParser_InvalidRetryValue_Skipped — retry: must parse as int;
// invalid values log a warning and leave the field at -1.
func TestSSEParser_InvalidRetryValue_Skipped(t *testing.T) {
	in := "retry: not-an-int\ndata: x\n\n"
	p := NewSSEParser(strings.NewReader(in))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if evt.Retry != -1 {
		t.Errorf("Retry = %d, want -1 after parse failure", evt.Retry)
	}
	if evt.Data != "x" {
		t.Errorf("Data = %q, want x", evt.Data)
	}
}

// TestSSEParser_UnknownField_Skipped — unknown field name is dropped with
// a warning; surrounding fields are still emitted.
func TestSSEParser_UnknownField_Skipped(t *testing.T) {
	in := "bogus: ignore-me\ndata: real\n\n"
	p := NewSSEParser(strings.NewReader(in))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if evt.Data != "real" {
		t.Errorf("Data = %q, want real", evt.Data)
	}
}

// TestSSEParser_FieldWithoutColon — a line with no ':' becomes a field
// name with empty value; treated as unknown and skipped.
func TestSSEParser_FieldWithoutColon(t *testing.T) {
	in := "rawline\ndata: real\n\n"
	p := NewSSEParser(strings.NewReader(in))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if evt.Data != "real" {
		t.Errorf("Data = %q, want real", evt.Data)
	}
}

// TestSSEParser_FinalEvent_EmittedAtScannerEnd_WithRetryOnly — only a
// retry: line at the end yields a final event (covers the
// "hasData==false but retry!=-1" terminator arm).
func TestSSEParser_FinalEvent_EmittedAtScannerEnd_WithRetryOnly(t *testing.T) {
	in := "retry: 7"
	p := NewSSEParser(strings.NewReader(in))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if evt.Retry != 7 {
		t.Errorf("Retry = %d, want 7", evt.Retry)
	}
	if evt.Event != "message" {
		t.Errorf("Event = %q, want default 'message'", evt.Event)
	}
}

// TestSSEParser_FinalEvent_EmittedAtScannerEnd_WithIDOnly — same as above,
// covers the id-only branch.
func TestSSEParser_FinalEvent_EmittedAtScannerEnd_WithIDOnly(t *testing.T) {
	in := "id: abc"
	p := NewSSEParser(strings.NewReader(in))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if evt.ID != "abc" {
		t.Errorf("ID = %q, want abc", evt.ID)
	}
}

// TestSSEParser_FinalEvent_DoneAtScannerEnd — `data: [DONE]` without a
// trailing blank line still triggers the Done flag on the final event.
func TestSSEParser_FinalEvent_DoneAtScannerEnd(t *testing.T) {
	in := "data: [DONE]"
	p := NewSSEParser(strings.NewReader(in))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !evt.Done {
		t.Error("Done = false, want true")
	}
}

// TestSSEParser_EventOnlyWithBlank — `event: foo\n\n` (no data) — produces
// an event with default message? Actually our parser emits with the
// custom event type and no data, because eventType != "" → emitted.
func TestSSEParser_EventOnlyWithBlank(t *testing.T) {
	in := "event: custom\n\n"
	p := NewSSEParser(strings.NewReader(in))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if evt.Event != "custom" {
		t.Errorf("Event = %q, want custom", evt.Event)
	}
	if evt.Data != "" {
		t.Errorf("Data = %q, want empty", evt.Data)
	}
}

// errOnceReader emits a non-EOF error on first Read so bufio.Scanner
// surfaces it from scanner.Err().
type errOnceReader struct{ err error }

func (r *errOnceReader) Read(_ []byte) (int, error) { return 0, r.err }

// TestSSEParser_ScannerError_Surfaced — non-EOF scanner error surfaces
// from Next as the original error (not io.EOF).
func TestSSEParser_ScannerError_Surfaced(t *testing.T) {
	myErr := errors.New("upstream-broke")
	p := NewSSEParserWithLogger(&errOnceReader{err: myErr}, nil)
	_, err := p.Next()
	if err == nil || !errors.Is(err, myErr) {
		t.Errorf("err = %v, want %v", err, myErr)
	}
}

// TestExtractDeltaText_OpenAIWithChoices_ReturnsContent — happy path
// extraction from OpenAI chat-completion shape.
func TestExtractDeltaText_OpenAIWithChoices_ReturnsContent(t *testing.T) {
	evt := &SSEEvent{Data: `{"choices":[{"delta":{"content":"abc"}}]}`}
	if got := extractDeltaText(evt); got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
}

// TestExtractDeltaText_OpenAIEmptyChoices — JSON parses but `choices`
// is empty → return "".
func TestExtractDeltaText_OpenAIEmptyChoices(t *testing.T) {
	evt := &SSEEvent{Data: `{"choices":[]}`}
	if got := extractDeltaText(evt); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestExtractDeltaText_FallbackOnNonJSON — non-JSON Data is returned
// verbatim as the fallback text.
func TestExtractDeltaText_FallbackOnNonJSON(t *testing.T) {
	evt := &SSEEvent{Data: "raw plain text"}
	// json.Unmarshal will fail on this, so the fallback returns data.
	if got := extractDeltaText(evt); got != "raw plain text" {
		t.Errorf("got %q, want fallback verbatim", got)
	}
}

// TestExtractDeltaText_EmptyData — empty data returns empty.
func TestExtractDeltaText_EmptyData(t *testing.T) {
	if got := extractDeltaText(&SSEEvent{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestExtractDeltaText_DoneEvent — Done events return empty regardless of
// data content.
func TestExtractDeltaText_DoneEvent(t *testing.T) {
	if got := extractDeltaText(&SSEEvent{Done: true, Data: "x"}); got != "" {
		t.Errorf("got %q, want empty on Done", got)
	}
}

// TestPassthrough_PreCancelledCtx_ReturnsErr — Passthrough returns ctx.Err
// before reading.
func TestPassthrough_PreCancelledCtx_ReturnsErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Passthrough(ctx, strings.NewReader("payload"), io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestPassthrough_UpstreamReadError — non-EOF reader error is surfaced.
func TestPassthrough_UpstreamReadError(t *testing.T) {
	myErr := errors.New("upstream-fail")
	err := Passthrough(context.Background(), &errReader{err: myErr}, io.Discard)
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want %v", err, myErr)
	}
}

// TestPassthrough_ClientWriteError — client write error surfaces.
func TestPassthrough_ClientWriteError(t *testing.T) {
	myErr := errors.New("client-gone")
	err := Passthrough(context.Background(), strings.NewReader("hello"), &errWriter{err: myErr})
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want %v", err, myErr)
	}
}

// TestPassthrough_FlushCalled — flusher-capable client receives Flush.
func TestPassthrough_FlushCalled(t *testing.T) {
	client := &recordingFlusherWriter{}
	if err := Passthrough(context.Background(), strings.NewReader("hello"), client); err != nil {
		t.Fatalf("err = %v", err)
	}
	if client.flushes == 0 {
		t.Error("Flush never called")
	}
}

// TestPassthroughWithAccumulator_PreCancelledCtx — pre-cancelled ctx
// returns ctx.Err without sweeping bytes.
func TestPassthroughWithAccumulator_PreCancelledCtx(t *testing.T) {
	acc := NewUsageAccumulator("openai", "gpt-4o")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := PassthroughWithAccumulator(ctx, strings.NewReader("data: x\n\n"), io.Discard, acc)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestPassthroughWithAccumulator_ClientWriteError — client error aborts.
func TestPassthroughWithAccumulator_ClientWriteError(t *testing.T) {
	acc := NewUsageAccumulator("openai", "gpt-4o")
	myErr := errors.New("client-gone")
	err := PassthroughWithAccumulator(context.Background(), strings.NewReader("data: x\n\n"), &errWriter{err: myErr}, acc)
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want %v", err, myErr)
	}
}

// TestPassthroughWithAccumulator_UpstreamReadError — non-EOF read error.
func TestPassthroughWithAccumulator_UpstreamReadError(t *testing.T) {
	acc := NewUsageAccumulator("openai", "gpt-4o")
	myErr := errors.New("upstream-fail")
	err := PassthroughWithAccumulator(context.Background(), &errReader{err: myErr}, io.Discard, acc)
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want %v", err, myErr)
	}
}
