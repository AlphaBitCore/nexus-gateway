package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCache_GetTyped(t *testing.T) {
	callCount := 0
	c := NewCache[[]string]("test", 5*time.Minute, func(ctx context.Context) ([]string, error) {
		callCount++
		return []string{"a", "b"}, nil
	}, testLogger())

	data, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// data is []string, no type assertion needed
	if len(data) != 2 || data[0] != "a" {
		t.Fatalf("unexpected data: %v", data)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 load, got %d", callCount)
	}

	// Second call: cache hit
	data2, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data2) != 2 {
		t.Fatalf("expected cached data, got %v", data2)
	}
	if callCount != 1 {
		t.Fatalf("expected still 1 load, got %d", callCount)
	}
}

func TestCache_Invalidate(t *testing.T) {
	callCount := 0
	c := NewCache[int]("test", 5*time.Minute, func(ctx context.Context) (int, error) {
		callCount++
		return callCount, nil
	}, testLogger())

	data, _ := c.Get(context.Background())
	if data != 1 {
		t.Fatalf("expected 1, got %d", data)
	}

	c.InvalidateCache()

	data, _ = c.Get(context.Background())
	if data != 2 {
		t.Fatalf("expected 2 after invalidation, got %d", data)
	}
}

func TestCache_StaleOnError(t *testing.T) {
	callCount := 0
	c := NewCache[string]("test", 5*time.Minute, func(ctx context.Context) (string, error) {
		callCount++
		if callCount == 2 {
			return "", errors.New("db error")
		}
		return "v" + string(rune('0'+callCount)), nil
	}, testLogger())

	data, err := c.Get(context.Background())
	if err != nil || data != "v1" {
		t.Fatalf("expected v1, got %q err=%v", data, err)
	}

	c.InvalidateCache()

	// Load fails, should return stale data
	data, err = c.Get(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if data != "v1" {
		t.Fatalf("expected stale v1, got %q", data)
	}
}

func TestCache_Name(t *testing.T) {
	c := NewCache[int]("hooks", 0, nil, testLogger())
	if c.Name() != "hooks" {
		t.Fatalf("expected 'hooks', got %q", c.Name())
	}
}

func TestCache_StalenessSeconds(t *testing.T) {
	c := NewCache[string]("test", 5*time.Minute, func(ctx context.Context) (string, error) {
		return "data", nil
	}, testLogger())

	if s := c.StalenessSeconds(); s != -1 {
		t.Fatalf("expected -1 before load, got %f", s)
	}

	_, _ = c.Get(context.Background())

	s := c.StalenessSeconds()
	if s < 0 || s > 1 {
		t.Fatalf("expected ~0 staleness, got %f", s)
	}
}
