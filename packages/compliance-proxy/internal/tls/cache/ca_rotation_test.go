package cache

import (
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
)

// A rotated CA must not be able to reach the previous CA's entries. Before the
// key carried the CA fingerprint, every hostname's entry survived the rotation
// holding a leaf signed by the old CA and a key encrypted under the old DEK —
// one wasted Redis round-trip and one alarming WARN per hostname, until the
// re-mint overwrote it. Measured on a live rotation before the fix; this pins
// that it cannot recur.
func TestRedisKey_IsScopedToTheCA(t *testing.T) {
	installFreshMetricsRegistry()
	t.Cleanup(resetMetricsForCertTests)
	s, rdb := newTestRedis(t)
	before := NewCertCache(newIssuerForCacheTests(t), NewLRUCache(10), rdb, time.Hour, discardLogger())
	after := NewCertCache(newIssuerForCacheTests(t), NewLRUCache(10), rdb, time.Hour, discardLogger())

	oldKey := before.redisKey("api.example.com")
	newKey := after.redisKey("api.example.com")
	if oldKey == newKey {
		t.Fatalf("two CAs produced the same cache key %q — a rotation would inherit the old entry", oldKey)
	}
	if !strings.HasPrefix(oldKey, redisKeyPrefix) || !strings.HasSuffix(oldKey, ":api.example.com") {
		t.Errorf("key %q must stay readable: prefix + CA scope + hostname", oldKey)
	}

	// Populate under the first CA, then read through the second: it must be an
	// ordinary miss that mints, not a decrypt failure.
	if _, err := before.GetCertByHostname("api.example.com"); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	if s.Exists(newKey) {
		t.Fatal("the rotated CA's key must not exist yet")
	}
	metrics.RedisAvailable.With().Set(0) // prove the read path resets it
	if _, err := after.GetCertByHostname("api.example.com"); err != nil {
		t.Fatalf("post-rotation mint: %v", err)
	}
	if got := readGauge(t, metrics.RedisAvailable); got != 1 {
		t.Errorf("redis.available = %v after a post-rotation miss; want 1 — Redis was reachable throughout", got)
	}
	if !s.Exists(newKey) {
		t.Error("the post-rotation mint must be cached under the new CA's key")
	}
	if !s.Exists(oldKey) {
		t.Error("the previous CA's entry is left to expire on its TTL, not deleted")
	}
}

// An entry that Redis serves correctly but this process cannot decrypt is a
// KEY-MATERIAL condition, not an availability one. Zeroing the gauge here would
// page an operator about Redis when Redis is healthy — and with CA-scoped keys
// this can now only mean the encryption key changed under a stable CA.
func TestRedisGet_UndecryptableEntryDoesNotBlameRedis(t *testing.T) {
	installFreshMetricsRegistry()
	t.Cleanup(resetMetricsForCertTests)
	s, rdb := newTestRedis(t)
	victim := NewCertCache(newIssuerForCacheTests(t), NewLRUCache(10), rdb, time.Hour, discardLogger())
	stranger := NewCertCache(newIssuerForCacheTests(t), NewLRUCache(10), rdb, time.Hour, discardLogger())

	// Mint through the stranger, then move its entry under the victim's key so the
	// victim finds a well-formed entry it cannot decrypt.
	if _, err := stranger.GetCertByHostname("swap.example.com"); err != nil {
		t.Fatalf("stranger mint: %v", err)
	}
	blob, err := s.Get(stranger.redisKey("swap.example.com"))
	if err != nil {
		t.Fatalf("read stranger entry: %v", err)
	}
	if err := s.Set(victim.redisKey("swap.example.com"), blob); err != nil {
		t.Fatalf("seed victim slot: %v", err)
	}

	metrics.RedisAvailable.With().Set(1)
	cert, err := victim.GetCertByHostname("swap.example.com")
	if err != nil {
		t.Fatalf("an undecryptable entry must fall through to a fresh mint, got: %v", err)
	}
	if cert == nil {
		t.Fatal("want a freshly minted certificate")
	}
	if got := readGauge(t, metrics.RedisAvailable); got != 1 {
		t.Errorf("redis.available = %v; want 1 — Redis answered, the entry was simply not ours", got)
	}
}

// A cache with no issuer still produces a well-formed key rather than one that
// could collide with a real CA scope.
func TestCAScope_WithoutAnIssuer(t *testing.T) {
	c := &CertCache{}
	if got := c.caScope(); got != "nocert" {
		t.Errorf("caScope() = %q, want %q", got, "nocert")
	}
	if got := c.redisKey("h"); got != redisKeyPrefix+"nocert:h" {
		t.Errorf("redisKey = %q", got)
	}
}
