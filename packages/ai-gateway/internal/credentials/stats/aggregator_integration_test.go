package credstats

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// TestBuffer_WriteBehind_StatsDeferThenFlush verifies the T1b credstats hot-path
// contract: with write-behind on, a successful RecordAttempt does NOT write the
// stats hash to Redis on the request path (the per-attempt pipeline is the
// biggest single Redis cost), and FlushStats later persists the summed count +
// latest timestamps + dirty mark — identical end state to the synchronous path.
func TestBuffer_WriteBehind_StatsDeferThenFlush(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	b := New(rdb, nil, nil, nil)
	b.EnableWriteBehind()

	const cred = "cred-wb-1"
	statsKey := credstate.StatsKey(cred)

	// Two successful attempts on the hot path — must NOT reach Redis yet.
	b.RecordAttempt(cred, 200, "")
	b.RecordAttempt(cred, 200, "")
	if mini.Exists(statsKey) {
		t.Fatalf("write-behind leaked stats to Redis on the hot path: %s exists before flush", statsKey)
	}

	// Flush persists: cnt summed, timestamps set, dirty marked.
	if err := b.FlushStats(context.Background()); err != nil {
		t.Fatalf("FlushStats: %v", err)
	}
	cnt := mini.HGet(statsKey, credstate.StatsFieldCount)
	if cnt != "2" {
		t.Errorf("cnt after flush = %q, want 2 (summed)", cnt)
	}
	if ts := mini.HGet(statsKey, credstate.StatsFieldOkAt); ts == "" {
		t.Errorf("ok_at not set after flush")
	}
	if ts := mini.HGet(statsKey, credstate.StatsFieldUsedAt); ts == "" {
		t.Errorf("used_at not set after flush")
	}
	members, _ := rdb.SMembers(context.Background(), credstate.StatsDirtySet).Result()
	found := false
	for _, m := range members {
		if m == cred {
			found = true
		}
	}
	if !found {
		t.Errorf("dirty set missing %s after flush (Hub flush job would never drain it)", cred)
	}
}
