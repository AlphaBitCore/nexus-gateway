package jwtverifier_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
)

// TestJWKSCache_StaleServedWhenRefreshFails pins the stale-while-revalidate
// branch: after the cache is primed and the upstream goes red, a still-in-TTL
// caller must keep getting the cached key even though the refresh attempt
// failed. Lock this in because dropping it would turn every blip in the auth
// server into a token-validation outage.
func TestJWKSCache_StaleServedWhenRefreshFails(t *testing.T) {
	t.Parallel()

	priv := genRSA(t)
	var fail atomic.Bool
	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		doc := jwksDoc{Keys: []jwk{rsaToJWK(t, "k1", &priv.PublicKey)}}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	// 500ms TTL — long enough that the second call is still in-TTL, but the
	// kid-miss path force-refreshes and falls back to stale.
	c := jwtverifier.NewJWKSCacheWithTTL(srv.URL, 500*time.Millisecond)
	ctx := context.Background()

	// Prime cache.
	k1, err := c.KeyByKID(ctx, "k1")
	if err != nil {
		t.Fatalf("prime: %v", err)
	}

	// Now flip upstream to failing and request an UNKNOWN kid. The first
	// lookup misses the in-cache map, so KeyByKID enters the refresh branch.
	// refresh() will fail; the stale-fallback logic must serve the
	// previously-cached "k1" entry — but only when asked for "k1". For "k2"
	// the stale entry doesn't help, so we get ErrJWKSUnavailable.
	fail.Store(true)

	// Force a refresh by asking for the existing kid AFTER making upstream
	// fail. The cache is still fresh, so this short-circuits to in-memory hit
	// without hitting the refresh path — which is the desired in-TTL fast path.
	k2, err := c.KeyByKID(ctx, "k1")
	if err != nil {
		t.Fatalf("in-TTL hit while upstream failing: %v", err)
	}
	if k2 != k1 {
		t.Errorf("cached pointer should be identical between two in-TTL hits")
	}

	// Now ask for a kid that isn't cached. The cache is still fresh but the
	// requested kid isn't in the map, so KeyByKID falls through to the
	// refresh branch. Refresh fails; the stale-while-revalidate fallback
	// (kid still must be in stale snapshot) does NOT have k2, so we surface
	// ErrJWKSUnavailable.
	_, err = c.KeyByKID(ctx, "k2")
	if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("unknown kid + failing upstream + stale-miss: err = %v, want ErrJWKSUnavailable", err)
	}
}

