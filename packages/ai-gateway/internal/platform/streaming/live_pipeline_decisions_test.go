package streaming

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// slowReader yields a chunk after Read is called once, then blocks until
// the embedded context is cancelled. This is the canonical way to force
// the reader goroutine's select to hit the <-ctx.Done() arm and the
// downstream main goroutine's MaxBufferSize early-exit (without a race).
type slowReader struct {
	first []byte
	done  bool
	block <-chan struct{}
}

func (r *slowReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		n := copy(p, r.first)
		return n, nil
	}
	<-r.block
	return 0, io.EOF
}

// errReader returns first some data then a non-EOF error to drive the
// reader-goroutine's "SSE read error" warn-log branch.
type errReader struct {
	first []byte
	done  bool
}

var errBoom = errors.New("simulated upstream read error")

func (r *errReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.first), nil
	}
	return 0, errBoom
}

// non-Flusher writer to take canFlush=false branches in Process.
type plainWriter struct {
	buf    bytes.Buffer
	header http.Header
	code   int
}

func newPlainWriter() *plainWriter {
	return &plainWriter{header: http.Header{}}
}
func (p *plainWriter) Header() http.Header         { return p.header }
func (p *plainWriter) Write(b []byte) (int, error) { return p.buf.Write(b) }
func (p *plainWriter) WriteHeader(code int)        { p.code = code }

// Zero-valued LiveConfig must populate the three default constants. This
// pins withDefaults's three default-setting branches that the rest of the
// test suite skips by always supplying explicit values.
func TestLiveConfig_WithDefaultsAppliesAllDefaults(t *testing.T) {
	got := (&LiveConfig{}).withDefaults()
	if got.FirstInspectChars != defaultFirstInspectChars {
		t.Errorf("FirstInspectChars default: want %d, got %d", defaultFirstInspectChars, got.FirstInspectChars)
	}
	if got.ReinspectStepChars != defaultReinspectStepChars {
		t.Errorf("ReinspectStepChars default: want %d, got %d", defaultReinspectStepChars, got.ReinspectStepChars)
	}
	if got.MaxBufferSize != defaultMaxStreamBufferSize {
		t.Errorf("MaxBufferSize default: want %d, got %d", defaultMaxStreamBufferSize, got.MaxBufferSize)
	}
}

// A pre-cancelled context must short-circuit the reader goroutine's
// loop on the first iteration without parsing any upstream bytes.
func TestLivePipeline_PreCancelledContextStopsReader(t *testing.T) {
	input := makeSSEStream(`{"choices":[{"delta":{"content":"never read"}}]}`)
	lp := NewLivePipeline(LiveConfig{FirstInspectChars: 1000}, approveStreamHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	blocked := lp.Process(ctx, strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("pre-cancelled stream should not be reported as blocked")
	}
	// Body may or may not contain a chunk depending on goroutine
	// interleaving — what matters is no panic / no goroutine leak,
	// surfaced as a clean Process return.
}

// A non-EOF read error from upstream must hit the "SSE read error" log
// path and return cleanly without blocking.
func TestLivePipeline_NonEOFReadErrorTearsDownCleanly(t *testing.T) {
	r := &errReader{first: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")}
	lp := NewLivePipeline(LiveConfig{FirstInspectChars: 1000}, approveStreamHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), r, rec, hookCtx)
	if blocked {
		t.Error("read error mid-stream should not be reported as `blocked` (blocked = compliance reject only)")
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Errorf("first chunk should have been written before error: %q", rec.Body.String())
	}
}

// Transform returning an error must skip the chunk (continue, no panic),
// not abort the stream. Pins the "chunk transform error" warn path.
func TestLivePipeline_TransformErrorSkipsChunk(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"first"}}]}`,
		`{"choices":[{"delta":{"content":"second"}}]}`,
	)
	calls := 0
	transform := func(data []byte) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transform boom")
		}
		return data, nil
	}
	lp := NewLivePipeline(LiveConfig{FirstInspectChars: 1000}, approveStreamHook, transform, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("transform error must not block the stream")
	}
	body := rec.Body.String()
	if strings.Contains(body, "first") {
		t.Errorf("first chunk should have been skipped by transform error, got %q", body)
	}
	if !strings.Contains(body, "second") {
		t.Errorf("second chunk should have been emitted, got %q", body)
	}
}

// A nil result from hookRun must be treated as Approve: held buffer
// flushes, no blocked decision. Pins the `res == nil` arm.
func TestLivePipeline_NilHookResultTreatedAsApprove(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"approved by nil"}}]}`,
	)
	nilHook := func(_ context.Context, _ *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		return nil
	}
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 5,
		HoldBack:          true,
	}, nilHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("nil hook result must not block")
	}
	if !strings.Contains(rec.Body.String(), "approved by nil") {
		t.Errorf("held buffer must flush after nil-result approval: %q", rec.Body.String())
	}
}

