package pipeline

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// lazyTimeoutContext is context.WithTimeout for a callee that will probably
// never look.
//
// safeHookExecute calls Execute SYNCHRONOUSLY, so a per-hook deadline cannot
// interrupt anything — it is a deadline the hook may honour or ignore. The
// built-in scanning hooks (pii-detector, keyword-filter, content-safety,
// rulepack-engine) are pure CPU and never read ctx.Done(); only webhook-forward
// does, through http.NewRequestWithContext. Every one of them was nonetheless
// charged a runtime timer, registered in the timer heap and cancelled
// microseconds later — per hook, per checkpoint, and a streamed response runs
// the response stage at every checkpoint.
//
// So the timer is armed on FIRST Done() and not before. Deadline() answers from
// a field. Err() derives from the clock when unarmed. A hook that never asks
// pays one small struct.
//
// Arming delegates to context.WithDeadline rather than building a channel and a
// watcher goroutine: from outside the context package there is no way to
// register with a parent cancelCtx's children, so a hand-rolled arm would need
// a goroutine per armed hook — strictly worse than today for the one hook that
// does use Done(). Delegating makes the armed path byte-for-byte the current
// behaviour and the unarmed path free.
type lazyTimeoutContext struct {
	context.Context // the parent

	// deadline is the EFFECTIVE deadline: ours, or the parent's when the parent
	// expires first. A child whose Deadline() reports later than its parent's is
	// lying to a caller that sizes an operation by what it is told, and a caller
	// that sizes an HTTP timeout from it would wait past the point where the
	// whole request is already dead.
	deadline time.Time

	once  sync.Once
	armed atomic.Pointer[armedTimeout]

	// cancelled records a cancel() that arrived while unarmed. Without it, a
	// cancel before any Done() left the context reporting nil forever, and a
	// later arm would install a full-length deadline nobody releases.
	cancelled atomic.Bool

	// terminal latches the first non-nil error observed, so the answer does not
	// change under a caller that asks twice. context.Context requires exactly
	// that stability, and the timeout metric reads Err() after Execute returns.
	terminal atomic.Pointer[error]
}

type armedTimeout struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// newLazyTimeout returns a context carrying `timeout` from now, plus the cancel
// that releases whatever the context ended up allocating. The cancel is
// mandatory, exactly as with context.WithTimeout: an armed context holds a
// runtime timer until it is called.
func newLazyTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if pd, ok := parent.Deadline(); ok && pd.Before(deadline) {
		deadline = pd
	}
	c := &lazyTimeoutContext{Context: parent, deadline: deadline}
	return c, c.cancel
}

// cancel releases the armed timer if there is one, and in either case puts the
// context into its terminal state.
//
// The unarmed half is the one that used to be missing: cancel() only fired when
// a timer existed, so a hook that handed its context to a goroutine outliving
// Execute — webhook-forward's dialer does — armed AFTER the deferred cancel had
// run and produced a live, full-length deadline context whose timer nobody
// releases. That is the exact allocation this type exists to avoid.
func (c *lazyTimeoutContext) cancel() {
	c.cancelled.Store(true)
	if a := c.armed.Load(); a != nil {
		a.cancel()
	}
}

// Deadline is the whole point: the common caller asks for it, gets it from a
// field, and never causes a timer to exist.
func (c *lazyTimeoutContext) Deadline() (time.Time, bool) { return c.deadline, true }

// Done arms the real deadline context and returns its channel. Idempotent: a
// hook that selects on Done() in a loop arms once.
func (c *lazyTimeoutContext) Done() <-chan struct{} {
	return c.arm().ctx.Done()
}

// Err reports the same thing context.WithTimeout would.
//
// The unarmed branch is the one that matters and the one a naive version gets
// wrong: executeOneHook reads Err() AFTER Execute returns, to decide whether to
// count a HookTimeoutTotal. A version that returned only the parent's error
// while unarmed would report nil for every hook that ran long and never looked
// — the timeout metric would silently read zero, which is indistinguishable
// from "nothing ever timed out".
//
// A cancelled parent outranks the deadline, matching the stdlib, where the
// child is cancelled by propagation the moment the parent is.
//
// The residual divergence, stated rather than hidden: when the deadline passes
// FIRST and the parent is cancelled afterwards, the stdlib latches
// DeadlineExceeded at the moment its timer fires and keeps it, while this
// reports Canceled if nobody asked in between. Recovering the true order needs
// something watching, which is the entire cost this type exists to avoid.
// Latching every observation narrows it to the case where no caller looked
// between the two events; there, the order is not recoverable by anyone.
//
// What it costs, so the choice is auditable: a hook that ran past its budget and
// whose client then disconnected is counted as a cancel rather than a timeout,
// so HookTimeoutTotal under-reports in exactly that overlap. The metric's
// previous failure was far larger — it read zero for every unarmed hook — and
// that is what the branches below fix.
func (c *lazyTimeoutContext) Err() error {
	if t := c.terminal.Load(); t != nil {
		return *t
	}
	if a := c.armed.Load(); a != nil {
		return c.latch(a.ctx.Err())
	}
	if err := c.Context.Err(); err != nil {
		return c.latch(err)
	}
	if !time.Now().Before(c.deadline) {
		return c.latch(context.DeadlineExceeded)
	}
	if c.cancelled.Load() {
		return c.latch(context.Canceled)
	}
	return nil
}

// latch stores the first non-nil error and returns it, so repeated calls agree.
func (c *lazyTimeoutContext) latch(err error) error {
	if err == nil {
		return nil
	}
	c.terminal.CompareAndSwap(nil, &err)
	return *c.terminal.Load()
}

// arm installs the real deadline context, once. A cancel that arrived while
// unarmed is applied immediately, so Done() after cancel() closes rather than
// handing back a channel that will not fire for the full timeout.
func (c *lazyTimeoutContext) arm() *armedTimeout {
	c.once.Do(func() {
		ctx, cancel := context.WithDeadline(c.Context, c.deadline)
		a := &armedTimeout{ctx: ctx, cancel: cancel}
		c.armed.Store(a)
		if c.cancelled.Load() {
			cancel()
		}
	})
	return c.armed.Load()
}
