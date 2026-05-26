package cache

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/issuer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// newTestRedis returns an in-memory miniredis instance + a connected
// go-redis client. The miniredis SCRIPT/EVALSHA surface is unused here —
// we only need GET/SET, which miniredis supports natively.
func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return s, rdb
}

// newIssuerForCacheTests builds a real Issuer backed by a freshly generated
// CA so cache encryption + cert chain assertions are end-to-end.
func newIssuerForCacheTests(t *testing.T) *issuer.Issuer {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := issuer.NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss
}

// discardLogger silences logger output during tests; tests assert on
// observable side effects (metrics, return values), not log lines.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func setupTestCache(t *testing.T) *CertCache {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := issuer.NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	lru := NewLRUCache(100)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// No Redis client (nil) - tests run without Redis
	return NewCertCache(iss, lru, nil, 1*time.Hour, logger)
}

func TestCertCache_LRUHit(t *testing.T) {
	cache := setupTestCache(t)

	// First call signs a new cert and stores in LRU
	cert1, err := cache.GetCertByHostname("lru-hit.example.com")
	if err != nil {
		t.Fatalf("first GetCertByHostname: %v", err)
	}
	if cert1 == nil {
		t.Fatal("expected non-nil cert")
	}

	// Second call should hit LRU (same pointer)
	cert2, err := cache.GetCertByHostname("lru-hit.example.com")
	if err != nil {
		t.Fatalf("second GetCertByHostname: %v", err)
	}
	if cert1 != cert2 {
		t.Error("expected same cert pointer from LRU cache hit")
	}
}

func TestCertCache_RedisMiss_Signs(t *testing.T) {
	cache := setupTestCache(t)

	// With nil Redis, should fall through to signing
	cert, err := cache.GetCertByHostname("no-redis.example.com")
	if err != nil {
		t.Fatalf("GetCertByHostname: %v", err)
	}
	if cert == nil {
		t.Fatal("expected non-nil cert")
	}
	if len(cert.Certificate) != 2 {
		t.Errorf("cert chain length = %d, want 2", len(cert.Certificate))
	}

	// Verify the cert is valid
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "no-redis.example.com" {
		t.Errorf("DNSNames = %v, want [no-redis.example.com]", leaf.DNSNames)
	}
}

