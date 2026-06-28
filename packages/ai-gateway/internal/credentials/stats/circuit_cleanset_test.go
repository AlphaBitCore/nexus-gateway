package credstats

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// TestBuffer_CircuitCleanSet_NeverFailedSkipsRedis verifies the T1b circuit
// clean-set: a credential that has never failed gets confirmed-clean on its first
// success and thereafter its success path touches NO circuit Redis (no HGet, no
// HSet auth_fails=0). Observable: the circuit hash is never CREATED for a
// never-failed credential (the legacy path used to HSet auth_fails=0, creating
// an empty hash on every success).
func TestBuffer_CircuitCleanSet_NeverFailedSkipsRedis(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	b := New(rdb, nil, nil, nil)
	b.EnableWriteBehind()

	const cred = "cred-clean-1"
	circuitKey := credstate.CircuitKey(cred)

	b.RecordAttempt(cred, 200, "") // first success: confirms clean
	b.RecordAttempt(cred, 200, "") // subsequent: must skip circuit Redis entirely

	if mini.Exists(circuitKey) {
		t.Errorf("circuit hash %s created for a never-failed credential — clean-set should skip the auth_fails reset", circuitKey)
	}
}

// TestBuffer_CircuitCleanSet_FailureStillOpens verifies the clean-set never
// suppresses a real transition: an auth-fail past the threshold still OPENS the
// circuit (the cred is removed from the clean set on failure), and a later
// success still CLOSES it. This is the load-bearing safety property.
func TestBuffer_CircuitCleanSet_FailureStillOpens(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	// Default thresholds: AuthFailThreshold drives OPEN. Use nil resolver =
	// credstate.DefaultThresholds.
	b := New(rdb, nil, nil, nil)
	b.EnableWriteBehind()

	const cred = "cred-clean-2"
	circuitKey := credstate.CircuitKey(cred)

	// Confirm clean first.
	b.RecordAttempt(cred, 200, "")

	// Drive auth-fails to the threshold → circuit must OPEN despite prior clean.
	thr := credstate.DefaultThresholds.AuthFailThreshold
	for range thr {
		b.RecordAttempt(cred, 401, "bad key")
	}
	state := mini.HGet(circuitKey, credstate.CircuitFieldState)
	if state != credstate.CircuitOpen {
		t.Fatalf("circuit state after %d auth-fails = %q, want open (clean-set must not suppress the transition)", thr, state)
	}

	// A success now must CLOSE it (DEL the hash).
	b.RecordAttempt(cred, 200, "")
	if mini.Exists(circuitKey) {
		t.Errorf("circuit hash still exists after a successful recovery — close-on-success was suppressed")
	}
}
