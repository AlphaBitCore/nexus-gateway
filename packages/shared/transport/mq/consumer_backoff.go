package mq

import (
	"context"
	"time"
)

const (
	idleBackoffBase = 1 * time.Second
	idleBackoffMax  = 30 * time.Second
)

// idleBackoff returns the sleep after n consecutive empty fetch rounds: 0 for the
// first empty round (the 5s FetchMaxWait already paced it), then exponential up to
// a cap. A quiet consumer settles at idleBackoffMax between polls; the audit drain
// never reaches here (its rounds return data, resetting the streak).
func idleBackoff(n int) time.Duration {
	if n <= 1 {
		return 0
	}
	d := idleBackoffBase << (n - 2) // 1s, 2s, 4s, … from the 2nd empty round
	if d > idleBackoffMax {
		d = idleBackoffMax
	}
	return d
}

// sleepCtx sleeps for d (a no-op for d<=0) but returns false if ctx is cancelled
// first, so the caller can exit the consume loop promptly on shutdown.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
