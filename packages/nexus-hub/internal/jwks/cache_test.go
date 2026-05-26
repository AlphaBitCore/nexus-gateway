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
