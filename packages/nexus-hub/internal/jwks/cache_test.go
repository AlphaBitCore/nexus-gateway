package jwks_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jwks"
)

func generateTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

func jwksServerFor(t *testing.T, kid string, key *rsa.PrivateKey) *httptest.Server {
	t.Helper()
	nBytes := key.N.Bytes()
	eVal := big.NewInt(int64(key.E))
	eBytes := eVal.Bytes()

	payload := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": kid,
				"n":   base64.RawURLEncoding.EncodeToString(nBytes),
				"e":   base64.RawURLEncoding.EncodeToString(eBytes),
			},
		},
	}
	body, _ := json.Marshal(payload)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body) //nolint:errcheck
	}))
}

func TestCacheHit(t *testing.T) {
	key := generateTestKey(t)
	srv := jwksServerFor(t, "key-1", key)
	defer srv.Close()

	c := jwks.New(srv.URL, slog.Default())
	defer c.Close()

	// Give the initial fetch a moment to complete.
	time.Sleep(50 * time.Millisecond)

	got, err := c.Get("key-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.N.Cmp(key.N) != 0 {
		t.Error("returned key does not match generated key")
	}
}

func TestCacheMiss(t *testing.T) {
	key := generateTestKey(t)
	srv := jwksServerFor(t, "key-1", key)
	defer srv.Close()

	c := jwks.New(srv.URL, slog.Default())
	defer c.Close()

	time.Sleep(50 * time.Millisecond)

	_, err := c.Get("unknown-kid")
	if err == nil {
		t.Fatal("expected error for unknown kid")
	}
}

func TestCacheEmptyOnNeverFetched(t *testing.T) {
	// Point at a server that always fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := jwks.New(srv.URL, slog.Default())
	defer c.Close()

	time.Sleep(50 * time.Millisecond)

	_, err := c.Get("any-kid")
	if err == nil {
		t.Fatal("expected ErrCacheEmpty")
	}
}

func TestCacheEmptyKidReturnsSomeKey(t *testing.T) {
	key := generateTestKey(t)
	srv := jwksServerFor(t, "key-1", key)
	defer srv.Close()

	c := jwks.New(srv.URL, slog.Default())
	defer c.Close()

	time.Sleep(50 * time.Millisecond)

	got, err := c.Get("")
	if err != nil {
		t.Fatalf("Get with empty kid: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil key for empty kid")
	}
}

// rsaJWKEntry builds a single JWK map for a private key under a kid.
func rsaJWKEntry(kid string, key *rsa.PrivateKey) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func jwksServerWith(t *testing.T, entries ...map[string]any) *httptest.Server {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"keys": entries})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

// TestCache_EmptyKidMultipleKeysRejected covers F-0254: when the cache holds
// more than one key and the caller passes an empty kid, Get must return an
// error rather than a nondeterministic map-iteration key. The single-key
// convenience is covered by TestCacheEmptyKidReturnsSomeKey.
func TestCache_EmptyKidMultipleKeysRejected(t *testing.T) {
	k1, k2 := generateTestKey(t), generateTestKey(t)
	srv := jwksServerWith(t, rsaJWKEntry("a", k1), rsaJWKEntry("b", k2))
	defer srv.Close()

	c := jwks.New(srv.URL, slog.Default())
	defer c.Close()
	time.Sleep(50 * time.Millisecond)

	// Both kids individually resolve.
	if _, err := c.Get("a"); err != nil {
		t.Fatalf("Get(a): %v", err)
	}
	if _, err := c.Get("b"); err != nil {
		t.Fatalf("Get(b): %v", err)
	}
	// Empty kid against a multi-key set must be rejected deterministically.
	if _, err := c.Get(""); err == nil {
		t.Fatal("empty kid with multiple keys must error (F-0254), got nil")
	}
}

// TestCache_WeakModulusRejected covers F-0253: a sub-2048-bit RSA modulus must
// be rejected at parse time so it never enters the cache, even on a 200 with
// otherwise well-formed base64url fields.
func TestCache_WeakModulusRejected(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate 1024-bit key: %v", err)
	}
	good := generateTestKey(t)
	srv := jwksServerWith(t, rsaJWKEntry("weak", weak), rsaJWKEntry("good", good))
	defer srv.Close()

	c := jwks.New(srv.URL, slog.Default())
	defer c.Close()
	time.Sleep(50 * time.Millisecond)

	if _, err := c.Get("weak"); err == nil {
		t.Error("1024-bit modulus must be rejected (F-0253), but Get(weak) succeeded")
	}
	if _, err := c.Get("good"); err != nil {
		t.Errorf("2048-bit sibling should still be installed: %v", err)
	}
}