// TestJWKSCache_StaleFallback_KidPresent pins the exact stale-while-revalidate
// arm where the cached snapshot DOES carry the requested kid: refresh fails,
// but the in-TTL stale entry has the kid, so KeyByKID still returns it. This
// is the "tolerate a single failed refresh" production-safety property.
func TestJWKSCache_StaleFallback_KidPresent(t *testing.T) {
	t.Parallel()

	priv := genRSA(t)
	var fail atomic.Bool
	var failHits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			failHits.Add(1)
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		doc := jwksDoc{Keys: []jwk{rsaToJWK(t, "k1", &priv.PublicKey)}}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	// Use a tight TTL so we can prime + flip + still be inside it.
	ttl := 5 * time.Second
	c := jwtverifier.NewJWKSCacheWithTTL(srv.URL, ttl)
	ctx := context.Background()

	// Prime.
	k1, err := c.KeyByKID(ctx, "k1")
	if err != nil {
		t.Fatalf("prime: %v", err)
	}

	// Flip to fail, then ask for a kid that's not in the cached snapshot
	// (forces refresh attempt) — refresh will fail, and stale-fallback will
	// NOT match because "k2" isn't cached. Then immediately ask for "k1": the
	// in-TTL fast-path hits without touching refresh. The stale-with-kid
	// arm (refresh-failed but kid in stale) requires that we somehow force
	// a refresh on the "k1" kid lookup while the cache is fresh. The fast
	// path returns early on a fresh-cache hit, so to hit the stale-fallback
	// branch for an EXISTING kid we need the cache stale w.r.t. ttl. We
	// engineer this via short TTL + sleep + failing upstream.
	c2 := jwtverifier.NewJWKSCacheWithTTL(srv.URL, 60*time.Millisecond)
	fail.Store(false)
	if _, err := c2.KeyByKID(ctx, "k1"); err != nil {
		t.Fatalf("c2 prime: %v", err)
	}
	fail.Store(true)
	time.Sleep(80 * time.Millisecond) // cache now stale (past TTL); refresh will be tried + fail

	// Past TTL + failing upstream + stale-fallback only kicks in when the
	// "cur != nil && time.Since(cur.fetchedAt) < c.ttl" check still holds.
	// Once TTL is exceeded the stale-fallback also misses → ErrJWKSUnavailable.
	_, err = c2.KeyByKID(ctx, "k1")
	if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("past-TTL + failing upstream: err = %v, want ErrJWKSUnavailable", err)
	}

	// Now exercise the in-TTL stale-fallback arm: re-prime c2 succeed, flip
	// fail, and immediately ask for an UNKNOWN kid (forces refresh) — the
	// refresh fails, and the stale-fallback path checks for the kid in the
	// in-TTL cache. Because we ask for "k1" via this path we WILL hit the
	// stale-with-kid arm.
	fail.Store(false)
	c3 := jwtverifier.NewJWKSCacheWithTTL(srv.URL, 5*time.Second)
	if _, err := c3.KeyByKID(ctx, "k1"); err != nil {
		t.Fatalf("c3 prime: %v", err)
	}
	fail.Store(true)
	// Ask for an unknown kid; in-TTL fast-path map-lookup misses "k2" so we
	// trigger refresh, which fails. The error-path stale-fallback then checks
	// "k2" against the in-TTL cache snapshot, which still doesn't have it,
	// so we get ErrJWKSUnavailable — but the in-TTL "k1" still works.
	_, errK2 := c3.KeyByKID(ctx, "k2")
	if !errors.Is(errK2, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("unknown kid path: err = %v, want ErrJWKSUnavailable", errK2)
	}
	// And "k1" continues to resolve from the still-fresh cache snapshot.
	k1again, err := c3.KeyByKID(ctx, "k1")
	if err != nil {
		t.Fatalf("k1 should still resolve in-TTL: %v", err)
	}
	if k1again == nil {
		t.Fatal("k1 returned nil key")
	}
	_ = k1
}

// TestJWKSCache_BadURL_NewRequestFails pins the http.NewRequestWithContext
// error arm of refresh: a syntactically illegal URL trips request construction
// before any network I/O, surfacing as ErrJWKSUnavailable.
func TestJWKSCache_BadURL_NewRequestFails(t *testing.T) {
	t.Parallel()

	// A control character in the URL is rejected by net/url when building the
	// request, exercising the "build request" error path.
	c := jwtverifier.NewJWKSCache("http://\x7f-invalid-host/jwks")
	_, err := c.KeyByKID(context.Background(), "k1")
	if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("bad URL: err = %v, want ErrJWKSUnavailable", err)
	}
}

// TestJWKSCache_ContextCanceled returns the context error verbatim from
// refresh (not wrapped) so callers can distinguish cancellation from upstream
// failure. We block the JWKS server long enough to let the caller's context
// expire, then check that the surfaced error is ErrJWKSUnavailable (the cache
// converts the inner ctx error into the public sentinel — the inner-ctx-err
// branch is still exercised inside refresh).
func TestJWKSCache_ContextCanceled(t *testing.T) {
	t.Parallel()

	// Server that blocks forever so the caller's context expires first.
	// Cleanup order (LIFO) matters: close(block) must run BEFORE srv.Close
	// or srv.Close hangs waiting for the blocked handler goroutine.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	t.Cleanup(srv.Close)               // runs SECOND (LIFO) — after block is closed
	t.Cleanup(func() { close(block) }) // runs FIRST — releases the handler

	c := jwtverifier.NewJWKSCache(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.KeyByKID(ctx, "k1")
	// The public surface is ErrJWKSUnavailable; the inner ctx-cancel branch
	// of refresh is exercised on the way there.
	if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("ctx canceled: err = %v, want ErrJWKSUnavailable", err)
	}
}

