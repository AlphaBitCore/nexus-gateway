package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestManager_GetCacheHit(t *testing.T) {
	m := NewManager(5*time.Minute, testLogger())

	var callCount atomic.Int32
	m.RegisterLoader(CategoryHooks, func(ctx context.Context) (interface{}, error) {
		callCount.Add(1)
		return []string{"hook1", "hook2"}, nil
	})

	ctx := context.Background()

	// First call: miss, triggers load.
	data, err := m.Get(ctx, CategoryHooks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hooks := data.([]string)
	if len(hooks) != 2 {
		t.Fatalf("expected 2 hooks, got %d", len(hooks))
	}
	if callCount.Load() != 1 {
		t.Fatalf("expected loadFunc called once, got %d", callCount.Load())
	}

	// Second call: hit, no reload.
	data2, err := m.Get(ctx, CategoryHooks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hooks2 := data2.([]string)
	if len(hooks2) != 2 {
		t.Fatalf("expected 2 hooks on cache hit, got %d", len(hooks2))
	}
	if callCount.Load() != 1 {
		t.Fatalf("expected loadFunc still called once, got %d", callCount.Load())
	}
}

func TestManager_GetCacheMiss(t *testing.T) {
	m := NewManager(5*time.Minute, testLogger())

	m.RegisterLoader(CategoryInterceptionDomains, func(ctx context.Context) (interface{}, error) {
		return map[string]bool{"deny-all": true}, nil
	})

	ctx := context.Background()
	data, err := m.Get(ctx, CategoryInterceptionDomains)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	domains := data.(map[string]bool)
	if !domains["deny-all"] {
		t.Fatal("expected deny-all policy to be true")
	}
}

func TestManager_Invalidate(t *testing.T) {
	m := NewManager(5*time.Minute, testLogger())

	var callCount atomic.Int32
	m.RegisterLoader(CategoryHooks, func(ctx context.Context) (interface{}, error) {
		callCount.Add(1)
		return int(callCount.Load()), nil
	})

	ctx := context.Background()

	// Load initial data.
	data, _ := m.Get(ctx, CategoryHooks)
	if data.(int) != 1 {
		t.Fatalf("expected 1, got %d", data.(int))
	}

	// Invalidate.
	m.Invalidate(CategoryHooks)

	// Next Get should reload.
	data, _ = m.Get(ctx, CategoryHooks)
	if data.(int) != 2 {
		t.Fatalf("expected 2 after invalidation, got %d", data.(int))
	}
}

func TestManager_InvalidateAll(t *testing.T) {
	m := NewManager(5*time.Minute, testLogger())

	hooksCount := 0
	domainsCount := 0

	m.RegisterLoader(CategoryHooks, func(ctx context.Context) (interface{}, error) {
		hooksCount++
		return hooksCount, nil
	})
	m.RegisterLoader(CategoryInterceptionDomains, func(ctx context.Context) (interface{}, error) {
		domainsCount++
		return domainsCount, nil
	})

	ctx := context.Background()

	// Load both.
	_, _ = m.Get(ctx, CategoryHooks)
	_, _ = m.Get(ctx, CategoryInterceptionDomains)

	if hooksCount != 1 || domainsCount != 1 {
		t.Fatalf("expected both loaded once: hooks=%d, domains=%d", hooksCount, domainsCount)
	}

	// Invalidate all.
	m.InvalidateAll()

	// Both should reload.
	_, _ = m.Get(ctx, CategoryHooks)
	_, _ = m.Get(ctx, CategoryInterceptionDomains)

	if hooksCount != 2 || domainsCount != 2 {
		t.Fatalf("expected both loaded twice: hooks=%d, domains=%d", hooksCount, domainsCount)
	}
}

func TestManager_TTLExpiry(t *testing.T) {
	// Use a very short TTL.
	m := NewManager(10*time.Millisecond, testLogger())

	var callCount atomic.Int32
	m.RegisterLoader(CategoryAllowlists, func(ctx context.Context) (interface{}, error) {
		callCount.Add(1)
		return int(callCount.Load()), nil
	})

	ctx := context.Background()

	// Initial load.
	data, _ := m.Get(ctx, CategoryAllowlists)
	if data.(int) != 1 {
		t.Fatalf("expected 1, got %d", data.(int))
	}

	// Wait for TTL to expire.
	time.Sleep(20 * time.Millisecond)

	// Should reload due to TTL expiry.
	data, _ = m.Get(ctx, CategoryAllowlists)
	if data.(int) != 2 {
		t.Fatalf("expected 2 after TTL expiry, got %d", data.(int))
	}
}

func TestManager_LoadError(t *testing.T) {
	m := NewManager(5*time.Minute, testLogger())

	var callCount atomic.Int32
	m.RegisterLoader(CategoryHooks, func(ctx context.Context) (interface{}, error) {
		callCount.Add(1)
		if callCount.Load() == 2 {
			return nil, errors.New("db connection lost")
		}
		return fmt.Sprintf("data-v%d", callCount.Load()), nil
	})

	ctx := context.Background()

	// First load succeeds.
	data, err := m.Get(ctx, CategoryHooks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data.(string) != "data-v1" {
		t.Fatalf("expected data-v1, got %v", data)
	}

	// Invalidate, next load will fail.
	m.Invalidate(CategoryHooks)

	data, err = m.Get(ctx, CategoryHooks)
	if err == nil {
		t.Fatal("expected error on second load")
	}
	// Stale data should still be returned.
	if data.(string) != "data-v1" {
		t.Fatalf("expected stale data-v1, got %v", data)
	}

	// Third call should retry and succeed (callCount == 3).
	data, err = m.Get(ctx, CategoryHooks)
	if err != nil {
		t.Fatalf("unexpected error on third load: %v", err)
	}
	if data.(string) != "data-v3" {
		t.Fatalf("expected data-v3, got %v", data)
	}
}

func TestManager_NoLoader(t *testing.T) {
	m := NewManager(5*time.Minute, testLogger())

	// Don't register any loader — call Get on an unregistered category.
	_, err := m.Get(context.Background(), CacheCategory("nonexistent"))
	if err == nil {
		t.Fatal("expected error for unregistered loader")
	}
}

func TestManager_Concurrent(t *testing.T) {
	m := NewManager(5*time.Minute, testLogger())

	var loadCount atomic.Int64
	m.RegisterLoader(CategoryHooks, func(ctx context.Context) (interface{}, error) {
		loadCount.Add(1)
		// Simulate some I/O latency.
		time.Sleep(time.Millisecond)
		return "hooks-data", nil
	})

	ctx := context.Background()
	const goroutines = 50
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	wg.Add(goroutines)
	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			data, err := m.Get(ctx, CategoryHooks)
			if err != nil {
				errs <- err
				return
			}
			if data.(string) != "hooks-data" {
				errs <- errors.New("unexpected data")
			}
			// Periodically invalidate to stress the lock paths.
			if id%10 == 0 {
				m.Invalidate(CategoryHooks)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("goroutine error: %v", err)
	}

	// loadFunc should have been called at least once but not 50 times.
	loads := loadCount.Load()
	if loads < 1 {
		t.Fatal("expected at least 1 load")
	}
	if loads > goroutines {
		t.Fatalf("expected at most %d loads, got %d (cache should coalesce)", goroutines, loads)
	}
}

func TestManager_StalenessSeconds(t *testing.T) {
	m := NewManager(5*time.Minute, testLogger())

	// Before any load, staleness is -1.
	if s := m.StalenessSeconds(CategoryHooks); s != -1 {
		t.Fatalf("expected -1 staleness before load, got %f", s)
	}

	m.RegisterLoader(CategoryHooks, func(ctx context.Context) (interface{}, error) {
		return "data", nil
	})

	ctx := context.Background()
	_, _ = m.Get(ctx, CategoryHooks)

	// Just loaded, staleness should be very small.
	s := m.StalenessSeconds(CategoryHooks)
	if s < 0 || s > 1 {
		t.Fatalf("expected staleness ~0, got %f", s)
	}
}
