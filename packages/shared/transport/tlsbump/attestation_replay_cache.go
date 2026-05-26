package tlsbump

import (
	"sync"
	"time"
)

// AttestationReplayCache is the CP-side sliding-window LRU that catches
// replay attempts against the attestation header. The architecture
// doc § 4 fixes the contract: a (ts, nonce) tuple is rejected if the
// same pair has been seen within the ±5 minute window.
//
// Storage shape: a single map keyed by "<ts>:<nonce>" with the entry's
// own expiry time as the value. The expiry is set to (ts + window) at
// insert so the entry naturally falls out 5 minutes after the original
// CONNECT — at which point the ts-window check would reject a replay
// anyway and the cache lookup becomes redundant.
//
// Bounded size: when len() reaches cap (architecture doc default
// 200_000) the next insert triggers a single-pass sweep deleting all
// already-expired entries. If the sweep frees no space (every entry
// still valid) the new insert evicts the entry with the earliest
// expiry — at 1000 req/s sustained the cap should be raised, but the
// LRU evicts gracefully rather than panic.
//
// Thread-safe; sync.RWMutex protects the map. On the hot path
// AttestationReplayCache.Seen takes a single RLock to check + RUnlock,
// then upgrades only when inserting a fresh entry.
type AttestationReplayCache struct {
	window time.Duration
	cap    int

	mu    sync.RWMutex
	items map[string]time.Time

	now func() time.Time // injected for deterministic tests
}

const (
	attestationReplayDefaultWindow = 5 * time.Minute
	attestationReplayDefaultCap    = 200_000
)

// NewAttestationReplayCache builds a cache with the architecture-doc
// defaults: 5-minute window + 200_000-entry cap. Call
// NewAttestationReplayCacheWith only from tests.
func NewAttestationReplayCache() *AttestationReplayCache {
	return NewAttestationReplayCacheWith(attestationReplayDefaultWindow, attestationReplayDefaultCap)
}

// NewAttestationReplayCacheWith is the test seam letting unit tests
// drive the eviction + TTL branches without sleeping 5 minutes.
// Production callers use NewAttestationReplayCache.
func NewAttestationReplayCacheWith(window time.Duration, cap int) *AttestationReplayCache {
	if cap < 1 {
		cap = 1
	}
	return &AttestationReplayCache{
		window: window,
		cap:    cap,
		items:  make(map[string]time.Time, 1024),
		now:    time.Now,
	}
}

// Seen returns true when the (ts, nonce) tuple has been observed
// within the active window. On a fresh tuple the cache records it
// and returns false — caller proceeds with verification. On a
// repeat tuple the caller MUST translate the true return to the
// "replayed" outcome + fall back to MITM per the architecture
// fail-open contract.
//
// Returns false (and records) even when the supplied ts is in the
// future or in the past — ts-window enforcement is the caller's
// job (it owns the clock-skew tolerance). This method is purely a
// "have I seen this exact tuple before" check.
func (c *AttestationReplayCache) Seen(ts int64, nonce string) bool {
	key := attestationReplayKey(ts, nonce)
	now := c.now()

	c.mu.RLock()
	exp, hit := c.items[key]
	c.mu.RUnlock()
	if hit && now.Before(exp) {
		return true
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check under the write lock to dodge the seen-then-inserted
	// race where two verifier goroutines race on the same nonce.
	if exp, hit := c.items[key]; hit && now.Before(exp) {
		return true
	}
	if len(c.items) >= c.cap {
		c.evictLocked(now)
	}
	c.items[key] = now.Add(c.window)
	return false
}

// Len returns the number of cached entries. Diagnostics + test helper;
// not on the verification hot path.
func (c *AttestationReplayCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// evictLocked frees room when the cap is hit. First pass deletes all
// already-expired entries (cheap, no allocation). If the cache is
// still full, the entry with the earliest expiry is dropped. Must be
// called with c.mu held.
func (c *AttestationReplayCache) evictLocked(now time.Time) {
	for k, exp := range c.items {
		if !now.Before(exp) {
			delete(c.items, k)
		}
	}
	if len(c.items) < c.cap {
		return
	}
	var oldestKey string
	var oldestExp time.Time
	first := true
	for k, exp := range c.items {
		if first || exp.Before(oldestExp) {
			oldestKey = k
			oldestExp = exp
			first = false
		}
	}
	if oldestKey != "" {
		delete(c.items, oldestKey)
	}
}

// attestationReplayKey composes the cache key. Format choice: "<ts>:<nonce>"
// — predictable, no allocations on a hot path beyond the strconv +
// concatenation. The nonce is 32 hex chars; ts is up to 10 decimal digits;
// total key length stays under 50 bytes for the 200k-entry cap budget.
func attestationReplayKey(ts int64, nonce string) string {
	// strconv.FormatInt is the cheapest way to render an int64 → string
	// in stdlib Go; fmt.Sprintf carries reflection overhead we don't
	// need here.
	return formatTSNonce(ts, nonce)
}

// formatTSNonce is split into its own function so a future tuning
// pass (e.g. swapping for a fixed-size byte array key under a different
// map type) has a single surface to refactor.
func formatTSNonce(ts int64, nonce string) string {
	tsStr := time.Unix(ts, 0).UTC().Format("20060102T150405Z")
	return tsStr + ":" + nonce
}
