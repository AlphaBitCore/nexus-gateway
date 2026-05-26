package streamcache

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

func TestRingBuffer_AppendAndReadInOrder(t *testing.T) {
	rb := NewRingBuffer()
	rb.Append(provcore.Chunk{Delta: "a"})
	rb.Append(provcore.Chunk{Delta: "b"})
	rb.AppendTerminal(provcore.Chunk{Done: true})

	ctx := context.Background()
	var got []string
	var doneSeen bool
	idx := 0
	for {
		chunk, next, err := rb.Read(ctx, idx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Delta != "" {
			got = append(got, chunk.Delta)
		}
		if chunk.Done {
			doneSeen = true
		}
		idx = next
	}
	if !doneSeen {
		t.Fatal("Done chunk not consumed before EOF")
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %v", got)
	}
}

func TestRingBuffer_LateSubscriberCatchesUp(t *testing.T) {
	rb := NewRingBuffer()
	rb.Append(provcore.Chunk{Delta: "early"})
	rb.Append(provcore.Chunk{Delta: "middle"})

	// Late subscriber starts at idx=0; should see all chunks.
	ctx := context.Background()
	chunk, next, err := rb.Read(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Delta != "early" || next != 1 {
		t.Fatalf("expected early/1, got %s/%d", chunk.Delta, next)
	}
	chunk, next, err = rb.Read(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Delta != "middle" || next != 2 {
		t.Fatalf("expected middle/2, got %s/%d", chunk.Delta, next)
	}
}

func TestRingBuffer_BlockingReadWakesOnAppend(t *testing.T) {
	rb := NewRingBuffer()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	var got string
	go func() {
		defer wg.Done()
		chunk, _, err := rb.Read(ctx, 0)
		if err != nil {
			t.Errorf("Read err: %v", err)
			return
		}
		got = chunk.Delta
	}()

	// Give the reader time to park on the empty buffer.
	time.Sleep(50 * time.Millisecond)
	rb.Append(provcore.Chunk{Delta: "later"})
	wg.Wait()

	if got != "later" {
		t.Fatalf("got %s", got)
	}
}

func TestRingBuffer_FailBroadcasts(t *testing.T) {
	rb := NewRingBuffer()
	rb.Append(provcore.Chunk{Delta: "a"})
	failErr := &provcore.ProviderError{Code: provcore.CodeUpstreamError, Message: "boom"}
	rb.Fail(failErr)

	ctx := context.Background()
	// Reading past the appended chunks should return the failure.
	_, _, err := rb.Read(ctx, 1)
	pe := &provcore.ProviderError{}
	ok := errors.As(err, &pe)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T: %v", err, err)
	}
	if pe.Code != provcore.CodeUpstreamError {
		t.Fatalf("expected upstream_error, got %s", pe.Code)
	}
	// But chunks already in the buffer should still be reachable.
	chunk, _, err := rb.Read(ctx, 0)
	if err != nil {
		t.Fatalf("expected to read appended chunk before fail, got err %v", err)
	}
	if chunk.Delta != "a" {
		t.Fatalf("got %s", chunk.Delta)
	}
}

func TestRingBuffer_FailWakesParkedReader(t *testing.T) {
	rb := NewRingBuffer()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	failErr := &provcore.ProviderError{Code: provcore.CodeUpstreamError, Message: "boom"}

	resultCh := make(chan error, 1)
	go func() {
		_, _, err := rb.Read(ctx, 0)
		resultCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	rb.Fail(failErr)

	select {
	case err := <-resultCh:
		pe := &provcore.ProviderError{}
		ok := errors.As(err, &pe)
		if !ok {
			t.Fatalf("expected ProviderError, got %T", err)
		}
		if pe.Code != provcore.CodeUpstreamError {
			t.Fatalf("got code %s", pe.Code)
		}
	case <-ctx.Done():
		t.Fatal("parked reader not woken by Fail")
	}
}

func TestRingBuffer_ContextCancelWakesParkedReader(t *testing.T) {
	rb := NewRingBuffer()
	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan error, 1)
	go func() {
		_, _, err := rb.Read(ctx, 0)
		resultCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel did not wake parked reader")
	}
}

func TestRingBuffer_SnapshotReturnsCopy(t *testing.T) {
	rb := NewRingBuffer()
	rb.Append(provcore.Chunk{Delta: "a"})
	rb.AppendTerminal(provcore.Chunk{Done: true})

	snap := rb.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(snap))
	}
	// Mutate the snapshot; the buffer should be unaffected.
	snap[0].Delta = "MUTATED"

	// Read again from the buffer directly.
	ctx := context.Background()
	chunk, _, err := rb.Read(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Delta != "a" {
		t.Fatalf("snapshot mutation leaked into buffer: got %s", chunk.Delta)
	}
}

func TestRingBuffer_AppendAfterTerminalIsNoop(t *testing.T) {
	rb := NewRingBuffer()
	rb.AppendTerminal(provcore.Chunk{Done: true})
	rb.Append(provcore.Chunk{Delta: "post-terminal"}) // should be silently dropped

	ctx := context.Background()
	chunk, next, err := rb.Read(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !chunk.Done {
		t.Fatal("first chunk should be terminal Done")
	}
	_, _, err = rb.Read(ctx, next)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after terminal, got %v", err)
	}
}

// TestRingBuffer_AppendTerminalAfterTerminalIsNoop covers the
// (r.done || r.err != nil) guard in AppendTerminal: a second
// AppendTerminal call must not append, must not panic. Observable: the
// buffer still contains exactly the first terminal chunk and Read past
// it returns io.EOF.
func TestRingBuffer_AppendTerminalAfterTerminalIsNoop(t *testing.T) {
	rb := NewRingBuffer()
	rb.AppendTerminal(provcore.Chunk{Done: true, Delta: "first"})
	rb.AppendTerminal(provcore.Chunk{Done: true, Delta: "second"}) // dropped

	snap := rb.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected exactly 1 chunk after double terminal, got %d", len(snap))
	}
	if snap[0].Delta != "first" {
		t.Errorf("expected first terminal preserved, got %q", snap[0].Delta)
	}
}