// OnCheckpoint callback must receive each pipeline result. Pins the
// `hookCtx.OnCheckpoint != nil` arm including its nil-result invocation.
func TestLivePipeline_OnCheckpointCallbackInvoked(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"long-enough body to trigger ckpt"}}]}`,
	)
	var (
		mu       sync.Mutex
		received []*goHooks.CompliancePipelineResult
	)
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 5,
	}, approveStreamHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{
		IngressType: "AI_GATEWAY",
		Path:        "/v1/chat/completions",
		OnCheckpoint: func(r *goHooks.CompliancePipelineResult) {
			mu.Lock()
			received = append(received, r)
			mu.Unlock()
		},
	}
	lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Fatal("OnCheckpoint must be invoked at least once")
	}
	if received[0] == nil || received[0].Decision != goHooks.Approve {
		t.Errorf("first checkpoint result mismatch: %+v", received[0])
	}
}

// OnStreamRewrite callback must fire exactly once with the rewritten
// slot count when Modify successfully rewrites the held buffer.
func TestLivePipeline_OnStreamRewriteFiresOnSuccessfulModify(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"card "}}]}`,
		`{"choices":[{"delta":{"content":"4111111111111111"}}]}`,
	)
	modifyHook := func(_ context.Context, in *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		segs := in.TextSegments()
		full := ""
		if len(segs) > 0 {
			full = segs[0]
		}
		return &goHooks.CompliancePipelineResult{
			Decision: goHooks.Modify,
			Reason:   "redact",
			ModifiedContent: []goHooks.ContentBlock{
				{Role: "assistant", Type: "text", Text: strings.ReplaceAll(full, "4111111111111111", "[REDACTED]")},
			},
		}
	}
	var (
		mu        sync.Mutex
		writtenCt []int
	)
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 20,
		HoldBack:          true,
	}, modifyHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{
		IngressType: "AI_GATEWAY",
		Path:        "/v1/chat/completions",
		OnStreamRewrite: func(n int) {
			mu.Lock()
			writtenCt = append(writtenCt, n)
			mu.Unlock()
		},
	}
	lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	mu.Lock()
	defer mu.Unlock()
	if len(writtenCt) != 1 || writtenCt[0] != 1 {
		t.Errorf("OnStreamRewrite expected to fire once with n=1, got %v", writtenCt)
	}
}

// BlockSoft decision must flush the buffer and continue (not blocked).
// Pins the BlockSoft case-arm previously uncovered.
func TestLivePipeline_BlockSoftContinuesWithFlush(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"soft-blocked content goes through"}}]}`,
	)
	softHook := func(_ context.Context, _ *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		return &goHooks.CompliancePipelineResult{Decision: goHooks.BlockSoft, Reason: "flag-only"}
	}
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 5,
		HoldBack:          true,
	}, softHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("BlockSoft must not block the stream")
	}
	if !strings.Contains(rec.Body.String(), "soft-blocked") {
		t.Errorf("BlockSoft must still flush pending content: %q", rec.Body.String())
	}
}

// An unknown Decision value must default-arm to "flush and continue"
// (no block, no rewrite). Pins the switch's `default:` branch.
func TestLivePipeline_UnknownDecisionDefaultsToContinue(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"unknown decision body"}}]}`,
	)
	weirdHook := func(_ context.Context, _ *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		return &goHooks.CompliancePipelineResult{Decision: goHooks.Decision("MADE_UP_DECISION")}
	}
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 5,
		HoldBack:          true,
	}, weirdHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("unknown decision must default to continue, not block")
	}
	if !strings.Contains(rec.Body.String(), "unknown decision") {
		t.Errorf("default arm must flush pending: %q", rec.Body.String())
	}
}

