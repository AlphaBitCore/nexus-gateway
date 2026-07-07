package bytebudget

import (
	"sync"
	"testing"
	"time"
)

// A full budget must block Acquire until Release frees space — the core
// back-pressure contract: the producer parks, then proceeds exactly when the
// consumer returns bytes.
func TestAcquire_BlocksAtBudgetAndWakesOnRelease(t *testing.T) {
	b := New(100, nil)
	if !b.Acquire(100) { // fills the budget exactly
		t.Fatal("first Acquire within budget must admit")
	}
	acquired := make(chan struct{})
	go func() {
		b.Acquire(50) // over budget → must park
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("Acquire admitted while the budget was exhausted")
	case <-time.After(50 * time.Millisecond):
	}
	b.Release(100)
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not wake after Release freed the budget")
	}
	if got := b.InUse(); got != 50 {
		t.Fatalf("InUse after acquire/release cycle = %d, want 50", got)
	}
}

// TryAcquire is the non-blocking admission: it must admit while there is room
// and refuse (without blocking) once the budget is exhausted, then admit again
// after Release.
func TestTryAcquire_NonBlockingAdmission(t *testing.T) {
	b := New(100, nil)
	if !b.TryAcquire(60) {
		t.Fatal("TryAcquire with room must admit")
	}
	if !b.TryAcquire(60) { // used=60 < budget=100 → admits (soft overshoot to 120)
		t.Fatal("TryAcquire below the budget line must admit even if n overshoots")
	}
	if b.TryAcquire(1) { // used=120 ≥ budget → refuse
		t.Fatal("TryAcquire on an exhausted budget must refuse")
	}
	b.Release(120)
	if !b.TryAcquire(1) {
		t.Fatal("TryAcquire after Release must admit again")
	}
}

// The soft budget admits whenever used < budget, so the overshoot is bounded by
// one in-flight acquisition per concurrent producer — never unbounded.
func TestAcquire_SoftOvershootBounded(t *testing.T) {
	b := New(100, nil)
	if !b.Acquire(99) || !b.Acquire(70) { // 99 < 100 → second admits, used=169
		t.Fatal("soft admission below the budget line must admit")
	}
	if got := b.InUse(); got != 169 {
		t.Fatalf("InUse = %d, want 169 (bounded overshoot)", got)
	}
	if b.TryAcquire(1) {
		t.Fatal("no further admission once used ≥ budget")
	}
}

// An item larger than the whole budget must be admitted once the pipeline is
// empty — a single oversized item can never deadlock the queue.
func TestAcquire_OversizedItemAdmittedWhenEmpty(t *testing.T) {
	b := New(100, nil)
	if !b.Acquire(500) {
		t.Fatal("oversized item on an empty pipeline must admit")
	}
	if b.TryAcquire(1) {
		t.Fatal("budget exhausted by the oversized item must refuse the next")
	}
	b.Release(500)
	if !b.Acquire(500) {
		t.Fatal("oversized item must admit again once drained to empty")
	}
}

// A closed stop channel must unblock a parked Acquire with false so shutdown
// never hangs a producer; the caller then handles the item without enqueuing.
func TestAcquire_StopEscapesWithFalse(t *testing.T) {
	stop := make(chan struct{})
	b := New(100, stop)
	b.Acquire(100)
	res := make(chan bool, 1)
	go func() { res <- b.Acquire(1) }()
	select {
	case <-res:
		t.Fatal("Acquire returned before stop fired on a full budget")
	case <-time.After(50 * time.Millisecond):
	}
	close(stop)
	select {
	case ok := <-res:
		if ok {
			t.Fatal("Acquire after stop must return false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not unblock on stop")
	}
}

// Even when the Release signal races an about-to-park acquirer (the notFull
// nudge can be consumed before the acquirer waits), the WakePollInterval
// re-check must recover — no missed-wakeup wedge. Hammer the handoff to give a
// real race a chance to bite under -race.
func TestAcquireRelease_NoMissedWakeupUnderChurn(t *testing.T) {
	b := New(64, nil)
	const producers = 8
	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(producers)
	for range producers {
		go func() {
			defer wg.Done()
			for range rounds {
				if !b.Acquire(16) {
					t.Error("Acquire returned false with no stop channel")
					return
				}
				b.Release(16)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("producers wedged — missed wakeup")
	}
	if got := b.InUse(); got != 0 {
		t.Fatalf("InUse after balanced acquire/release churn = %d, want 0", got)
	}
}

// A non-positive budget disables back-pressure entirely: every Acquire admits,
// Release is a no-op — the off switch a caller wires by config.
func TestDisabledBudget_AlwaysAdmits(t *testing.T) {
	b := New(0, nil)
	if !b.Acquire(1<<40) || !b.TryAcquire(1<<40) {
		t.Fatal("disabled budget must always admit")
	}
	b.Release(1 << 40) // must not underflow anything
	if got := b.InUse(); got != 0 {
		t.Fatalf("disabled budget InUse = %d, want 0 (accounting off)", got)
	}
}

// Nil receivers are safe no-ops so an unwired consumer (tests constructing the
// owner struct directly) never panics or blocks.
func TestNilBudget_SafeNoOps(t *testing.T) {
	var b *Budget
	if !b.Acquire(10) || !b.TryAcquire(10) {
		t.Fatal("nil budget must admit")
	}
	b.Release(10)
	if b.InUse() != 0 || b.Budget() != 0 {
		t.Fatal("nil budget must report zero usage and zero ceiling")
	}
}

// Budget reports the configured ceiling; Release(0) must not emit a wake signal
// (n==0 short-circuits before the atomic).
func TestBudgetAccessorsAndZeroRelease(t *testing.T) {
	b := New(4096, nil)
	if b.Budget() != 4096 {
		t.Fatalf("Budget() = %d, want 4096", b.Budget())
	}
	b.Release(0)
	if got := b.InUse(); got != 0 {
		t.Fatalf("InUse after Release(0) = %d, want 0", got)
	}
}