func TestCertCache_GetCertForTLS(t *testing.T) {
	cache := setupTestCache(t)

	hello := &tls.ClientHelloInfo{
		ServerName: "tls-hello.example.com",
	}

	cert, err := cache.GetCert(hello)
	if err != nil {
		t.Fatalf("GetCert: %v", err)
	}
	if cert == nil {
		t.Fatal("expected non-nil cert")
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "tls-hello.example.com" {
		t.Errorf("DNSNames = %v, want [tls-hello.example.com]", leaf.DNSNames)
	}
}

func TestCertCache_GetCert_EmptySNI(t *testing.T) {
	cache := setupTestCache(t)

	hello := &tls.ClientHelloInfo{
		ServerName: "",
	}

	if _, err := cache.GetCert(hello); err == nil {
		t.Error("expected error for empty SNI")
	}
}

// TestNewCertCache_RedisPingSetsGauge asserts that the constructor probes
// Redis on startup and stamps the redis.available gauge before any TLS
// traffic arrives — a healthy ping must produce gauge=1.
func TestNewCertCache_RedisPingSetsGauge(t *testing.T) {
	installFreshMetricsRegistry()
	defer resetMetricsForCertTests()

	_, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	lru := NewLRUCache(10)
	cache := NewCertCache(issuer, lru, rdb, time.Hour, discardLogger())
	if cache == nil {
		t.Fatal("NewCertCache returned nil")
	}
	if got := readGauge(t, metrics.RedisAvailable); got != 1 {
		t.Errorf("redis.available after healthy ping = %v, want 1", got)
	}
}

// TestNewCertCache_RedisPingFailureSetsGauge: when the Redis endpoint is
// dead, the startup probe must mark redis.available=0 so observability is
// honest about the cache layer.
func TestNewCertCache_RedisPingFailureSetsGauge(t *testing.T) {
	installFreshMetricsRegistry()
	defer resetMetricsForCertTests()

	s, rdb := newTestRedis(t)
	s.Close() // kill before constructor runs the ping

	issuer := newIssuerForCacheTests(t)
	lru := NewLRUCache(10)
	cache := NewCertCache(issuer, lru, rdb, time.Hour, discardLogger())
	if cache == nil {
		t.Fatal("NewCertCache returned nil")
	}
	if got := readGauge(t, metrics.RedisAvailable); got != 0 {
		t.Errorf("redis.available after failed ping = %v, want 0", got)
	}
}

// TestCertCache_RedisRoundTrip exercises the full putToRedis -> getFromRedis
// cycle: sign a cert, persist it under one cache, then load it back via a
// SECOND cache that shares the same issuer (so the AES key matches) and
// the same miniredis instance. Asserts: leaf cert chain bytes round-trip,
// private key matches (same signature material), Leaf is parsed eagerly.
func TestCertCache_RedisRoundTrip(t *testing.T) {
	_, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	logger := discardLogger()

	// Cache 1: sign + store
	cache1 := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, logger)
	cert1, err := cache1.GetCertByHostname("roundtrip.example.com")
	if err != nil {
		t.Fatalf("first GetCertByHostname: %v", err)
	}

	// Cache 2: fresh LRU so we MUST hit Redis
	cache2 := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, logger)
	cert2, err := cache2.GetCertByHostname("roundtrip.example.com")
	if err != nil {
		t.Fatalf("second GetCertByHostname (redis): %v", err)
	}

	// Chain must round-trip byte-for-byte (leaf DER + CA DER).
	if len(cert2.Certificate) != len(cert1.Certificate) {
		t.Fatalf("redis chain len = %d, want %d", len(cert2.Certificate), len(cert1.Certificate))
	}
	for i := range cert1.Certificate {
		if !bytesEqual(cert1.Certificate[i], cert2.Certificate[i]) {
			t.Errorf("cert chain[%d] mismatch after redis round-trip", i)
		}
	}

	// Leaf must be parsed eagerly (no lazy re-parse per handshake).
	if cert2.Leaf == nil {
		t.Fatal("Leaf is nil after redis load; should be eagerly parsed")
	}
	if cert2.Leaf.DNSNames[0] != "roundtrip.example.com" {
		t.Errorf("Leaf DNSNames = %v, want [roundtrip.example.com]", cert2.Leaf.DNSNames)
	}

	// Private key must successfully sign with the same public key as cert1
	// (i.e. decrypt produced the right key, not a random ECDSA).
	pub1 := cert1.PrivateKey.(*ecdsa.PrivateKey).PublicKey
	pub2 := cert2.PrivateKey.(*ecdsa.PrivateKey).PublicKey
	if !pub1.Equal(&pub2) {
		t.Error("private key after redis load has different public key")
	}

	// Verify cert chain against the original CA — proves we didn't load a
	// forged cert from Redis.
	roots := x509.NewCertPool()
	roots.AddCert(issuer.CACert())
	if _, err := cert2.Leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("redis-loaded cert fails verification against CA: %v", err)
	}
}

// TestCertCache_RedisMiss_FallsThrough: miniredis is alive, key absent,
// so the cache must fall through to signing — and NOT mark redis as
// unavailable. After the miss + sign, the entry must land in Redis.
func TestCertCache_RedisMiss_FallsThrough(t *testing.T) {
	installFreshMetricsRegistry()
	defer resetMetricsForCertTests()

	s, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	cache := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, discardLogger())

	cert, err := cache.GetCertByHostname("miss.example.com")
	if err != nil {
		t.Fatalf("GetCertByHostname: %v", err)
	}
	if cert == nil {
		t.Fatal("expected non-nil cert from sign-on-miss")
	}

	// Redis miss MUST keep gauge=1 (server reachable, key just absent).
	if got := readGauge(t, metrics.RedisAvailable); got != 1 {
		t.Errorf("redis.available after miss = %v, want 1 (server still reachable)", got)
	}

	// Redis must now hold the entry under the documented key prefix.
	keys := s.Keys()
	want := redisKeyPrefix + "miss.example.com"
	found := false
	for _, k := range keys {
		if k == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected redis key %q, got %v", want, keys)
	}
}

