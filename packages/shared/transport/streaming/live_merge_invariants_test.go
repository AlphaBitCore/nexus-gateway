package streaming

import (
	"bytes"
	"context"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Finding C-30 merged the reader goroutine and the per-frame channel into the delivery loop.
// The B6 review named three merge traps; these pin the two that are observable from
// behaviour. (The third — do not delete the exported CloseUpstreamOnExit — is pinned by the
// rewritten TestLivePipeline_WriterError_ClosesUpstream and by ai-gateway failing to build.)

// TestLivePipeline_NoGoroutineLeak is the invariant the merge INTRODUCED, and the reason the
// merge is worth having independently of the per-frame saving. Before it, a panic in the
// delivery loop unwound through `defer cancel()` but not through CloseUpstreamOnExit, and a
// reader parked in upstream.Read never observed ctx — so that goroutine and its pooled
// 64 KiB scan buffer leaked for the life of the process.
func TestLivePipeline_NoGoroutineLeak(t *testing.T) {
	settle := func() {
		for range 5 {
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
		}
	}
	settle()
	before := runtime.NumGoroutine()

	for range 20 {
		lp := NewLivePipeline(LiveConfig{CheckpointChars: 5}, nil, slog.Default())
		var out bytes.Buffer
		if _, err := lp.Process(context.Background(),
			strings.NewReader(makeOpenAISSE("a", "b", "c")), &out, nil); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}

	settle()
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines %d -> %d across 20 streams: Process is leaking. Inline parsing is "+
			"supposed to leave nothing running after it returns.", before, after)
	}
}

// TestLivePipeline_DoneTerminatesWithoutBlocking is MERGE TRAP 2. The old reader returned
// only AFTER sending [DONE], so the delivery loop processed it fully. Inline, [DONE] must be
// written and accumulated and only then break — and critically, the two audit skips must not
// be `continue`, or they would jump over that break and the loop would sit in parser.Next()
// on an upstream with nothing left to send.
func TestLivePipeline_DoneTerminatesWithoutBlocking(t *testing.T) {
	// blockingReader hands over the seed bytes, then BLOCKS on every later Read. If the loop
	// does not terminate on [DONE] it will park there and the deadline below fires.
	upstream := newBlockingReader([]byte(makeOpenAISSE("x", "y")))
	// MaxBufferSize: 1 so the audit cap trips on the FIRST frame. That is what makes this
	// test able to see the trap at all: the `continue` only bypasses the evt.Done break when
	// auditCapped is already true, so a generous buffer leaves that path dead and the test
	// inert. Verified by mutation — reinstating the `continue` makes this block and time out.
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 1000, MaxBufferSize: 1}, nil, slog.Default())

	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = lp.Process(context.Background(), upstream, &out, nil)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not return after [DONE]: the loop is blocked in parser.Next(). " +
			"An audit skip written as `continue` jumps over the evt.Done break.")
	}
	if !strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("[DONE] was not relayed to the client:\n%s", out.String())
	}
}

// TestLivePipeline_UsageFedPastTheAuditCap is MERGE TRAP 1, the highest-severity one. Feed
// used to live in the reader, so the delivery loop's audit-capped and MaxBufferSize skips
// could not affect it. If it is placed below either skip, every stream past MaxBufferSize
// stops feeding the accumulator partway — and since every tier-1 accumulator reads its
// counts from frames at the END of the stream, such streams lose provider-reported usage
// entirely and silently fall back to the tokenizer.
func TestLivePipeline_UsageFedPastTheAuditCap(t *testing.T) {
	acc := NewUsageAccumulator("openai", "gpt-4o")
	// MaxBufferSize is deliberately tiny so the cap trips almost immediately, then a usage
	// trailer arrives well after it.
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 1 << 20, MaxBufferSize: 64}, nil, slog.Default()).
		WithUsageAccumulator(acc)

	var sb strings.Builder
	for range 8 {
		sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("p", 40) + "\"}}]}\n\n")
	}
	sb.WriteString(`data: {"choices":[],"usage":{"prompt_tokens":13,"completion_tokens":5,"total_tokens":18}}` + "\n\n")
	sb.WriteString("data: [DONE]\n\n")

	var out bytes.Buffer
	if _, err := lp.Process(context.Background(), strings.NewReader(sb.String()), &out, nil); err != nil {
		t.Fatalf("Process: %v", err)
	}

	usage := acc.Finalize(context.Background())
	if usage.PromptTokens == nil || *usage.PromptTokens != 13 {
		got := "<nil>"
		if usage.PromptTokens != nil {
			got = string(rune('0' + *usage.PromptTokens%10))
		}
		t.Fatalf("PromptTokens = %s, want 13. The usage trailer arrived AFTER the audit cap "+
			"tripped, so Feed must sit above the audit skips; below them, every stream past "+
			"MaxBufferSize silently loses provider-reported usage.", got)
	}
}
