package jwks

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// generateTestRSA + encodeRSAJWKS are inlined here so this file stays
// in `package jwks` (needed to reach the unexported newWithGrace
// seam). The _test sibling has parallel helpers in package jwks_test.
func generateTestRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA: %v", err)
	}
	return key
}

func encodeRSAJWKS(kid string, key *rsa.PrivateKey) []byte {
	nBytes := key.N.Bytes()
	eVal := big.NewInt(int64(key.E))
	body, _ := json.Marshal(map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": kid,
				"n":   base64.RawURLEncoding.EncodeToString(nBytes),
				"e":   base64.RawURLEncoding.EncodeToString(eVal.Bytes()),
			},
		},
	})
	return body
}

// TestHandleFetchFailure_ClearsCacheAfterGrace exercises the
// stale-grace branch in handleFetchFailure: a successful initial
// fetch installs the cache; subsequent fetches fail; once the grace
// period elapses, the cache must be cleared and Get must return
// ErrCacheEmpty. The 10-minute production grace is shortened via
// newWithGrace so the test runs in <1s.
//
// Without this test the only handleFetchFailure path covered is the
// "still inside grace, keep stale cache" branch — meaning a future
// regression that swaps the comparator or zeroes the wrong field
// would silently let a fully-expired JWKS keep verifying tokens.
func TestHandleFetchFailure_ClearsCacheAfterGrace(t *testing.T) {
	// Stage 1: server returns valid JWKS so the initial fetch
	// installs at least one key.
	var serveValid atomic.Bool
	serveValid.Store(true)
	priv := generateTestRSA(t)
	jwksBody := encodeRSAJWKS("k1", priv)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !serveValid.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksBody)
	}))
	defer srv.Close()

	// 50ms grace so the test runs quickly. The cache is constructed
	// via the test-only seam; production callers go through New().
	c := newWithGrace(srv.URL, slog.Default(), 50*time.Millisecond)
	defer c.Close()

	// Wait for the initial successful fetch to populate the cache.
	if !waitForKey(c, "k1", 500*time.Millisecond) {
		t.Fatal("initial fetch never installed k1; cache stayed empty")
	}

	// Stage 2: flip the server to fail. The cache must stay valid
	// until grace elapses.
	serveValid.Store(false)

	// Trigger an explicit failure cycle inline — we don't want to
	// wait for the 5-minute refresh ticker.
	c.refresh() // first failure, still inside grace
	if !waitForKey(c, "k1", 200*time.Millisecond) {
		t.Fatal("cache cleared too early — staleGrace boundary regressed")
	}

	// Stage 3: wait past the grace window, then refresh again. Now
	// the cache must be cleared.
	time.Sleep(75 * time.Millisecond) // > 50ms grace
	c.refresh()                       // second failure, past grace

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := c.Get("k1"); errors.Is(err, ErrCacheEmpty) {
			return // success
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("cache should have been cleared after grace expired; Get still returns a key")
}

// TestRefresh_ZeroKey200RetainsStaleCache covers F-0248: a 200 response
// carrying an empty "keys" array must be treated as a soft fetch failure —
// the previously-installed key must survive (stale-grace), NOT be wiped, and
// fetchedAt must NOT advance (otherwise the grace window would reset on every
// empty fetch and the cache could never expire). Before the fix the swap was
// unconditional, so one empty 200 emptied the cache and every enrollment 503'd.
func TestRefresh_ZeroKey200RetainsStaleCache(t *testing.T) {
	var serveValid atomic.Bool
	serveValid.Store(true)
	priv := generateTestRSA(t)
	jwksBody := encodeRSAJWKS("k1", priv)
	emptyBody := []byte(`{"keys":[]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if serveValid.Load() {
			_, _ = w.Write(jwksBody)
			return
		}
		_, _ = w.Write(emptyBody) // 200 OK with zero keys
	}))
	defer srv.Close()

	c := newWithGrace(srv.URL, slog.Default(), 50*time.Millisecond)
	defer c.Close()

	if !waitForKey(c, "k1", 500*time.Millisecond) {
		t.Fatal("initial fetch never installed k1")
	}
	fetchedBefore := c.lastFetchedAt()

	// Flip to empty-200 and refresh within the grace window. The stale key
	// must still be served and fetchedAt must not move.
	serveValid.Store(false)
	c.refresh()

	if _, err := c.Get("k1"); err != nil {
		t.Fatalf("empty-200 wiped the cache (F-0248 regression): %v", err)
	}
	if got := c.lastFetchedAt(); !got.Equal(fetchedBefore) {
		t.Errorf("fetchedAt advanced on empty-200 fetch: before=%v after=%v", fetchedBefore, got)
	}

	// Past the grace window an empty 200 must let the stale cache expire,
	// proving it went through the soft-failure path rather than a silent swap.
	time.Sleep(75 * time.Millisecond)
	c.refresh()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := c.Get("k1"); errors.Is(err, ErrCacheEmpty) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("stale cache should have expired after grace on repeated empty-200")
}

// lastFetchedAt exposes fetchedAt for the zero-key test. Kept in the test file
// (same package) so production code carries no test-only accessor.
func (c *Cache) lastFetchedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fetchedAt
}

// waitForKey polls Get(kid) until it succeeds or the deadline expires.
func waitForKey(c *Cache, kid string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := c.Get(kid); err == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestRefresh_TransportErrorTakesFetchFailureBranch covers the
// `c.client.Get(...)` error branch in refresh() — a URL pointing at a
// closed port surfaces a connection-refused err, distinct from the
// non-200-status branch tested elsewhere. The cache must enter
// handleFetchFailure and stay empty.
func TestRefresh_TransportErrorTakesFetchFailureBranch(t *testing.T) {
	// Stand a server up so we know a free port, then close it so the
	// URL is unreachable for the refresh() call below.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	deadAddr := srv.URL
	srv.Close()

	c := newWithGrace(deadAddr, slog.Default(), 50*time.Millisecond)
	defer c.Close()
	// Background goroutine fired initial fetch already; explicitly
	// drive another to make the assertion deterministic.
	c.refresh()

	if _, err := c.Get("any"); !errors.Is(err, ErrCacheEmpty) {
		t.Errorf("expected ErrCacheEmpty after transport-failed refresh, got: %v", err)
	}
}
