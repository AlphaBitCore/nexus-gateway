package tlsbump

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// AttestationKeyLoader resolves an agent_id to its current Ed25519
// attestation public key. Implementations may pull from Hub via HTTP,
// from a thingclient shadow subscription, or from a local map (tests).
// The loader is allowed to block on IO; the cache holds no locks while
// it runs.
type AttestationKeyLoader func(ctx context.Context, agentID string) (ed25519.PublicKey, error)

// ErrUnknownAgent is the canonical loader-side miss. The CP verifier
// translates this into the `unknown_agent` Prometheus outcome label and
// reverts to normal MITM per the fail-open contract.
var ErrUnknownAgent = errors.New("attestation: agent_id not known to Hub")

const (
	attestationKeyDefaultTTL         = 60 * time.Second
	attestationKeyDefaultNegativeTTL = 5 * time.Second
	attestationKeyDefaultCap         = 1000
)

type cachedAttestationKey struct {
	key       ed25519.PublicKey
	expiresAt time.Time
	err       error // populated only for negative-cache entries
}

// AttestationKeyCache is a bounded TTL cache of (agent_id → Ed25519
// attestation public key). It is read by the CP CONNECT handler on the
// hot path of every attested request — Ed25519 verify cost dominates
// the check, so the cache stays lock-light (a single RWMutex on a map)
// and never makes IO under any lock.
//
// On miss the injected loader runs synchronously. Both positive and
// negative results are cached: a positive entry lives for ttl
// (default 60s); a loader error caches for negativeTTL (default 5s) to
// dampen scan-the-key-space attacks on bogus agent_ids without blinding
// the cache to a real rotation for more than a few seconds.
//
// Fail-open contract: every Get error MUST
// translate to "CP runs normal MITM" at the caller — never propagate
// as a 4xx/5xx to the original client, and never treat an error here
// as a security signal.
type AttestationKeyCache struct {
	loader      AttestationKeyLoader
	logger      *slog.Logger
	ttl         time.Duration
	negativeTTL time.Duration
	cap         int

	mu    sync.RWMutex
	items map[string]*cachedAttestationKey

	now func() time.Time // injected for deterministic tests
}

// NewAttestationKeyCache builds a cache wired to the given loader.
// Defaults: 60s positive TTL, 5s negative TTL, 1000-entry cap. The
// architecture doc § 4 fixes these — call NewAttestationKeyCacheWith
// only from tests.
func NewAttestationKeyCache(loader AttestationKeyLoader, logger *slog.Logger) *AttestationKeyCache {
	return NewAttestationKeyCacheWith(
		loader, logger,
		attestationKeyDefaultTTL,
		attestationKeyDefaultNegativeTTL,
		attestationKeyDefaultCap,
	)
}

// NewAttestationKeyCacheWith is the test seam letting unit tests drive
// the TTL and cap branches without sleeping for a minute. Production
// callers must use NewAttestationKeyCache; cap < 1 is clamped to 1 so
// the cache always holds at least the most-recent lookup.
func NewAttestationKeyCacheWith(
	loader AttestationKeyLoader,
	logger *slog.Logger,
	ttl, negativeTTL time.Duration,
	cap int,
) *AttestationKeyCache {
	if cap < 1 {
		cap = 1
	}
	return &AttestationKeyCache{
		loader:      loader,
		logger:      logger,
		ttl:         ttl,
		negativeTTL: negativeTTL,
		cap:         cap,
		items:       make(map[string]*cachedAttestationKey, cap),
		now:         time.Now,
	}
}

// Get returns the cached public key for agentID, calling the loader on
// miss. Errors (empty agentID, loader miss, expired negative entry)
// must translate to MITM fallback at the caller — never reject the
// request.
func (c *AttestationKeyCache) Get(ctx context.Context, agentID string) (ed25519.PublicKey, error) {
	if agentID == "" {
		return nil, errors.New("attestation: empty agent_id")
	}

	now := c.now()
	c.mu.RLock()
	entry, hit := c.items[agentID]
	c.mu.RUnlock()
	if hit && now.Before(entry.expiresAt) {
		if entry.err != nil {
			return nil, entry.err
		}
		return entry.key, nil
	}

	pub, loadErr := c.loader(ctx, agentID)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= c.cap {
		c.evictLocked(now)
	}
	if loadErr != nil {
		c.items[agentID] = &cachedAttestationKey{
			err:       loadErr,
			expiresAt: now.Add(c.negativeTTL),
		}
		if c.logger != nil {
			c.logger.Debug("attestation key cache: loader miss",
				"agent_id", agentID, "error", loadErr)
		}
		return nil, loadErr
	}
	c.items[agentID] = &cachedAttestationKey{
		key:       pub,
		expiresAt: now.Add(c.ttl),
	}
	return pub, nil
}

// Invalidate drops a single agent's cached entry. Called when Hub
// signals a key rotation or revocation via the shadow update path so
// the next CP verification picks up the fresh key without waiting for
// the TTL to elapse.
func (c *AttestationKeyCache) Invalidate(agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, agentID)
}

// Len returns the number of cached entries (positive + negative).
// Diagnostics + test helper; not used on the verification hot path.
func (c *AttestationKeyCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// evictLocked frees room in items when the cap is hit. First sweep:
// drop expired entries (cheap, the 60s TTL means most overflow is
// already-dead state). If the map is still at cap, evict the entry
// with the earliest expiry. O(n) but n ≤ cap (=1000), well inside the
// per-request budget. Must be called with c.mu held.
func (c *AttestationKeyCache) evictLocked(now time.Time) {
	for k, v := range c.items {
		if !now.Before(v.expiresAt) {
			delete(c.items, k)
		}
	}
	if len(c.items) < c.cap {
		return
	}
	var oldestKey string
	var oldestExp time.Time
	first := true
	for k, v := range c.items {
		if first || v.expiresAt.Before(oldestExp) {
			oldestKey = k
			oldestExp = v.expiresAt
			first = false
		}
	}
	if oldestKey != "" {
		delete(c.items, oldestKey)
	}
}
