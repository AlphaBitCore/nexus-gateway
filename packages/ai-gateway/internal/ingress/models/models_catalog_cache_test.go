package models

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

type countingLookup struct {
	models    []store.Model
	listCalls int
}

func (c *countingLookup) GetModel(context.Context, string) (*store.Model, error) {
	return nil, nil
}
func (c *countingLookup) GetModelByCode(context.Context, string) (*store.Model, error) {
	return nil, nil
}
func (c *countingLookup) ListEnabledModels(context.Context) ([]store.Model, error) {
	c.listCalls++
	return c.models, nil
}

func newMiniRedis(t *testing.T) (*miniredis.Miniredis, redis.UniversalClient) {
	t.Helper()
	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(s.Close)
	return s, redis.NewClient(&redis.Options{Addr: s.Addr()})
}

func TestCatalogEntries_missBuildsAndSetsWithTTL(t *testing.T) {
	s, rdb := newMiniRedis(t)
	lookup := &countingLookup{models: []store.Model{sampleModel()}}

	entries, err := catalogEntries(context.Background(), lookup, rdb, devLogger)
	if err != nil {
		t.Fatalf("catalogEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries: got %d, want 1", len(entries))
	}
	if lookup.listCalls != 1 {
		t.Errorf("miss should hit DB once, got %d", lookup.listCalls)
	}
	if !s.Exists(catalogCacheKey) {
		t.Fatal("cache key not set on miss")
	}
	ttl := s.TTL(catalogCacheKey)
	if ttl <= 0 || ttl > 5*time.Minute {
		t.Errorf("TTL: got %v, want ~5m", ttl)
	}
}

func TestCatalogEntries_hitSkipsDB(t *testing.T) {
	_, rdb := newMiniRedis(t)
	lookup := &countingLookup{models: []store.Model{sampleModel()}}

	_, _ = catalogEntries(context.Background(), lookup, rdb, devLogger)    // miss
	_, err := catalogEntries(context.Background(), lookup, rdb, devLogger) // hit
	if err != nil {
		t.Fatalf("catalogEntries: %v", err)
	}
	if lookup.listCalls != 1 {
		t.Errorf("second call should be a cache hit (no DB), listCalls=%d", lookup.listCalls)
	}
}

func TestCatalogEntries_redisErrorFallsBackToBuild(t *testing.T) {
	s, rdb := newMiniRedis(t)
	s.Close() // force every Redis op to error
	lookup := &countingLookup{models: []store.Model{sampleModel()}}

	entries, err := catalogEntries(context.Background(), lookup, rdb, devLogger)
	if err != nil {
		t.Fatalf("redis error must not surface: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("entries: got %d, want 1", len(entries))
	}
	if lookup.listCalls != 1 {
		t.Errorf("fallback should build from DB, listCalls=%d", lookup.listCalls)
	}
}

func TestCatalogEntries_nilRedisBuildsDirectly(t *testing.T) {
	lookup := &countingLookup{models: []store.Model{sampleModel()}}
	entries, err := catalogEntries(context.Background(), lookup, nil, devLogger)
	if err != nil || len(entries) != 1 {
		t.Fatalf("nil redis: entries=%d err=%v", len(entries), err)
	}
}