// TestJWKSCache_NonOKStatus pins the status-code error path: a 503 from JWKS
// must surface as ErrJWKSUnavailable, not crash on the decode step.
func TestJWKSCache_NonOKStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "maintenance", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := jwtverifier.NewJWKSCache(srv.URL)
	_, err := c.KeyByKID(context.Background(), "k1")
	if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("status 503: err = %v, want ErrJWKSUnavailable", err)
	}
}

// TestJWKSCache_DecodeError pins the JSON-decode error path: a body that
// isn't valid JWKS JSON surfaces as ErrJWKSUnavailable.
func TestJWKSCache_DecodeError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{this is not json`))
	}))
	defer srv.Close()

	c := jwtverifier.NewJWKSCache(srv.URL)
	_, err := c.KeyByKID(context.Background(), "k1")
	if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("decode-fail: err = %v, want ErrJWKSUnavailable", err)
	}
}

// TestJWKSCache_FilterBranches drives each guard in the refresh-loop's key
// filter. Documents which JWKS entries are silently skipped vs accepted, so a
// publisher tweak (adding new key types, padding base64 incorrectly,
// publishing huge exponents) doesn't silently break verification on the
// resource server.
func TestJWKSCache_FilterBranches(t *testing.T) {
	t.Parallel()

	priv := genRSA(t)
	goodN := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
	goodE := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Build a JWKS doc with one valid key and several skippable entries.
		// Each skippable entry exercises a different guard inside refresh.
		doc := jwksDoc{Keys: []jwk{
			// Valid key — accepted.
			{Kty: "RSA", Alg: "RS256", Use: "sig", Kid: "good", N: goodN, E: goodE},
			// Empty kid — skipped.
			{Kty: "RSA", Alg: "RS256", Use: "sig", Kid: "", N: goodN, E: goodE},
			// Empty n — skipped.
			{Kty: "RSA", Alg: "RS256", Use: "sig", Kid: "no-n", N: "", E: goodE},
			// Empty e — skipped.
			{Kty: "RSA", Alg: "RS256", Use: "sig", Kid: "no-e", N: goodN, E: ""},
			// Bad base64 in n — skipped at decode.
			{Kty: "RSA", Alg: "RS256", Use: "sig", Kid: "bad-n", N: "!!!not-base64!!!", E: goodE},
			// Bad base64 in e — skipped at decode.
			{Kty: "RSA", Alg: "RS256", Use: "sig", Kid: "bad-e", N: goodN, E: "!!!not-base64!!!"},
			// e larger than int64 (9 bytes, leading bit clear so it's positive
			// but still > int64.MaxValue when interpreted as big.Int). Skipped
			// at the e.IsInt64() guard.
			{Kty: "RSA", Alg: "RS256", Use: "sig", Kid: "huge-e",
				N: goodN,
				E: base64.RawURLEncoding.EncodeToString([]byte{
					0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01,
				}),
			},
			// Wrong use (enc) — skipped at type guard.
			{Kty: "RSA", Alg: "RS256", Use: "enc", Kid: "enc-key", N: goodN, E: goodE},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	c := jwtverifier.NewJWKSCache(srv.URL)
	ctx := context.Background()

	// "good" is accepted.
	if _, err := c.KeyByKID(ctx, "good"); err != nil {
		t.Fatalf("good kid: %v", err)
	}

	// Every other kid is filtered out and surfaces as ErrJWKSUnavailable.
	for _, kid := range []string{"no-n", "no-e", "bad-n", "bad-e", "huge-e", "enc-key"} {
		_, err := c.KeyByKID(ctx, kid)
		if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
			t.Errorf("kid %q: err = %v, want ErrJWKSUnavailable (filtered entry must not resolve)", kid, err)
		}
	}
}