// Modify with no ModifiedContent and no pending must fall through to
// the "Modify skipped rewrite" log path (applied=false). Pins L260-266.
func TestLivePipeline_ModifyWithoutContentSkipsRewrite(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"original text"}}]}`,
	)
	emptyModifyHook := func(_ context.Context, _ *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		return &goHooks.CompliancePipelineResult{
			Decision:        goHooks.Modify,
			Reason:          "no replacement supplied",
			ModifiedContent: nil,
		}
	}
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 5,
		HoldBack:          true,
	}, emptyModifyHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("Modify with no content must not block")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "original text") {
		t.Errorf("Modify-without-content must passthrough original: %q", body)
	}
}

// Modify whose joined text is empty (e.g. only non-text blocks) must
// also fall through to the skipped-rewrite branch.
func TestLivePipeline_ModifyEmptyJoinedTextSkipsRewrite(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"keep me"}}]}`,
	)
	nonTextModifyHook := func(_ context.Context, _ *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		return &goHooks.CompliancePipelineResult{
			Decision: goHooks.Modify,
			Reason:   "non-text block",
			ModifiedContent: []goHooks.ContentBlock{
				{Role: "assistant", Type: "image", Text: "ignored-because-not-text"},
			},
		}
	}
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 5,
		HoldBack:          true,
	}, nonTextModifyHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if !strings.Contains(rec.Body.String(), "keep me") {
		t.Errorf("empty-text Modify should not destroy original payload: %q", rec.Body.String())
	}
}

