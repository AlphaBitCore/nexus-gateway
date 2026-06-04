package assistant

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// owner_integration_test.go exercises the AC-3 cross-pod affinity mechanism against a
// REAL Redis (beyond the miniredis unit tests in owner_test.go) — two ownerRegistry
// instances sharing one Redis stand in for two CP replicas. It SKIPs unless
// NEXUS_TEST_REDIS_ADDR points at a reachable Redis (so CI without Redis skips cleanly
// — a real env precondition, not a deferred "fix later"). Run locally with e.g.
//
//	NEXUS_TEST_REDIS_ADDR=localhost:6379 go test ./internal/assistant/ -run TestOwnerRegistry_RealRedis -v
//
// It validates the decision that produces the HTTP 421 wrong_owner: a confirm POST that
// lands on a non-owner pod sees (mine=false, known=true) and must 421; the owner sees
// (mine=true); a lapsed TTL yields (known=false) so the non-owner fails OPEN (no 421);
// and a newer claim takes ownership over (newest-turn-owns + dead-owner takeover).
func TestOwnerRegistry_RealRedis(t *testing.T) {
	addr := os.Getenv("NEXUS_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEXUS_TEST_REDIS_ADDR not set — needs a reachable Redis for the live ≥2-replica AC-3 check")
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis at %s unreachable: %v", addr, err)
	}

	// Two replicas sharing the same Redis. A long TTL for the routing checks.
	scope := "itest-user:itest-sess-" + time.Now().Format("150405.000000")
	defer rdb.Del(ctx, ownerKey(scope))
	podA := newOwnerRegistry(rdb, "pod-A", time.Minute)
	podB := newOwnerRegistry(rdb, "pod-B", time.Minute)

	// Pod A starts the turn → claims ownership.
	podA.claim(ctx, scope)

	if mine, known := podA.owner(ctx, scope); !known || !mine {
		t.Fatalf("the owning pod must report (mine=true, known=true), got (mine=%v, known=%v)", mine, known)
	}
	// The 421 trigger: a confirm misrouted to pod B sees a known owner that is not it.
	if mine, known := podB.owner(ctx, scope); !known || mine {
		t.Fatalf("a non-owner pod must report (mine=false, known=true) → 421, got (mine=%v, known=%v)", mine, known)
	}

	// Newest-turn-owns / dead-owner takeover: pod B claims (a new turn) → ownership moves.
	podB.claim(ctx, scope)
	if mine, known := podB.owner(ctx, scope); !known || !mine {
		t.Fatalf("after takeover pod B must own the session, got (mine=%v, known=%v)", mine, known)
	}
	if mine, _ := podA.owner(ctx, scope); mine {
		t.Fatal("pod A must no longer own the session after pod B's claim")
	}

	// Lapsed TTL → no live owner → fail-open (known=false, no 421).
	shortPod := newOwnerRegistry(rdb, "pod-A", 200*time.Millisecond)
	ttlScope := scope + "-ttl"
	defer rdb.Del(ctx, ownerKey(ttlScope))
	shortPod.claim(ctx, ttlScope)
	time.Sleep(400 * time.Millisecond)
	if _, known := podB.owner(ctx, ttlScope); known {
		t.Fatal("an expired ownership key must read as known=false (fail-open, no 421)")
	}
}