// TestRingBuffer_AppendTerminalAfterFailIsNoop covers the second arm
// of the same guard. Once Fail has fired, AppendTerminal must not
// flip the buffer to "done" — readers past the failure point still
// see the error.
func TestRingBuffer_AppendTerminalAfterFailIsNoop(t *testing.T) {
	rb := NewRingBuffer()
	failErr := &provcore.ProviderError{Code: provcore.CodeUpstreamError, Message: "boom"}
	rb.Fail(failErr)
	rb.AppendTerminal(provcore.Chunk{Done: true}) // dropped

	ctx := context.Background()
	_, _, err := rb.Read(ctx, 0)
	pe := &provcore.ProviderError{}
	if !errors.As(err, &pe) || pe.Code != provcore.CodeUpstreamError {
		t.Fatalf("expected ProviderError to survive AppendTerminal-after-Fail, got %v", err)
	}
}

// TestRingBuffer_FailAfterTerminalIsNoop covers the (r.done) arm of
// the Fail guard. Once AppendTerminal completed, a subsequent Fail
// must not overwrite the io.EOF outcome.
func TestRingBuffer_FailAfterTerminalIsNoop(t *testing.T) {
	rb := NewRingBuffer()
	rb.AppendTerminal(provcore.Chunk{Done: true})
	rb.Fail(&provcore.ProviderError{Code: provcore.CodeUpstreamError, Message: "boom"}) // dropped

	ctx := context.Background()
	// Consume the terminal chunk, then verify Read past it returns EOF, not the would-be error.
	_, next, err := rb.Read(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = rb.Read(ctx, next)
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF after AppendTerminal (Fail must be dropped), got %v", err)
	}
}

// TestRingBuffer_FailAfterFailIsNoop covers the (r.err != nil) arm of
// the Fail guard. The first error wins; the second Fail must not
// replace it.
func TestRingBuffer_FailAfterFailIsNoop(t *testing.T) {
	rb := NewRingBuffer()
	first := errors.New("first")
	second := errors.New("second")
	rb.Fail(first)
	rb.Fail(second) // dropped

	ctx := context.Background()
	_, _, err := rb.Read(ctx, 0)
	if !errors.Is(err, first) {
		t.Errorf("expected first error preserved, got %v", err)
	}
}