// Modify after the buffer was already released must also skip the
// rewrite (released=true path of the inner guard).
func TestLivePipeline_ModifyAfterReleasedSkipsRewrite(t *testing.T) {
	// Two chunks. First triggers checkpoint #1 (Approve → release), second
	// triggers checkpoint #2 (Modify → released=true, must skip).
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"AAAAAAAAAA"}}]}`,
		`{"choices":[{"delta":{"content":"BBBBBBBBBB"}}]}`,
	)
	calls := 0
	mixedHook := func(_ context.Context, _ *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		calls++
		if calls == 1 {
			return &goHooks.CompliancePipelineResult{Decision: goHooks.Approve}
		}
		return &goHooks.CompliancePipelineResult{
			Decision: goHooks.Modify,
			Reason:   "late modify",
			ModifiedContent: []goHooks.ContentBlock{
				{Role: "assistant", Type: "text", Text: "should be skipped because already released"},
			},
		}
	}
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars:  5,
		ReinspectStepChars: 5,
		HoldBack:           true,
	}, mixedHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if blocked {
		t.Error("late Modify must not block")
	}
	body := rec.Body.String()
	if strings.Contains(body, "should be skipped because already released") {
		t.Errorf("late Modify after release must NOT rewrite the wire: %q", body)
	}
	if !strings.Contains(body, "AAAAAAAAAA") || !strings.Contains(body, "BBBBBBBBBB") {
		t.Errorf("both chunks must reach the client unmodified: %q", body)
	}
}

// closableBlockingReader is an io.Reader + io.Closer that yields
// `first` on the first Read then BLOCKS until Close. Drives the
// CloseUpstreamOnExit invocation on ai-gateway LivePipeline's
// error / reject branches (R-3 coverage fix from the 2nd-round
// architect review — every prior test used strings.Reader which has
// no Close method, so the type-assertion branch never fired in the
// ai-gateway call sites).
type closableBlockingReader struct {
	first   []byte
	yielded bool
	closed  chan struct{}
	closeN  int
}

func newClosableBlockingReader(first []byte) *closableBlockingReader {
	return &closableBlockingReader{first: first, closed: make(chan struct{})}
}

func (b *closableBlockingReader) Read(p []byte) (int, error) {
	if !b.yielded {
		b.yielded = true
		return copy(p, b.first), nil
	}
	<-b.closed
	return 0, io.EOF
}

func (b *closableBlockingReader) Close() error {
	b.closeN++
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

// TestLivePipeline_RejectHard_ClosesUpstream pins the R-3 coverage
// fix: ai-gateway's CloseUpstreamOnExit invocation at the RejectHard
// branch (live.go:285) was previously code-covered (count=1) but the
// inner Close call inside CloseUpstreamOnExit never fired because
// every test upstream was strings.Reader (not an io.Closer). This
// test passes a closableBlockingReader so the type-assertion branch
// DOES fire; the reader's blocked Read unblocks on Close and Process
// returns within the 2-second deadline. Without the close-on-reject
// fix the test would time out — pinning both code coverage and the
// behavioral wedge-prevention guarantee.
func TestLivePipeline_RejectHard_ClosesUpstream(t *testing.T) {
	rejectHook := func(_ context.Context, _ *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		return &goHooks.CompliancePipelineResult{Decision: goHooks.RejectHard, Reason: "test reject"}
	}
	upstream := newClosableBlockingReader([]byte(makeSSEStream(
		`{"choices":[{"delta":{"content":"long enough to trigger checkpoint"}}]}`,
	)))
	lp := NewLivePipeline(LiveConfig{FirstInspectChars: 5}, rejectHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	done := make(chan bool)
	go func() {
		done <- lp.Process(context.Background(), upstream, rec, hookCtx)
	}()
	select {
	case blocked := <-done:
		if !blocked {
			t.Errorf("RejectHard must report blocked=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not return within 2s after RejectHard on blocking upstream — wedge regression")
	}
	if upstream.closeN == 0 {
		t.Errorf("expected upstream.Close on RejectHard (R-3 coverage fix); got 0")
	}
}

// flushCountingWriter is an http.ResponseWriter + http.Flusher that
// tracks the order of Write and Flush calls. Used to assert the
// MaxBufferSize-overflow path flushes the error frame BEFORE
// cancel — a missing flush leaves the error in the kernel buffer and
// the SSE client sees a silent disconnect instead of the overflow
// signal.
type flushCountingWriter struct {
	buf    bytes.Buffer
	header http.Header
	events []string // appended on each Write ("w:<n>") and Flush ("f")
}

func newFlushCountingWriter() *flushCountingWriter {
	return &flushCountingWriter{header: http.Header{}}
}
func (f *flushCountingWriter) Header() http.Header { return f.header }
func (f *flushCountingWriter) Write(p []byte) (int, error) {
	n, err := f.buf.Write(p)
	f.events = append(f.events, "w")
	return n, err
}
func (f *flushCountingWriter) WriteHeader(_ int) {}
func (f *flushCountingWriter) Flush()            { f.events = append(f.events, "f") }

// Asserts the overflow path flushes the error frame before cancel.
// PR #24 follow-up R4: previously the compliance-block path flushed
// after WriteError but the buffer-overflow path did not, so on
// overflow the client could see a silent disconnect with the error
// frame stuck in the response buffer. The fix added a flush call;
// this test pins that fix by requiring at least one Flush event
// after the overflow Write.
func TestLivePipeline_MaxBufferSize_FlushesBeforeCancel(t *testing.T) {
	big := strings.Repeat("x", 200)
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"`+big+`"}}]}`,
		`{"choices":[{"delta":{"content":"`+big+`"}}]}`,
	)
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 10000,
		MaxBufferSize:     100,
	}, approveStreamHook, nil, slog.Default())
	w := newFlushCountingWriter()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), w, hookCtx)
	if !blocked {
		t.Fatal("oversized stream must be reported as blocked")
	}
	if !strings.Contains(w.buf.String(), "stream buffer exceeded") {
		t.Fatalf("client must receive buffer-exceeded SSE error frame: %q", w.buf.String())
	}
	// The error-frame write MUST be followed by a Flush event in the
	// same Write/Flush sequence; without it the kernel could hold the
	// error frame until the client times out.
	sawFlushAfterLastWrite := false
	for i := len(w.events) - 1; i >= 0; i-- {
		if w.events[i] == "f" {
			sawFlushAfterLastWrite = true
			break
		}
		if w.events[i] == "w" {
			break // hit a Write with no following Flush
		}
	}
	if !sawFlushAfterLastWrite {
		t.Errorf("overflow path must Flush after final Write; event sequence: %v", w.events)
	}
}

