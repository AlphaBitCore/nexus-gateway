package executor

import (
	"context"
	"math/rand"
	"sync"
	"time"

	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

var (
	backoffRandMu sync.Mutex
	backoffRand   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// computeBackoff returns how long a target waits before its next dispatch.
//
// waits is 1-based and counts the waits this TARGET has already been made to
// serve, across every turn it has had — not the position within one turn. A
// target gets one escalating schedule, because the thing being backed off is
// the target: a walk that came back to it after two in-turn retries has to wait
// longer than it did the first time, and an index that restarts per turn hands
// it a SHORTER pause than the one it has already outlasted.
//
// Doubles BackoffInitial until reaching BackoffMax, then applies uniform
// ±BackoffJitter jitter. Returns 0 (never negative) on extreme jitter.
func computeBackoff(waits int, p cfgpolicy.RetryPolicy) time.Duration {
	if waits < 1 {
		waits = 1
	}
	base := p.BackoffInitial
	for i := 1; i < waits; i++ {
		base *= 2
		if base >= p.BackoffMax {
			base = p.BackoffMax
			break
		}
	}
	if base > p.BackoffMax {
		base = p.BackoffMax
	}
	if p.BackoffJitter > 0 {
		backoffRandMu.Lock()
		delta := float64(base) * p.BackoffJitter
		base += time.Duration(backoffRand.Float64()*2*delta - delta)
		backoffRandMu.Unlock()
	}
	if base < 0 {
		return 0
	}
	return base
}

// waitOut serves the pause a target owes before the walk returns to it.
//
// proceed is false when the caller's deadline arrives before the pause would
// end: sleeping past it hands them a context error instead of the answer the
// walk already has. ctxErr is non-nil only when the caller went away mid-pause,
// which is a different outcome — there is nobody left to answer.
func waitOut(ctx context.Context, w *walkState, p cfgpolicy.RetryPolicy) (proceed bool, ctxErr error) {
	backoff := w.nextBackoff(p)
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= backoff {
		return false, nil
	}
	select {
	case <-time.After(backoff):
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
