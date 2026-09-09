package jwtverifier_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
)

// replayServer answers the catchup endpoint with a fixed page.
func replayServer(t *testing.T, events []any, lastID int64) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"events": events, "lastId": lastID})
	}))
	t.Cleanup(s.Close)
	return s
}

// The defect, pinned in its own name.
//
// A catchup that reaches the tail with NOTHING to apply is the strongest
// evidence of currency there is: the replay endpoint answered and had nothing
// left to give. Clearing strict mode only when events came back meant a quiet
// deployment — which revokes nothing and so legitimately replays nothing —
// could prove itself current on every catchup and never leave.
//
// That matters because strict mode is not a degraded state, it is a
// deny-everything one: IsRevoked routes every check to introspect and
// jwt.Verifier turns any error from that call into a rejected token. On prod
// the control-plane sat in it for fifteen minutes after a restart, refusing
// freshly minted, correctly signed, unexpired tokens while every health check
// reported the service fine.
func TestRunCatchup_ReachingTheTailWithNoEventsLeavesStrictMode(t *testing.T) {
	server := replayServer(t, []any{}, 42)
	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		ReplayURL: server.URL + "/api/internal/revocations",
	})
	jwtverifier.SetStrict(ch, true)

	if err := ch.RunCatchup(context.Background()); err != nil {
		t.Fatalf("RunCatchup: %v", err)
	}
	if jwtverifier.StrictLoad(ch) {
		t.Error("a catchup that reached the tail proves the checker is current; " +
			"staying strict means every token keeps going to introspect, and an " +
			"unreachable introspect rejects all of them")
	}
}

// The path that already worked must keep working.
func TestRunCatchup_ApplyingEventsStillLeavesStrictMode(t *testing.T) {
	server := replayServer(t, []any{
		map[string]any{"id": 1, "scope": "user", "targetUserId": "u1", "revokedAt": "2026-08-27T00:00:00Z", "expiresAt": "2026-08-28T00:00:00Z"},
	}, 1)
	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		ReplayURL: server.URL + "/api/internal/revocations",
	})
	jwtverifier.SetStrict(ch, true)

	if err := ch.RunCatchup(context.Background()); err != nil {
		t.Fatalf("RunCatchup: %v", err)
	}
	if jwtverifier.StrictLoad(ch) {
		t.Error("applying events has always cleared strict mode and must continue to")
	}
}

// The security property this must not cost. A catchup that FAILED proves
// nothing — the checker may well have missed a revocation — so strict mode has
// to survive it. This is the arm that separates "clear when current" from
// "clear whenever catchup is called".
func TestRunCatchup_AFailedCatchupKeepsStrictMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // the 502 seen on prod during boot
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		ReplayURL: server.URL + "/api/internal/revocations",
	})
	jwtverifier.SetStrict(ch, true)

	if err := ch.RunCatchup(context.Background()); err == nil {
		t.Fatal("a 502 from the replay endpoint must be reported as an error")
	}
	if !jwtverifier.StrictLoad(ch) {
		t.Error("a catchup that could not reach the endpoint proves nothing about " +
			"currency; clearing strict mode here would admit tokens the checker " +
			"cannot vouch for")
	}
}

// A checker that was never strict must not be disturbed.
func TestRunCatchup_DoesNotDisturbANonStrictChecker(t *testing.T) {
	server := replayServer(t, []any{}, 9)
	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		ReplayURL: server.URL + "/api/internal/revocations",
	})
	if jwtverifier.StrictLoad(ch) {
		t.Fatal("a fresh checker should not start strict")
	}
	if err := ch.RunCatchup(context.Background()); err != nil {
		t.Fatalf("RunCatchup: %v", err)
	}
	if jwtverifier.StrictLoad(ch) {
		t.Error("catchup must never ENTER strict mode")
	}
}
