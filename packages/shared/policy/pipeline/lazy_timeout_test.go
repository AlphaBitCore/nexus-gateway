// Package pipeline — the lazy per-hook deadline must be indistinguishable from
// the one it replaces.
//
// Named failure modes:
//   - Err() reads nil for a hook that ran past its deadline without ever
//     calling Done(), so HookTimeoutTotal silently reads zero
//   - a cancelled parent stops propagating
//   - arming twice produces two timers, or two different channels
package pipeline

import (
	"context"
	"testing"
	"time"
)

// eachImplementation runs a case against both the stdlib context and the lazy
// one. The differential form is the point: the claim is not "the lazy one
// behaves sensibly", it is "the lazy one behaves IDENTICALLY", and only a
// side-by-side comparison can say that.
func eachImplementation(t *testing.T, name string, body func(t *testing.T, mk func(context.Context, time.Duration) (context.Context, context.CancelFunc))) {
	t.Helper()
	t.Run(name+"/stdlib", func(t *testing.T) { body(t, context.WithTimeout) })
	t.Run(name+"/lazy", func(t *testing.T) { body(t, newLazyTimeout) })
}

func TestLazyTimeout_DeadlineIsReported(t *testing.T) {
	eachImplementation(t, "deadline", func(t *testing.T, mk func(context.Context, time.Duration) (context.Context, context.CancelFunc)) {
		before := time.Now()
		ctx, cancel := mk(context.Background(), 50*time.Millisecond)
		defer cancel()

		d, ok := ctx.Deadline()
		if !ok {
			t.Fatal("Deadline() reported no deadline")
		}
		if d.Before(before.Add(40*time.Millisecond)) || d.After(before.Add(80*time.Millisecond)) {
			t.Errorf("deadline %v is not ~50ms from %v", d, before)
		}
	})
}

// The case the metric depends on: the deadline passes and nobody ever selects
// on Done(), which is every pure-CPU hook.
func TestLazyTimeout_ErrAfterDeadlineWithoutDone(t *testing.T) {
	eachImplementation(t, "expired-unwatched", func(t *testing.T, mk func(context.Context, time.Duration) (context.Context, context.CancelFunc)) {
		ctx, cancel := mk(context.Background(), 10*time.Millisecond)
		defer cancel()

		if err := ctx.Err(); err != nil {
			t.Fatalf("Err() before the deadline = %v, want nil", err)
		}
		time.Sleep(30 * time.Millisecond)
		if err := ctx.Err(); err != context.DeadlineExceeded {
			t.Errorf("Err() past the deadline = %v, want DeadlineExceeded. executeOneHook reads "+
				"this to count HookTimeoutTotal, so nil here makes a real timeout invisible.", err)
		}
	})
}

func TestLazyTimeout_DoneClosesAtDeadline(t *testing.T) {
	eachImplementation(t, "done-fires", func(t *testing.T, mk func(context.Context, time.Duration) (context.Context, context.CancelFunc)) {
		ctx, cancel := mk(context.Background(), 20*time.Millisecond)
		defer cancel()

		select {
		case <-ctx.Done():
			t.Fatal("Done() was already closed before the deadline")
		case <-time.After(5 * time.Millisecond):
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
			t.Fatal("Done() never closed after the deadline")
		}
		if err := ctx.Err(); err != context.DeadlineExceeded {
			t.Errorf("Err() after Done() closed = %v, want DeadlineExceeded", err)
		}
	})
}

func TestLazyTimeout_ParentCancelPropagates(t *testing.T) {
	eachImplementation(t, "parent-cancel", func(t *testing.T, mk func(context.Context, time.Duration) (context.Context, context.CancelFunc)) {
		parent, cancelParent := context.WithCancel(context.Background())
		ctx, cancel := mk(parent, time.Hour) // deadline far away; the parent decides
		defer cancel()

		cancelParent()

		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
			t.Fatal("Done() did not close when the parent was cancelled")
		}
		if err := ctx.Err(); err != context.Canceled {
			t.Errorf("Err() after parent cancel = %v, want Canceled", err)
		}
	})
}

// Same as above but WITHOUT ever calling Done(), which is the unarmed path: a
// pure-CPU hook whose request is aborted mid-scan still has to see the
// cancellation when it finally checks.
func TestLazyTimeout_ParentCancelVisibleWithoutDone(t *testing.T) {
	eachImplementation(t, "parent-cancel-unwatched", func(t *testing.T, mk func(context.Context, time.Duration) (context.Context, context.CancelFunc)) {
		parent, cancelParent := context.WithCancel(context.Background())
		ctx, cancel := mk(parent, time.Hour)
		defer cancel()

		cancelParent()
		if err := ctx.Err(); err != context.Canceled {
			t.Errorf("Err() = %v, want Canceled — a hook that never selects on Done() still "+
				"reads Err(), and so does executeOneHook afterwards", err)
		}
	})
}

