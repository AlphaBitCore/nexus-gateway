package jwtverifier_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
)

// jwk mirrors the wire shape of a JWKS entry for test servers.
type jwk struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

// rsaToJWK encodes an RSA public key into a JWK entry with the given kid.
func rsaToJWK(t *testing.T, kid string, pub *rsa.PublicKey) jwk {
	t.Helper()
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	eBig := big.NewInt(int64(pub.E))
	e := base64.RawURLEncoding.EncodeToString(eBig.Bytes())
	return jwk{
		Kty: "RSA",
		Alg: "RS256",
		Use: "sig",
		Kid: kid,
		N:   n,
		E:   e,
	}
}

// genRSA generates a small RSA keypair suitable for tests.
func genRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return k
}

func TestJWKSCache_KeyByKID_HappyPath(t *testing.T) {
	t.Parallel()

	priv := genRSA(t)
	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		doc := jwksDoc{Keys: []jwk{rsaToJWK(t, "k1", &priv.PublicKey)}}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	c := jwtverifier.NewJWKSCache(srv.URL)
	ctx := context.Background()

	k1, err := c.KeyByKID(ctx, "k1")
	if err != nil {
		t.Fatalf("first KeyByKID: %v", err)
	}
	if k1 == nil {
		t.Fatal("first KeyByKID returned nil key")
	}

	k2, err := c.KeyByKID(ctx, "k1")
	if err != nil {
		t.Fatalf("second KeyByKID: %v", err)
	}
	if k2 != k1 {
		t.Fatalf("cached key pointer mismatch: got %p want %p", k2, k1)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1 (second call should be cached)", got)
	}
}

func TestJWKSCache_StaleWithinTTLOnFailure(t *testing.T) {
	t.Parallel()

	priv := genRSA(t)
	var fail atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "upstream down", http.StatusInternalServerError)
			return
		}
		doc := jwksDoc{Keys: []jwk{rsaToJWK(t, "k1", &priv.PublicKey)}}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	// Short TTL so we can cross it in-test without time.Sleep hacks.
	ttl := 150 * time.Millisecond
	c := jwtverifier.NewJWKSCacheWithTTL(srv.URL, ttl)
	ctx := context.Background()

	// Prime the cache.
	k, err := c.KeyByKID(ctx, "k1")
	if err != nil {
		t.Fatalf("prime: %v", err)
	}
	if k == nil {
		t.Fatal("prime returned nil key")
	}

	// Flip upstream to failing. Cache is still fresh, so this call should
	// hit cache without triggering a refresh and succeed with the cached
	// pointer.
	fail.Store(true)
	k2, err := c.KeyByKID(ctx, "k1")
	if err != nil {
		t.Fatalf("stale-within-ttl: %v", err)
	}
	if k2 != k {
		t.Fatalf("stale key pointer mismatch")
	}

	// Wait past TTL. Now the cache entry is expired; refresh will be
	// attempted and will fail, and there is no in-TTL fallback either.
	// Expect ErrJWKSUnavailable.
	time.Sleep(ttl + 50*time.Millisecond)

	_, err = c.KeyByKID(ctx, "k1")
	if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("expired+failing: err = %v, want ErrJWKSUnavailable", err)
	}
}

func TestJWKSCache_UnknownKidAfterSuccessfulRefresh(t *testing.T) {
	t.Parallel()

	priv := genRSA(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := jwksDoc{Keys: []jwk{rsaToJWK(t, "k1", &priv.PublicKey)}}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	c := jwtverifier.NewJWKSCache(srv.URL)

	_, err := c.KeyByKID(context.Background(), "k2")
	if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("unknown kid: err = %v, want ErrJWKSUnavailable", err)
	}
}

func TestJWKSCache_ConcurrentCallsCoalesce(t *testing.T) {
	t.Parallel()

	priv := genRSA(t)
	var hits atomic.Int64
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Block the first (and only) fetch until the test releases it.
		<-release
		doc := jwksDoc{Keys: []jwk{rsaToJWK(t, "k1", &priv.PublicKey)}}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	c := jwtverifier.NewJWKSCache(srv.URL)

	const callers = 10
	var wg sync.WaitGroup
	wg.Add(callers)
	errs := make(chan error, callers)
	keys := make(chan *rsa.PublicKey, callers)

	for range callers {
		go func() {
			defer wg.Done()
			k, err := c.KeyByKID(context.Background(), "k1")
			if err != nil {
				errs <- err
				return
			}
			keys <- k
		}()
	}

	// Give goroutines time to all arrive at singleflight. 50ms is enough to
	// schedule 10 goroutines without making the test slow.
	time.Sleep(50 * time.Millisecond)
	close(release)

	wg.Wait()
	close(errs)
	close(keys)

	for err := range errs {
		t.Fatalf("caller: %v", err)
	}

	var first *rsa.PublicKey
	count := 0
	for k := range keys {
		count++
		if first == nil {
			first = k
			continue
		}
		if k != first {
			t.Fatal("coalesced callers observed different key pointers")
		}
	}
	if count != callers {
		t.Fatalf("got %d successful callers, want %d", count, callers)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1 (singleflight should coalesce)", got)
	}
}

func TestJWKSCache_InitialFailureReturnsUnavailable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := jwtverifier.NewJWKSCache(srv.URL)

	_, err := c.KeyByKID(context.Background(), "k1")
	if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("err = %v, want ErrJWKSUnavailable", err)
	}
}

func TestJWKSCache_NonRSAKeysFiltered(t *testing.T) {
	t.Parallel()

	priv := genRSA(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := jwksDoc{Keys: []jwk{
			rsaToJWK(t, "k1", &priv.PublicKey),
			{
				Kty: "EC",
				Alg: "ES256",
				Use: "sig",
				Kid: "k2",
				Crv: "P-256",
				X:   base64.RawURLEncoding.EncodeToString([]byte("x-coord-bytes-unused-in-test")),
				Y:   base64.RawURLEncoding.EncodeToString([]byte("y-coord-bytes-unused-in-test")),
			},
		}}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	c := jwtverifier.NewJWKSCache(srv.URL)
	ctx := context.Background()

	if _, err := c.KeyByKID(ctx, "k1"); err != nil {
		t.Fatalf("RSA key k1: %v", err)
	}

	_, err := c.KeyByKID(ctx, "k2")
	if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("EC key k2: err = %v, want ErrJWKSUnavailable", err)
	}
}
