package cachelayer

import (
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// negVKCapacity bounds the virtual-key negative cache.
//
// The negative cache is deliberately a SEPARATE, much smaller LRU rather
// than negative entries stored in the positive VK cache. Unknown keys are
// attacker-controlled: any unauthenticated caller can mint an unlimited
// stream of distinct bad keys. Sharing one LRU would let that stream evict
// every resolved virtual key, converting a bad-key flood into a database
// amplification against legitimate traffic — strictly worse than the miss
// this fix removes. With a separate arena the blast radius of a spray is
// confined to the negative cache itself, and resolved keys keep their slots.
//
// It is a var rather than a const solely so the constructor's failure mode
// stays exercisable: a nil negative cache would panic on the first VK
// lookup, so newLayer must fail loudly instead of returning a half-built
// Layer. Production never assigns to it.
var negVKCapacity = 1024

// negativeVKHit reports whether hash is a known-absent virtual key whose
// negative entry is still within the TTL. Expired entries report a miss and
// are dropped; the next lookup re-reads the row.
//
// Peek, not Get: this runs on every VK resolution including the
// authenticated hot path, and Peek takes only a read lock. Recency ordering
// carries no meaning for negatives — eviction order among unknown keys is
// arbitrary by construction — so the write lock Get would take to bump
// recency is pure cost.
func (l *Layer) negativeVKHit(hash string) bool {
	recordedAt, ok := l.negVKeys.Peek(hash)
	if !ok {
		return false
	}
	if l.negVKTTL > 0 && l.now().Sub(recordedAt) > l.negVKTTL {
		l.negVKeys.Remove(hash)
		return false
	}
	return true
}

// recordNegativeVK marks hash as known-absent for negVKTTL.
//
// Freshness contract: negVKTTL is the same VKTTL that bounds the positive
// cache, so a virtual key created after the negative was recorded is
// honoured within the identical worst-case window the positive path already
// documents (cachelayer.Config.VKTTL, default 30s). In practice it is
// instant — the Hub pushes a `virtual_keys` invalidate carrying the new
// key's hash, and InvalidateVirtualKeys drops the negative entry alongside
// the positive one.
func (l *Layer) recordNegativeVK(hash string) {
	l.negVKeys.Add(hash, l.now())
}

// newNegativeVKCache builds the negative-cache arena. lru.New rejects a
// non-positive capacity; the error is propagated rather than discarded so
// a bad capacity fails construction instead of producing a Layer whose
// first VK lookup panics on a nil cache.
func newNegativeVKCache() (*lru.Cache[string, time.Time], error) {
	return lru.New[string, time.Time](negVKCapacity)
}