// TestCache_OversizedExponentRejected covers F-0253: an exponent larger than
// 8 bytes would be silently truncated by e.Int64()→int, producing a wrong key
// that mis-verifies signatures. Such a key must be rejected, not installed. The
// modulus is a real 2048-bit value so the rejection is attributable to the
// exponent guard alone.
func TestCache_OversizedExponentRejected(t *testing.T) {
	good := generateTestKey(t)
	// 9-byte exponent (> math.MaxInt64 fits-in-int64 boundary): does not fit
	// in an int64, so IsInt64() is false → rejected.
	bigE := make([]byte, 9)
	bigE[0] = 0x01
	oversized := map[string]any{
		"kty": "RSA",
		"kid": "bige",
		"n":   base64.RawURLEncoding.EncodeToString(good.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(bigE),
	}
	srv := jwksServerWith(t, oversized)
	defer srv.Close()

	c := jwks.New(srv.URL, slog.Default())
	defer c.Close()
	time.Sleep(50 * time.Millisecond)

	if _, err := c.Get("bige"); err == nil {
		t.Error("oversized exponent must be rejected (F-0253), but Get(bige) succeeded")
	}
}

// TestCache_NonRSAKeyTypeSkipped covers the
// `if k.KTY != "RSA" { continue }` filter — an EC or OKP key listed
// alongside RSA keys must be ignored, not cause a parse failure.
func TestCache_NonRSAKeyTypeSkipped(t *testing.T) {
	key := generateTestKey(t)
	nBytes := key.N.Bytes()
	eVal := big.NewInt(int64(key.E))
	eBytes := eVal.Bytes()

	payload := map[string]any{
		"keys": []map[string]any{
			// Non-RSA entry — must be skipped.
			{"kty": "EC", "kid": "ec-1", "crv": "P-256"},
			// Real RSA entry — must be installed.
			{
				"kty": "RSA",
				"kid": "rsa-1",
				"n":   base64.RawURLEncoding.EncodeToString(nBytes),
				"e":   base64.RawURLEncoding.EncodeToString(eBytes),
			},
		},
	}
	body, _ := json.Marshal(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := jwks.New(srv.URL, slog.Default())
	defer c.Close()
	time.Sleep(50 * time.Millisecond)

	// EC entry must NOT appear in cache.
	if _, err := c.Get("ec-1"); err == nil {
		t.Error("EC key must be skipped, but Get(ec-1) succeeded")
	}
	// RSA entry must work.
	if _, err := c.Get("rsa-1"); err != nil {
		t.Errorf("RSA key should be cached: %v", err)
	}
}

// TestCache_MalformedRSAKeySkipped covers the
// `parseRSAKey err → skip + log` branch. A JWK with non-base64url n
// must be ignored without poisoning sibling valid keys.
func TestCache_MalformedRSAKeySkipped(t *testing.T) {
	key := generateTestKey(t)
	nBytes := key.N.Bytes()
	eVal := big.NewInt(int64(key.E))
	eBytes := eVal.Bytes()

	payload := map[string]any{
		"keys": []map[string]any{
			// Malformed RSA: n is not base64-url-decodable.
			{"kty": "RSA", "kid": "bad", "n": "not-base64!!!", "e": "AQAB"},
			// Valid RSA sibling.
			{
				"kty": "RSA",
				"kid": "good",
				"n":   base64.RawURLEncoding.EncodeToString(nBytes),
				"e":   base64.RawURLEncoding.EncodeToString(eBytes),
			},
		},
	}
	body, _ := json.Marshal(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := jwks.New(srv.URL, slog.Default())
	defer c.Close()
	time.Sleep(50 * time.Millisecond)

	if _, err := c.Get("bad"); err == nil {
		t.Error("malformed key must be skipped, but Get(bad) succeeded")
	}
	if _, err := c.Get("good"); err != nil {
		t.Errorf("good sibling should still be installed: %v", err)
	}
}

// TestCache_MalformedRSAEFieldSkipped covers the e-decode error path
// in parseRSAKey (paired with the n-decode branch tested above).
func TestCache_MalformedRSAEFieldSkipped(t *testing.T) {
	key := generateTestKey(t)
	nBytes := key.N.Bytes()
	payload := map[string]any{
		"keys": []map[string]any{
			{"kty": "RSA", "kid": "bad-e", "n": base64.RawURLEncoding.EncodeToString(nBytes), "e": "!!!"},
		},
	}
	body, _ := json.Marshal(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := jwks.New(srv.URL, slog.Default())
	defer c.Close()
	time.Sleep(50 * time.Millisecond)

	if _, err := c.Get("bad-e"); err == nil {
		t.Error("malformed e must be skipped")
	}
}

// TestCache_ParseErrorOnInitialFetch covers the JSON-decode error
// branch in refresh(): server returns garbage, cache stays empty.
func TestCache_ParseErrorOnInitialFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := jwks.New(srv.URL, slog.Default())
	defer c.Close()
	time.Sleep(50 * time.Millisecond)

	if _, err := c.Get("any"); err == nil {
		t.Error("expected cache-empty error when initial fetch returns garbage")
	}
}
