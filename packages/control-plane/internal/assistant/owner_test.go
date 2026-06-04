package assistant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// TestOwnerRegistry_ClaimAndOwner pins the registry's core contract: after pod A
// claims a session, A sees itself as owner and pod B sees A (not itself); an
// unclaimed session is "unknown" (fail-open, no 421); a nil registry is always
// the local owner.
func TestOwnerRegistry_ClaimAndOwner(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()

	oA := newOwnerRegistry(rdb, "podA", time.Minute)
	oB := newOwnerRegistry(rdb, "podB", time.Minute)
	oA.claim(ctx, "u1:sess1")

	if mine, known := oA.owner(ctx, "u1:sess1"); !mine || !known {
		t.Errorf("podA must own its own claim: mine=%v known=%v", mine, known)
	}
	if mine, known := oB.owner(ctx, "u1:sess1"); mine || !known {
		t.Errorf("podB must see podA as owner: mine=%v known=%v", mine, known)
	}
	if _, known := oB.owner(ctx, "u1:unclaimed"); known {
		t.Error("an unclaimed session must be unknown (so the caller fails open, no 421)")
	}

	// A nil registry (no Redis / single replica) is always the local owner and is
	// never "known" — so the Confirm handler never 421s.
	var nilReg *ownerRegistry
	if mine, known := nilReg.owner(ctx, "u1:sess1"); !mine || known {
		t.Errorf("nil registry must report (true,false), got mine=%v known=%v", mine, known)
	}
	nilReg.claim(ctx, "u1:sess1") // must not panic
}

// TestConfirm_421WhenOwnedByAnotherPod is the affinity safety-net assertion: a
// confirm POST that lands on a pod which does NOT hold the parked confirm, while
// another live pod owns the session, returns 421 (retry at the owner) instead of
// a misleading 409.
func TestConfirm_421WhenOwnedByAnotherPod(t *testing.T) {
	rdb := newTestRedis(t)
	// Pod A claims the session. userID is empty in this unauthenticated test, so
	// the owner key the Confirm handler builds is ":sess1".
	newOwnerRegistry(rdb, "podA", time.Minute).claim(context.Background(), ":sess1")

	// Pod B receives the confirm — it has no parked confirm for this session.
	hB := New(Config{Redis: rdb, OwnerID: "podB"})
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"sessionId":"sess1","callId":"c1","decision":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := hB.Confirm(e.NewContext(req, rec)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("a confirm for a session owned by another pod must be 421, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "wrong_owner") {
		t.Errorf("expected wrong_owner code, got %s", rec.Body.String())
	}
}

// TestConfirm_FailOpenWhenRedisDown is the most operationally important fail-open
// guarantee: if Redis is unreachable, the owner check must NOT 421 (and must not
// 500/hang) — it degrades to the normal 409 so a Redis hiccup never blocks legit
// confirms.
func TestConfirm_FailOpenWhenRedisDown(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	// Establish ownership by ANOTHER pod first (so a working Redis WOULD 421)...
	newOwnerRegistry(rdb, "podA", time.Minute).claim(context.Background(), ":sess1")
	// ...then take Redis down. The owner GET now errors → fail-open → no 421.
	mr.Close()

	h := New(Config{Redis: rdb, OwnerID: "podB"})
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"sessionId":"sess1","callId":"c1","decision":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := h.Confirm(e.NewContext(req, rec)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("a Redis-down owner check must fail open to 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "wrong_owner") {
		t.Errorf("must not 421 when the owner registry is unreachable: %s", rec.Body.String())
	}
}

// TestConfirm_NoFalse421OnOwnPodMiss confirms the local-first design: when THIS
// pod owns the session but has no parked confirm (already answered / timed out),
// the answer is the normal 409, NOT a 421 — the owner registry must not turn a
// legitimate "expired" into a misroute.
func TestConfirm_NoFalse421OnOwnPodMiss(t *testing.T) {
	rdb := newTestRedis(t)
	h := New(Config{Redis: rdb, OwnerID: "podA"})
	h.owners.claim(context.Background(), ":sess1") // this pod owns it

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"sessionId":"sess1","callId":"c1","decision":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := h.Confirm(e.NewContext(req, rec)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("own-pod miss must be 409 expired, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "wrong_owner") {
		t.Errorf("must not 421 when this pod owns the session: %s", rec.Body.String())
	}
}
