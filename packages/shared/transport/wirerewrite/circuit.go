package wirerewrite

import (
	"sync"
	"time"
)

const (
	defaultCBThreshold = 10
	defaultCBWindow    = 60 * time.Second
)

// circuitBreaker trips open after threshold errors within window, then
// stays open until an explicit reset (triggered by a config Reload). This
// avoids silently corrupting upstream bytes after a recurring rule failure.
type circuitBreaker struct {
	mu        sync.Mutex
	errors    []time.Time
	open      bool
	threshold int
	window    time.Duration
}

func newCircuitBreaker() *circuitBreaker {
	return &circuitBreaker{
		threshold: defaultCBThreshold,
		window:    defaultCBWindow,
	}
}

// isOpen returns true when the circuit is tripped and calls should be skipped.
func (cb *circuitBreaker) isOpen() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.open
}

// recordError records one failure. Trips the circuit when the threshold
// within the sliding window is exceeded.
func (cb *circuitBreaker) recordError() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := time.Now()
	cb.errors = append(cb.errors, now)
	cb.purgeOldLocked(now)
	if len(cb.errors) >= cb.threshold {
		cb.open = true
	}
}

// reset clears the error history and closes the circuit. Called on Reload.
func (cb *circuitBreaker) reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.errors = cb.errors[:0]
	cb.open = false
}

// purgeOldLocked removes entries outside the sliding window. Must be called
// with mu held.
func (cb *circuitBreaker) purgeOldLocked(now time.Time) {
	cutoff := now.Add(-cb.window)
	n := 0
	for _, t := range cb.errors {
		if t.After(cutoff) {
			cb.errors[n] = t
			n++
		}
	}
	cb.errors = cb.errors[:n]
}