// TestCertCache_RedisGetError_SetsGaugeAndFallsThrough: when Redis is dead,
// the cache must (a) NOT propagate the error to the caller — it falls
// through to signing, and (b) flip redis.available to 0 so observability
// reflects reality.
func TestCertCache_RedisGetError_SetsGaugeAndFallsThrough(t *testing.T) {
	installFreshMetricsRegistry()
	defer resetMetricsForCertTests()

	s, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	cache := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, discardLogger())

	s.Close() // kill the server AFTER NewCertCache ping ran

	cert, err := cache.GetCertByHostname("get-fails.example.com")
	if err != nil {
		t.Fatalf("GetCertByHostname must NOT fail on redis error, got: %v", err)
	}
	if cert == nil {
		t.Fatal("expected sign fallback on redis error")
	}
	if got := readGauge(t, metrics.RedisAvailable); got != 0 {
		t.Errorf("redis.available after GET error = %v, want 0", got)
	}
}

// TestCertCache_RedisGet_CorruptJSON: when the redis blob is not valid
// JSON, getFromRedis returns an error and the outer GetCertByHostname
// falls through to signing. Asserts both observable outcomes.
func TestCertCache_RedisGet_CorruptJSON(t *testing.T) {
	s, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	cache := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, discardLogger())

	// Poison the cache slot with non-JSON bytes.
	if err := s.Set(redisKeyPrefix+"poison.example.com", "not-json-at-all"); err != nil {
		t.Fatalf("seed miniredis: %v", err)
	}

	cert, err := cache.GetCertByHostname("poison.example.com")
	if err != nil {
		t.Fatalf("must fall through to sign on corrupt blob, got: %v", err)
	}
	if cert == nil || len(cert.Certificate) != 2 {
		t.Fatal("expected freshly signed cert after corrupt-redis fall-through")
	}
}

