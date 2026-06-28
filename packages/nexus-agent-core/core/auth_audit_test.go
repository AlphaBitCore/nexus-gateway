package core

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestJWTTokenSource_ConcurrentNearExpiryRefreshesOnce guards the refresh-race fix:
// many concurrent Credential calls on a near-expiry token must serialize and hit the
// OAuth grant exactly once — not stampede it and rotate the refresh token N times,
// where a late goroutine could then present an already-rotated (stale) refresh token.
func TestJWTTokenSource_ConcurrentNearExpiryRefreshesOnce(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	_ = store.Set("local", SecretAccessToken, makeTestJWT(t, now.Add(10*time.Second))) // inside the refresh skew
	_ = store.Set("local", SecretRefreshToken, "refresh-1")
	fresh := makeTestJWT(t, now.Add(time.Hour))

	var refreshes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&refreshes, 1)
		time.Sleep(15 * time.Millisecond) // widen the window so a racing impl would double-refresh
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: fresh, RefreshToken: "refresh-2", ExpiresIn: 3600})
	}))
	defer srv.Close()

	src := newJWTSource(t, store, srv.URL, now)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, v, err := src.Credential(context.Background())
			if err != nil || v != "Bearer "+fresh {
				t.Errorf("Credential under contention: v=%q err=%v", v, err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Fatalf("near-expiry refresh must happen exactly once under concurrency, got %d", got)
	}
}
