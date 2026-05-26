package aiguard

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return s, rdb
}

func TestCache_SetGet_Roundtrip(t *testing.T) {
	_, rdb := newMiniRedis(t)
	c := NewCache(rdb)
	ctx := context.Background()
	want := &Response{Decision: "approve", Confidence: 0.9, Labels: []string{"clean"}}

	if err := c.Set(ctx, "k1", want, 30*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := c.Get(ctx, "k1")
	if err != nil || !ok {
		t.Fatalf("Get: hit=%v err=%v", ok, err)
	}
	if got.Decision != want.Decision || got.Confidence != want.Confidence {
		t.Fatalf("mismatch: got %+v want %+v", got, want)
	}
}

func TestCache_Get_Miss(t *testing.T) {
	_, rdb := newMiniRedis(t)
	c := NewCache(rdb)
	_, ok, err := c.Get(context.Background(), "missing")
	if err != nil {
		t.Fatalf("Get miss returned error: %v", err)
	}
	if ok {
		t.Fatal("expected miss, got hit")
	}
}

func TestCache_TTLZeroSkips(t *testing.T) {
	s, rdb := newMiniRedis(t)
	c := NewCache(rdb)
	ctx := context.Background()
	if err := c.Set(ctx, "k-zero", &Response{Decision: "approve"}, 0); err != nil {
		t.Fatalf("Set with zero TTL: %v", err)
	}
	if got := len(s.Keys()); got != 0 {
		t.Fatalf("TTL=0 must skip write; got %d keys", got)
	}
}

func TestCache_KeyFor_IsStable(t *testing.T) {
	k1 := CacheKey("prompt_injection", "hello", "fp-abc")
	k2 := CacheKey("prompt_injection", "hello", "fp-abc")
	if k1 != k2 {
		t.Fatalf("key not deterministic: %q vs %q", k1, k2)
	}
	if len(k1) < 11 || k1[:11] != "aiguard:v1:" {
		t.Fatalf("key missing prefix: %q", k1)
	}
}

func TestCache_NilClient_Safe(t *testing.T) {
	c := NewCache(nil)
	_, ok, err := c.Get(context.Background(), "x")
	if err != nil || ok {
		t.Fatalf("nil client Get: err=%v hit=%v", err, ok)
	}
	if err := c.Set(context.Background(), "x", &Response{Decision: "approve"}, time.Second); err != nil {
		t.Fatalf("nil client Set: %v", err)
	}
}