// TestCertCache_RedisGet_BadBase64: encryptedKey/nonce are base64; bad
// base64 must yield an error from getFromRedis and a sign fall-through.
func TestCertCache_RedisGet_BadBase64(t *testing.T) {
	s, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	cache := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, discardLogger())

	bad, _ := json.Marshal(redisCertEntry{
		EncryptedKey: "!!! not base64 !!!",
		CertChainPEM: "",
		Nonce:        "AAAA",
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err := s.Set(redisKeyPrefix+"badb64.example.com", string(bad)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cert, err := cache.GetCertByHostname("badb64.example.com")
	if err != nil || cert == nil {
		t.Fatalf("must sign-fallback on bad base64, got cert=%v err=%v", cert, err)
	}
}

// TestCertCache_RedisGet_BadNonce verifies the SECOND base64 decode branch
// (nonce) — separates "first decode failed" from "second decode failed".
func TestCertCache_RedisGet_BadNonce(t *testing.T) {
	s, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	cache := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, discardLogger())

	bad, _ := json.Marshal(redisCertEntry{
		EncryptedKey: base64.StdEncoding.EncodeToString([]byte("xxx")),
		CertChainPEM: "",
		Nonce:        "!!!not-base64!!!",
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err := s.Set(redisKeyPrefix+"badnonce.example.com", string(bad)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cert, err := cache.GetCertByHostname("badnonce.example.com")
	if err != nil || cert == nil {
		t.Fatalf("must sign-fallback on bad nonce b64, got cert=%v err=%v", cert, err)
	}
}

// TestCertCache_RedisGet_DecryptFails: when the encrypted key + nonce are
// valid base64 but won't decrypt under the current AES key, the decrypt
// branch must fail (and the cache must fall back to signing).
func TestCertCache_RedisGet_DecryptFails(t *testing.T) {
	s, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	cache := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, discardLogger())

	// Random 32-byte ciphertext + 12-byte nonce — GCM Open will fail auth.
	ct := make([]byte, 32)
	_, _ = io.ReadFull(rand.Reader, ct)
	nonce := make([]byte, 12)
	_, _ = io.ReadFull(rand.Reader, nonce)
	bad, _ := json.Marshal(redisCertEntry{
		EncryptedKey: base64.StdEncoding.EncodeToString(ct),
		CertChainPEM: "",
		Nonce:        base64.StdEncoding.EncodeToString(nonce),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err := s.Set(redisKeyPrefix+"baddec.example.com", string(bad)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cert, err := cache.GetCertByHostname("baddec.example.com")
	if err != nil || cert == nil {
		t.Fatalf("must sign-fallback on decrypt failure, got cert=%v err=%v", cert, err)
	}
}

// TestCertCache_RedisGet_EmptyPEM: a JSON blob that decrypts cleanly but
// carries no CERTIFICATE PEM blocks must produce an error inside
// getFromRedis (no certs found path).
func TestCertCache_RedisGet_EmptyPEM(t *testing.T) {
	s, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	cache := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, discardLogger())

	// Encrypt an arbitrary ECDSA key with the real AES key so the decrypt
	// step SUCCEEDS — the failure must come from the empty PEM, not from
	// the decrypt path covered above.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ct, nonce, err := issuer.EncryptPrivateKey(key)
	if err != nil {
		t.Fatalf("EncryptPrivateKey: %v", err)
	}
	entry, _ := json.Marshal(redisCertEntry{
		EncryptedKey: base64.StdEncoding.EncodeToString(ct),
		Nonce:        base64.StdEncoding.EncodeToString(nonce),
		CertChainPEM: "garbage-not-pem", // pem.Decode returns nil immediately
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err := s.Set(redisKeyPrefix+"emptypem.example.com", string(entry)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cert, err := cache.GetCertByHostname("emptypem.example.com")
	if err != nil || cert == nil {
		t.Fatalf("must sign-fallback on empty PEM, got cert=%v err=%v", cert, err)
	}
}

// TestCertCache_RedisGet_LeafParseFails: PEM is well-formed but the bytes
// inside aren't a parseable certificate — must fail in x509.ParseCertificate.
func TestCertCache_RedisGet_LeafParseFails(t *testing.T) {
	s, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	cache := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, discardLogger())

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ct, nonce, err := issuer.EncryptPrivateKey(key)
	if err != nil {
		t.Fatalf("EncryptPrivateKey: %v", err)
	}
	bogusPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-asn1")})
	entry, _ := json.Marshal(redisCertEntry{
		EncryptedKey: base64.StdEncoding.EncodeToString(ct),
		Nonce:        base64.StdEncoding.EncodeToString(nonce),
		CertChainPEM: string(bogusPEM),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err := s.Set(redisKeyPrefix+"badleaf.example.com", string(entry)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cert, err := cache.GetCertByHostname("badleaf.example.com")
	if err != nil || cert == nil {
		t.Fatalf("must sign-fallback on unparseable leaf, got cert=%v err=%v", cert, err)
	}
}

// TestCertCache_PutToRedisFailsAfterServerDeath: simulate a server that
// passes the startup ping but dies before the first sign — the put-error
// branch must flip the gauge to 0 AND still return the freshly-signed
// cert (best-effort cache write must not block the response).
func TestCertCache_PutToRedisFailsAfterServerDeath(t *testing.T) {
	installFreshMetricsRegistry()
	defer resetMetricsForCertTests()

	s, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	cache := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, discardLogger())

	// Pre-condition: gauge healthy after constructor ping.
	if got := readGauge(t, metrics.RedisAvailable); got != 1 {
		t.Fatalf("preconditon: redis.available = %v, want 1", got)
	}

	s.Close() // server dies; GET fails (returns nil cert + err, gauge -> 0)
	// then SET also fails (gauge stays 0).

	cert, err := cache.GetCertByHostname("putfail.example.com")
	if err != nil {
		t.Fatalf("GetCertByHostname must succeed despite redis death: %v", err)
	}
	if cert == nil {
		t.Fatal("expected cert from sign fallback")
	}
	if got := readGauge(t, metrics.RedisAvailable); got != 0 {
		t.Errorf("redis.available after GET+SET failures = %v, want 0", got)
	}
}

// TestCertCache_LRUHit_IncrementsHitCounter pins that an LRU hit fires
// the cert_cache.hits_total{layer="lru"} counter when metrics are
// registered. Production startup wires the counter; the increment is the
// only observable signal a cache hit happened (besides the response time).
func TestCertCache_LRUHit_IncrementsHitCounter(t *testing.T) {
	installFreshMetricsRegistry()
	defer resetMetricsForCertTests()

	cache := setupTestCache(t)
	// First call signs and populates LRU.
	if _, err := cache.GetCertByHostname("hit-metric.example.com"); err != nil {
		t.Fatalf("first GetCertByHostname: %v", err)
	}
	before := readCounter(t, "cert_cache_hits_total", "lru")
	if _, err := cache.GetCertByHostname("hit-metric.example.com"); err != nil {
		t.Fatalf("second GetCertByHostname: %v", err)
	}
	after := readCounter(t, "cert_cache_hits_total", "lru")
	if after-before < 1 {
		t.Errorf("lru hit counter delta = %v, want >= 1", after-before)
	}
}

// TestCertCache_RedisHit_IncrementsHitCounter pins the layer="redis"
// hits_total bump when the LRU is cold but Redis has the entry. Uses
// two CertCaches sharing the same issuer + redis so cache#2 is forced
// to look up via Redis.
func TestCertCache_RedisHit_IncrementsHitCounter(t *testing.T) {
	installFreshMetricsRegistry()
	defer resetMetricsForCertTests()

	_, rdb := newTestRedis(t)
	iss := newIssuerForCacheTests(t)
	cache1 := NewCertCache(iss, NewLRUCache(10), rdb, time.Hour, discardLogger())
	if _, err := cache1.GetCertByHostname("redis-hit.example.com"); err != nil {
		t.Fatalf("seed cache1: %v", err)
	}

	cache2 := NewCertCache(iss, NewLRUCache(10), rdb, time.Hour, discardLogger())
	before := readCounter(t, "cert_cache_hits_total", "redis")
	if _, err := cache2.GetCertByHostname("redis-hit.example.com"); err != nil {
		t.Fatalf("cache2 lookup: %v", err)
	}
	after := readCounter(t, "cert_cache_hits_total", "redis")
	if after-before < 1 {
		t.Errorf("redis hit counter delta = %v, want >= 1", after-before)
	}
}

// TestCertCache_RedisLoad_StampsAvailableGauge pins the
// `metrics.RedisAvailable != nil` arm inside getFromRedis that stamps the
// gauge to 1 on a successful load — separates "ping-time healthy" from
// "load-time healthy".
func TestCertCache_RedisLoad_StampsAvailableGauge(t *testing.T) {
	installFreshMetricsRegistry()
	defer resetMetricsForCertTests()

	_, rdb := newTestRedis(t)
	iss := newIssuerForCacheTests(t)
	cache1 := NewCertCache(iss, NewLRUCache(10), rdb, time.Hour, discardLogger())
	if _, err := cache1.GetCertByHostname("avail-load.example.com"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cache2 := NewCertCache(iss, NewLRUCache(10), rdb, time.Hour, discardLogger())
	if _, err := cache2.GetCertByHostname("avail-load.example.com"); err != nil {
		t.Fatalf("redis load: %v", err)
	}
	if got := readGauge(t, metrics.RedisAvailable); got != 1 {
		t.Errorf("redis.available after redis-load = %v, want 1", got)
	}
}

// TestPutToRedis_RejectsNonECDSAKey: putToRedis only knows how to
// serialize ECDSA private keys (the only kind SignCert produces). If a
// caller hands it an RSA cert, the helper must return a typed error
// without attempting AES encryption.
func TestPutToRedis_RejectsNonECDSAKey(t *testing.T) {
	_, rdb := newTestRedis(t)
	issuer := newIssuerForCacheTests(t)
	cache := NewCertCache(issuer, NewLRUCache(10), rdb, time.Hour, discardLogger())

	// Hand-crafted cert with a non-ECDSA private key (use a string sentinel
	// to make the type-assertion fail deterministically).
	bogus := &tls.Certificate{
		Certificate: [][]byte{[]byte("does-not-matter")},
		PrivateKey:  "not-a-key",
	}
	err := cache.putToRedis("bogus.example.com", bogus)
	if err == nil {
		t.Fatal("expected error for non-ECDSA private key")
	}
}

// certTestPromReg is the per-test prometheus registry that the gauge readers
// gather from. resetMetricsForCertTests / readGauge install + read it.
var certTestPromReg *prom.Registry

// installFreshMetricsRegistry wires up a brand-new opsmetrics registry over
// a fresh prom.Registry so each test gets isolated metric state. Returns
// the prom registry for Gather() calls.
func installFreshMetricsRegistry() *prom.Registry {
	pr := prom.NewRegistry()
	metrics.Register(registry.NewRegistry(pr))
	certTestPromReg = pr
	return pr
}

// resetMetricsForCertTests detaches the global metric pointers so the next
// test that doesn't register starts from nil (matches production startup).
func resetMetricsForCertTests() {
	metrics.RedisAvailable = nil
	metrics.CertCacheHits = nil
	metrics.CertCacheMisses = nil
	metrics.CertSignMs = nil
	metrics.CertPrewarmMs = nil
	certTestPromReg = nil
}

// readGauge gathers the per-test prom registry and returns the current
// value of the named gauge (no labels). Fails the test if the metric is
// absent — that's a wiring bug.
func readGauge(t *testing.T, _ *registry.Gauge) float64 {
	t.Helper()
	if certTestPromReg == nil {
		t.Fatal("certTestPromReg is nil; call installFreshMetricsRegistry first")
	}
	// We only assert on redis.available in this file; opsmetrics maps the
	// dotted name to nexus_*_redis_available — gather and match by suffix
	// to stay decoupled from the namespace prefix.
	mfs, err := certTestPromReg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "" {
			continue
		}
		if !endsWith(mf.GetName(), "redis_available") {
			continue
		}
		for _, m := range mf.GetMetric() {
			if m.Gauge != nil && m.Gauge.Value != nil {
				return *m.Gauge.Value
			}
		}
	}
	t.Fatal("redis_available gauge not found in registry")
	return -1
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// readCounter sums the metric values for a counter with the given suffix
// matching at least one label value (e.g. layer=lru). The opsmetrics
// counter is registered under nexus_<svc>_cert_cache_hits_total — gather
// + suffix-match keeps tests decoupled from the namespace prefix.
func readCounter(t *testing.T, suffix, labelVal string) float64 {
	t.Helper()
	if certTestPromReg == nil {
		t.Fatal("certTestPromReg is nil; call installFreshMetricsRegistry first")
	}
	mfs, err := certTestPromReg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if !endsWith(mf.GetName(), suffix) {
			continue
		}
		for _, m := range mf.GetMetric() {
			matched := labelVal == ""
			for _, lp := range m.GetLabel() {
				if lp.GetValue() == labelVal {
					matched = true
				}
			}
			if !matched {
				continue
			}
			if m.Counter != nil && m.Counter.Value != nil {
				total += *m.Counter.Value
			}
		}
	}
	return total
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
