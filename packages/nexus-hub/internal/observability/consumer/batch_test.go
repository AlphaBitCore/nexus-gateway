package consumer

import (
	"sync"
	"testing"
	"time"
)

func TestBatchAccumulator_FlushOnMaxSize(t *testing.T) {
	var mu sync.Mutex
	var flushed [][]int

	acc := NewBatchAccumulator[int](3, 10*time.Second, func(batch []int) error {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]int, len(batch))
		copy(cp, batch)
		flushed = append(flushed, cp)
		return nil
	})

	for i := 1; i <= 3; i++ {
		if err := acc.Add(i); err != nil {
			t.Fatalf("Add(%d) error: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(flushed) != 1 {
		t.Fatalf("expected 1 flush, got %d", len(flushed))
	}
	if len(flushed[0]) != 3 {
		t.Errorf("expected batch size 3, got %d", len(flushed[0]))
	}
}

func TestBatchAccumulator_FlushOnInterval(t *testing.T) {
	var mu sync.Mutex
	var flushed [][]int

	acc := NewBatchAccumulator[int](100, 50*time.Millisecond, func(batch []int) error {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]int, len(batch))
		copy(cp, batch)
		flushed = append(flushed, cp)
		return nil
	})

	if err := acc.Add(42); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(flushed) != 1 {
		t.Fatalf("expected 1 flush after interval, got %d", len(flushed))
	}
	if flushed[0][0] != 42 {
		t.Errorf("expected 42, got %d", flushed[0][0])
	}
}

func TestBatchAccumulator_Stop(t *testing.T) {
	var flushed []int

	acc := NewBatchAccumulator[int](100, 10*time.Second, func(batch []int) error {
		flushed = append(flushed, batch...)
		return nil
	})

	_ = acc.Add(1)
	_ = acc.Add(2)

	if err := acc.Stop(); err != nil {
		t.Fatalf("Stop error: %v", err)
	}

	if len(flushed) != 2 {
		t.Errorf("expected 2 flushed items, got %d", len(flushed))
	}
}

func TestBatchAccumulator_EmptyFlush(t *testing.T) {
	called := false
	acc := NewBatchAccumulator[int](10, time.Second, func(_ []int) error {
		called = true
		return nil
	})

	if err := acc.Flush(); err != nil {
		t.Fatalf("Flush error: %v", err)
	}
	if called {
		t.Error("flushFn should not be called on empty buffer")
	}
}

// TestBatchAccumulator_FlushDoesNotBlockAdds verifies that a concurrent Add
// is not serialized behind a long-running flushFn. Regression guard for I4.
func TestBatchAccumulator_FlushDoesNotBlockAdds(t *testing.T) {
	flushStarted := make(chan struct{})
	flushRelease := make(chan struct{})
	b := NewBatchAccumulator[int](2, time.Hour, func(_ []int) error {
		close(flushStarted)
		<-flushRelease
		return nil
	})

	if err := b.Add(1); err != nil {
		t.Fatalf("add 1: %v", err)
	}

	// Add(2) triggers flush; flushFn blocks on flushRelease. Run in a goroutine
	// so we keep the test goroutine free to drive the concurrent Add.
	trigger := make(chan error, 1)
	go func() { trigger <- b.Add(2) }()

	select {
	case <-flushStarted:
	case <-time.After(time.Second):
		t.Fatal("flushFn never started")
	}

	// While flushFn is blocked, a concurrent Add must not deadlock.
	addDone := make(chan error, 1)
	go func() { addDone <- b.Add(3) }()
	select {
	case err := <-addDone:
		if err != nil {
			t.Errorf("concurrent Add err: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("concurrent Add blocked behind flushFn")
	}

	close(flushRelease)

	// Drain the Add(2) goroutine so the test goroutine doesn't exit while the
	// child is still running inside the accumulator.
	select {
	case err := <-trigger:
		if err != nil {
			t.Errorf("Add(2) err: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Add(2) never returned")
	}
}

// TestBatchAccumulator_FlushFnPanicKeepsLockInvariant verifies that when
// flushFn panics, the mutex is re-acquired so the caller's defer Unlock
// does not trip a "sync: unlock of unlocked mutex" runtime panic, and the
// accumulator remains usable for subsequent operations. Regression guard for
// the panic-safety fix on top of I4.
func TestBatchAccumulator_FlushFnPanicKeepsLockInvariant(t *testing.T) {
	calls := 0
	b := NewBatchAccumulator[int](2, time.Hour, func(_ []int) error {
		calls++
		if calls == 1 {
			panic("boom")
		}
		return nil
	})

	if err := b.Add(1); err != nil {
		t.Fatalf("add 1: %v", err)
	}

	// Trigger first flush — flushFn panics. The Add goroutine's defer Unlock
	// must not see an unlocked mutex.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected flushFn panic to propagate through Add")
			}
		}()
		_ = b.Add(2)
	}()

	// The accumulator must still be usable.
	if err := b.Add(3); err != nil {
		t.Fatalf("add after panic: %v", err)
	}
	if err := b.Flush(); err != nil {
		t.Fatalf("flush after panic: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 flushFn invocations (1 panic + 1 success), got %d", calls)
	}
}
