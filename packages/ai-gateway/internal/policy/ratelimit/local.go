package ratelimit

import (
	"sync"
	"time"
)

// LocalLimiter implements per-key sliding window rate limiting in process memory.
type LocalLimiter struct {
	mu      sync.Mutex
	windows map[string]*window
}

type window struct {
	timestamps []int64
}

// NewLocalLimiter creates an in-memory rate limiter.
func NewLocalLimiter() *LocalLimiter {
	return &LocalLimiter{windows: make(map[string]*window)}
}

// Allow checks whether a request identified by key is within the rate limit.
// limit is requests per window; windowMs is the window duration in milliseconds.
// Returns (allowed, retryAfterSec).
func (ll *LocalLimiter) Allow(key string, limit int, windowMs int64) (bool, int) {
	if limit <= 0 {
		return true, 0
	}

	ll.mu.Lock()
	defer ll.mu.Unlock()

	now := time.Now().UnixMilli()
	cutoff := now - windowMs

	w := ll.windows[key]
	if w == nil {
		w = &window{}
		ll.windows[key] = w
	}

	w.prune(cutoff)

	if len(w.timestamps) >= limit {
		oldest := w.timestamps[0]
		retryMs := oldest + windowMs - now
		retrySeconds := int(retryMs/1000) + 1
		if retrySeconds < 1 {
			retrySeconds = 1
		}
		return false, retrySeconds
	}

	w.timestamps = append(w.timestamps, now)
	return true, 0
}

func (w *window) prune(cutoff int64) {
	i := 0
	for i < len(w.timestamps) && w.timestamps[i] < cutoff {
		i++
	}
	if i > 0 {
		w.timestamps = w.timestamps[i:]
	}
}

// Cleanup removes windows that have no recent activity.
func (ll *LocalLimiter) Cleanup() {
	ll.mu.Lock()
	defer ll.mu.Unlock()

	cutoff := time.Now().UnixMilli() - 60_000
	for key, w := range ll.windows {
		w.prune(cutoff)
		if len(w.timestamps) == 0 {
			delete(ll.windows, key)
		}
	}
}
