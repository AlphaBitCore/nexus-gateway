package wiring

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
)

type fakeReadAuth struct {
	meta *vkauth.VKMeta
	err  error
}

func (f *fakeReadAuth) Authenticate(_ context.Context, _ *http.Request) (*vkauth.VKMeta, error) {
	return f.meta, f.err
}

type fakeReadLimiter struct {
	allow      bool
	retryAfter int
	gotKey     string
	gotLimit   int
}

func (f *fakeReadLimiter) Allow(key string, limit int, _ int64) (bool, int) {
	f.gotKey = key
	f.gotLimit = limit
	return f.allow, f.retryAfter
}

func intPtr(i int) *int { return &i }

func okNext(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("served"))
}

// A VK over its RPM cap gets 429 and never reaches the inner handler.
func TestVKReadRateLimit_Throttles(t *testing.T) {
	auth := &fakeReadAuth{meta: &vkauth.VKMeta{ID: "vk-1", RateLimitRpm: intPtr(60)}}
	lim := &fakeReadLimiter{allow: false, retryAfter: 7}
	served := false
	h := vkReadRateLimit(auth, lim, discardLogger())(func(w http.ResponseWriter, r *http.Request) {
		served = true
		okNext(w, r)
	})

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rr.Code)
	}
	if served {
		t.Error("inner handler must not run when throttled")
	}
	if rr.Header().Get("Retry-After") != "7" {
		t.Errorf("Retry-After = %q, want 7", rr.Header().Get("Retry-After"))
	}
	if lim.gotKey != "vk-1" || lim.gotLimit != 60 {
		t.Errorf("limiter called with key=%q limit=%d, want vk-1/60", lim.gotKey, lim.gotLimit)
	}
}

// A VK under its cap passes through and the inner handler runs.
func TestVKReadRateLimit_AllowsUnderCap(t *testing.T) {
	auth := &fakeReadAuth{meta: &vkauth.VKMeta{ID: "vk-2", RateLimitRpm: intPtr(100)}}
	lim := &fakeReadLimiter{allow: true}
	h := vkReadRateLimit(auth, lim, discardLogger())(okNext)

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-RateLimit-Limit") != "100" {
		t.Errorf("X-RateLimit-Limit = %q, want 100", rr.Header().Get("X-RateLimit-Limit"))
	}
}

// An unauthenticated request defers to the inner handler (which emits the
// canonical 401); the wrapper must not consume a limiter slot or short-circuit.
func TestVKReadRateLimit_AuthFailDefersToInner(t *testing.T) {
	auth := &fakeReadAuth{err: errors.New("no vk")}
	lim := &fakeReadLimiter{allow: true}
	served := false
	h := vkReadRateLimit(auth, lim, discardLogger())(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusUnauthorized)
	})

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if !served {
		t.Error("inner handler must run on auth failure so it emits the canonical 401")
	}
	if lim.gotKey != "" {
		t.Error("limiter must not be consulted when auth fails")
	}
}

// A VK with no configured RPM cap is unthrottled (parity with ServeProxy).
func TestVKReadRateLimit_NoCapPassesThrough(t *testing.T) {
	auth := &fakeReadAuth{meta: &vkauth.VKMeta{ID: "vk-3"}} // RateLimitRpm nil
	lim := &fakeReadLimiter{allow: false}                   // would 429 if consulted
	h := vkReadRateLimit(auth, lim, discardLogger())(okNext)

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("VK with no cap should pass through, got %d", rr.Code)
	}
	if lim.gotKey != "" {
		t.Error("limiter must not be consulted for a VK with no RPM cap")
	}
}

// Nil limiter or nil auth → pass-through (limiter/auth-disabled boot).
func TestVKReadRateLimit_NilDepsPassThrough(t *testing.T) {
	h := vkReadRateLimit(nil, nil, discardLogger())(okNext)
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("nil deps should pass through, got %d", rr.Code)
	}
}
