package quota

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestUsageCache_RunFlusher_FlushesOnShutdown verifies the background flusher
// drains pending write-behind deltas to Redis when its context is cancelled —
// so a graceful shutdown loses NOTHING (bounded loss is only on a hard crash).
func TestUsageCache_RunFlusher_FlushesOnShutdown(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	c := NewUsageCache(rdb, testLogger())
	c.EnableWriteBehind()

	ctx, cancel := context.WithCancel(context.Background())
	period := "2026-06"
	if err := c.IncrMulti(ctx, []UsageLevel{{TargetType: "virtual_key", TargetID: "vk1"}}, period, 77); err != nil {
		t.Fatalf("IncrMulti: %v", err)
	}

	// Long interval so the only flush is the shutdown one.
	done := make(chan struct{})
	go func() { c.RunFlusher(ctx, time.Hour); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunFlusher did not return after ctx cancel")
	}

	got, err := c.GetUsage(context.Background(), "virtual_key", "vk1", period)
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if got != 77 {
		t.Errorf("usage after shutdown flush = %d, want 77 (pending delta must flush on cancel)", got)
	}
}