// Once cumulative upstream bytes exceed MaxBufferSize the pipeline must
// (a) write the buffer-exceeded SSE error, (b) cancel the stream, and
// (c) return blocked=true. Pins L280-286.
func TestLivePipeline_MaxBufferSizeAborts(t *testing.T) {
	big := strings.Repeat("x", 200)
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"`+big+`"}}]}`,
		`{"choices":[{"delta":{"content":"`+big+`"}}]}`,
	)
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 10000, // ensure no compliance checkpoint fires first
		MaxBufferSize:     100,   // tiny — first chunk's raw data alone exceeds
	}, approveStreamHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if !blocked {
		t.Fatal("oversized stream must be reported as blocked")
	}
	if !strings.Contains(rec.Body.String(), "stream buffer exceeded") {
		t.Errorf("client must receive buffer-exceeded SSE error frame: %q", rec.Body.String())
	}
}

// When the last accumulated chunk is short enough that no checkpoint
// fired during the loop, the post-loop `len(pendingText) > 0` branch
// runs the final checkpoint. Pins L314-317 (here with Approve →
// blocked stays false).
func TestLivePipeline_FinalCheckpointFiresOnLeftover(t *testing.T) {
	// FirstInspectChars > total content → no in-loop checkpoint → final-flush path.
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"abc"}}]}`,
	)
	var calls int
	countingHook := func(_ context.Context, _ *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		calls++
		return &goHooks.CompliancePipelineResult{Decision: goHooks.Approve}
	}
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 9999,
		HoldBack:          true,
	}, countingHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if calls != 1 {
		t.Errorf("expected exactly one final-flush checkpoint, got %d", calls)
	}
	if !strings.Contains(rec.Body.String(), "abc") {
		t.Errorf("leftover content must flush: %q", rec.Body.String())
	}
}

// Final-flush rejecting must set blocked=true and trip the
// `if runCheckpoint() { blocked = true }` arm at L315-317.
func TestLivePipeline_FinalCheckpointRejectBlocks(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"abc"}}]}`,
	)
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 9999,
		HoldBack:          true,
	}, rejectStreamHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	blocked := lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if !blocked {
		t.Fatal("final-flush reject must set blocked=true")
	}
	if !strings.Contains(rec.Body.String(), "blocked by compliance policy") {
		t.Errorf("client must receive the reject error frame: %q", rec.Body.String())
	}
}

// After checkpoint releases the buffer mid-stream (released=true,
// pendingText reset) and subsequent chunks emit immediately, the
// post-loop has pendingText==0 and pending==0. The "else if !blocked &&
// len(pending) > 0" branch (L318-320) only fires when no further
// checkpoint resets and there are leftovers. Construct a case where
// a 2nd chunk is too small to retrigger a checkpoint and HoldBack=false
// streams it directly: post-loop both pendingText and pending are zero,
// nothing fires. Use HoldBack=true so the 2nd chunk lands in pending
// without firing inspect, then exit drives `else if len(pending) > 0`.
func TestLivePipeline_PostLoopPendingFlush(t *testing.T) {
	// First chunk: long → triggers checkpoint (Approve) → flushed +
	// released=true. Second chunk: arrives, but since released=true and
	// HoldBack=true... wait — released path writes immediately, not pending.
	//
	// To hit the `len(pending) > 0` post-loop branch we need:
	// (a) checkpoint #1 was run (released=true), (b) hookRun is Modify
	// or default that flushPending → pending=nil pendingText="", and
	// (c) we somehow accumulate pending after that without retriggering
	// an inspect AND not via the "released || !HoldBack" branch.
	//
	// That combination is structurally unreachable in the current code:
	// after release, the "released || !HoldBack" else-branch writes
	// pending immediately, so pending can never grow >0 again without
	// pendingText also growing. Skip with a documented note rather than
	// fabricate a synthetic test that doesn't reflect runtime behavior.
	t.Skip("structurally unreachable: post-release new chunks always take the immediate-write else-arm (released=true) which leaves pending empty")
}

// NoDoneForAnthropicIngress test. EmitOpenAIDone true covered by
// PassThrough. No extra test needed.

// Use slowReader so the reader writes one chunk then blocks. The main
// goroutine's `for ch := range eventCh` consumes it. With a HoldBack and
// short FirstInspectChars we let the checkpoint run; the reader stays
// blocked on `<-r.block` until ctx cancel; the cancel propagates through
// the select arm `<-ctx.Done()` and `if ctx.Err() != nil` on next iter.
func TestLivePipeline_ContextCancelDuringRead(t *testing.T) {
	block := make(chan struct{})
	r := &slowReader{
		first: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"),
		block: block,
	}
	lp := NewLivePipeline(LiveConfig{FirstInspectChars: 1000}, approveStreamHook, nil, slog.Default())
	pw := newPlainWriter()
	hookCtx := &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- lp.Process(ctx, r, pw, hookCtx)
	}()
	// Give the goroutine a moment to start and consume the first chunk.
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(block) // unblock reader so it returns EOF
	select {
	case blocked := <-done:
		if blocked {
			t.Error("context cancel must not be reported as `blocked`")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not return after cancel")
	}
}