// A cancelled parent outranks an expired deadline, in both implementations.
func TestLazyTimeout_ParentCancelOutranksDeadline(t *testing.T) {
	eachImplementation(t, "cancel-outranks", func(t *testing.T, mk func(context.Context, time.Duration) (context.Context, context.CancelFunc)) {
		parent, cancelParent := context.WithCancel(context.Background())
		ctx, cancel := mk(parent, time.Millisecond)
		defer cancel()

		cancelParent()
		time.Sleep(20 * time.Millisecond) // the deadline has also passed now
		if err := ctx.Err(); err != context.Canceled {
			t.Errorf("Err() = %v, want Canceled (the parent was cancelled first)", err)
		}
	})
}

func TestLazyTimeout_RepeatedDoneReturnsTheSameChannel(t *testing.T) {
	eachImplementation(t, "done-idempotent", func(t *testing.T, mk func(context.Context, time.Duration) (context.Context, context.CancelFunc)) {
		ctx, cancel := mk(context.Background(), time.Hour)
		defer cancel()
		if a, b := ctx.Done(), ctx.Done(); a != b {
			t.Error("Done() returned two different channels — a hook selecting in a loop would " +
				"arm a new timer per iteration")
		}
	})
}

// Values must still resolve through to the parent: the pipeline puts request
// identity on the context that hooks read.
func TestLazyTimeout_ParentValuesResolve(t *testing.T) {
	type key struct{}
	eachImplementation(t, "values", func(t *testing.T, mk func(context.Context, time.Duration) (context.Context, context.CancelFunc)) {
		parent := context.WithValue(context.Background(), key{}, "carried")
		ctx, cancel := mk(parent, time.Hour)
		defer cancel()
		if got := ctx.Value(key{}); got != "carried" {
			t.Errorf("Value() = %v, want %q", got, "carried")
		}
	})
}

// BenchmarkPerHookTimeout is acceptance criterion (c): the allocation that
// motivated the change. The unarmed case is every pure-CPU hook; the armed case
// must not regress, since webhook-forward really does select on Done().
func BenchmarkPerHookTimeout(b *testing.B) {
	b.Run("stdlib/unwatched", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, _ = ctx.Deadline()
			_ = ctx.Err()
			cancel()
		}
	})
	b.Run("lazy/unwatched", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ctx, cancel := newLazyTimeout(context.Background(), time.Second)
			_, _ = ctx.Deadline()
			_ = ctx.Err()
			cancel()
		}
	})
	b.Run("stdlib/watched", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = ctx.Done()
			cancel()
		}
	})
	b.Run("lazy/watched", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ctx, cancel := newLazyTimeout(context.Background(), time.Second)
			_ = ctx.Done()
			cancel()
		}
	})
}

// TestLazyTimeout_CancelBeforeAnyDone is the divergence that produced a live
// leak rather than a wrong number.
//
// cancel() used to release only an ARMED timer. A hook that hands its context to
// a goroutine outliving Execute — webhook-forward's dialer does — arms after the
// deferred cancel has already run, so the arm installed a full-length deadline
// context whose timer nobody would ever release. Which is the exact allocation
// this type exists to avoid, reintroduced on the one path that uses Done().
func TestLazyTimeout_CancelBeforeAnyDone(t *testing.T) {
	eachImplementation(t, "cancel-before-done", func(t *testing.T, mk func(context.Context, time.Duration) (context.Context, context.CancelFunc)) {
		ctx, cancel := mk(context.Background(), time.Hour)
		cancel()

		if err := ctx.Err(); err != context.Canceled {
			t.Errorf("Err() = %v after cancel(), want Canceled — a context cancelled before "+
				"anyone looked must still report cancelled", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Error("Done() did not close after cancel(); the caller is now waiting out the full " +
				"timeout on a context that was released an hour early")
		}
	})
}

// TestLazyTimeout_ParentDeadlineWins pins the effective deadline. A child that
// reports a deadline LATER than its parent's is lying to whoever sizes an
// operation by asking — an HTTP client built from it would wait past the point
// where the whole request is already dead.
func TestLazyTimeout_ParentDeadlineWins(t *testing.T) {
	eachImplementation(t, "parent-deadline", func(t *testing.T, mk func(context.Context, time.Duration) (context.Context, context.CancelFunc)) {
		parent, cancelParent := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancelParent()
		pd, _ := parent.Deadline()

		ctx, cancel := mk(parent, time.Hour)
		defer cancel()

		d, ok := ctx.Deadline()
		if !ok {
			t.Fatal("Deadline() reported no deadline")
		}
		if d.After(pd) {
			t.Errorf("Deadline() = %v, later than the parent's %v — the child cannot outlive the "+
				"parent, so reporting a later deadline misleads every caller that asks", d, pd)
		}
	})
}
