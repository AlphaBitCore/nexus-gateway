package configcache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeyCache_TTLZeroDisablesExpiry(t *testing.T) {
	// TTL=0 means "no expiry" — without the early-return in expired(),
	// time.Now().Sub(loadedAt) would always be > 0 and every entry
	// would appear expired (Get becomes a permanent miss).
	c, err := NewKeyCache[string, int](
		func(_ context.Context, k string) (int, error) { return len(k), nil },
		16, 0, // ttl=0 disables expiry
	)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_, _ = c.Get(context.Background(), "abc")
	}
	hits, misses, _ := c.Stats()
	// Single miss (first Get), then two hits — would be 3 misses if
	// TTL=0 didn't disable expiry.
	if hits != 2 || misses != 1 {
		t.Errorf("TTL=0 disable: hits=%d misses=%d, want 2/1", hits, misses)
	}
}

func TestKeyCache_PurgeEmptyDoesNotFireInvalidate(t *testing.T) {
	// Purge on an empty cache must NOT increment counters or fire
	// onInvalidate — would mislead metrics into showing "we purged
	// N entries" when nothing happened.
	invs := 0
	c, _ := NewKeyCache[string, int](
		func(context.Context, string) (int, error) { return 0, nil },
		16, time.Minute,
		WithKeyOnInvalidate(func(_ string, n int) { invs += n }),
	)
	c.Purge()
	if invs != 0 {
		t.Errorf("empty Purge fired callback: invs=%d", invs)
	}
}

func TestKeyCache_AllOptionsWired(t *testing.T) {
	// Pins that each WithKey* option actually populates the corresponding
	// keyOptions field. Without these tests, a refactor could silently
	// drop one of the callbacks (e.g. onHit) and metrics would stop
	// firing in prod.
	var hits, misses, invs int
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := func() time.Time { return time.Unix(0, 0).UTC() }

	c, err := NewKeyCache[string, int](
		func(_ context.Context, k string) (int, error) { return len(k), nil },
		16, time.Minute,
		WithKeyLogger(logger),
		WithKeyName("test-cache"),
		WithKeyClock(clock),
		WithKeyOnHit(func(string) { hits++ }),
		WithKeyOnMiss(func(string) { misses++ }),
		WithKeyOnInvalidate(func(_ string, n int) { invs += n }),
	)
	if err != nil {
		t.Fatalf("NewKeyCache: %v", err)
	}

	// Miss → onMiss fires.
	if _, err := c.Get(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	// Hit on same key → onHit fires.
	if _, err := c.Get(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	// Invalidate → onInvalidate fires.
	if removed := c.Invalidate("abc"); removed != 1 {
		t.Errorf("Invalidate removed %d", removed)
	}
	if hits != 1 || misses != 1 || invs != 1 {
		t.Errorf("callback counts: hits=%d misses=%d invs=%d, want 1/1/1", hits, misses, invs)
	}
	if c.Name() != "test-cache" {
		t.Errorf("WithKeyName not applied: %q", c.Name())
	}
}

func TestKeyCache_NilLoaderErrors(t *testing.T) {
	_, err := NewKeyCache[string, int](nil, 16, time.Minute)
	if err == nil {
		t.Fatal("nil loader should error")
	}
}

func TestKeyCache_ZeroCapacityErrors(t *testing.T) {
	_, err := NewKeyCache[string, int](
		func(context.Context, string) (int, error) { return 0, nil },
		0, time.Minute,
	)
	if err == nil {
		t.Fatal("zero capacity should error")
	}
}

func TestKeyCache_HitAndMiss(t *testing.T) {
	var loads atomic.Int64
	loader := func(ctx context.Context, k string) (int, error) {
		loads.Add(1)
		return len(k), nil
	}
	c, err := NewKeyCache[string, int](loader, 16, time.Minute)
	if err != nil {
		t.Fatalf("NewKeyCache: %v", err)
	}

	v, err := c.Get(context.Background(), "abc")
	if err != nil || v != 3 {
		t.Fatalf("first Get: v=%d err=%v", v, err)
	}
	v, err = c.Get(context.Background(), "abc")
	if err != nil || v != 3 {
		t.Fatalf("second Get: v=%d err=%v", v, err)
	}
	if loads.Load() != 1 {
		t.Errorf("loader called %d times, want 1", loads.Load())
	}
}

func TestKeyCache_TTLExpiry(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	var loads atomic.Int64
	loader := func(ctx context.Context, k string) (int, error) {
		loads.Add(1)
		return int(loads.Load()), nil
	}
	c, err := NewKeyCache[string, int](loader, 16, 100*time.Millisecond, WithKeyClock(clock))
	if err != nil {
		t.Fatalf("NewKeyCache: %v", err)
	}

	if v, _ := c.Get(context.Background(), "k"); v != 1 {
		t.Errorf("first: v=%d, want 1", v)
	}

	now = now.Add(50 * time.Millisecond)
	if v, _ := c.Get(context.Background(), "k"); v != 1 {
		t.Errorf("within-ttl: v=%d, want 1 (cached)", v)
	}

	now = now.Add(200 * time.Millisecond)
	if v, _ := c.Get(context.Background(), "k"); v != 2 {
		t.Errorf("post-ttl: v=%d, want 2 (re-loaded)", v)
	}
}

func TestKeyCache_SingleflightCollapse(t *testing.T) {
	var loads atomic.Int64
	gate := make(chan struct{})
	loader := func(ctx context.Context, k string) (string, error) {
		<-gate
		loads.Add(1)
		return k + "!", nil
	}
	c, err := NewKeyCache[string, string](loader, 16, time.Minute)
	if err != nil {
		t.Fatalf("NewKeyCache: %v", err)
	}

	const concurrent = 50
	var wg sync.WaitGroup
	results := make([]string, concurrent)
	for i := range concurrent {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, _ := c.Get(context.Background(), "shared")
			results[i] = v
		}(i)
	}

	time.Sleep(20 * time.Millisecond) // let the goroutines fan in
	close(gate)
	wg.Wait()

	if loads.Load() != 1 {
		t.Errorf("loader called %d times, want 1 (singleflight should collapse)", loads.Load())
	}
	for i, r := range results {
		if r != "shared!" {
			t.Errorf("result[%d]=%q, want shared!", i, r)
		}
	}
}

func TestKeyCache_LoaderErrorNotCached(t *testing.T) {
	var loads atomic.Int64
	loader := func(ctx context.Context, k string) (int, error) {
		n := loads.Add(1)
		if n < 3 {
			return 0, errors.New("transient")
		}
		return 42, nil
	}
	c, err := NewKeyCache[string, int](loader, 16, time.Minute)
	if err != nil {
		t.Fatalf("NewKeyCache: %v", err)
	}

	if _, err := c.Get(context.Background(), "k"); err == nil {
		t.Error("first call: expected error")
	}
	if _, err := c.Get(context.Background(), "k"); err == nil {
		t.Error("second call: expected error (errors must not be cached)")
	}
	if v, err := c.Get(context.Background(), "k"); err != nil || v != 42 {
		t.Errorf("third call: v=%d err=%v, want 42/nil", v, err)
	}
}

func TestKeyCache_Invalidate(t *testing.T) {
	var loads atomic.Int64
	loader := func(ctx context.Context, k string) (int, error) {
		loads.Add(1)
		return int(loads.Load()), nil
	}
	c, err := NewKeyCache[string, int](loader, 16, time.Minute)
	if err != nil {
		t.Fatalf("NewKeyCache: %v", err)
	}

	if v, _ := c.Get(context.Background(), "a"); v != 1 {
		t.Errorf("first: v=%d, want 1", v)
	}
	removed := c.Invalidate("a", "b") // b is not present
	if removed != 1 {
		t.Errorf("Invalidate returned %d, want 1", removed)
	}
	if v, _ := c.Get(context.Background(), "a"); v != 2 {
		t.Errorf("after invalidate: v=%d, want 2 (re-loaded)", v)
	}
}

func TestKeyCache_Purge(t *testing.T) {
	loader := func(ctx context.Context, k string) (int, error) {
		return len(k), nil
	}
	c, err := NewKeyCache[string, int](loader, 16, time.Minute)
	if err != nil {
		t.Fatalf("NewKeyCache: %v", err)
	}
	for _, k := range []string{"a", "bb", "ccc"} {
		_, _ = c.Get(context.Background(), k)
	}
	if c.Size() != 3 {
		t.Fatalf("pre-purge size: %d", c.Size())
	}
	c.Purge()
	if c.Size() != 0 {
		t.Errorf("post-purge size: %d, want 0", c.Size())
	}
}

func TestKeyCache_Stats(t *testing.T) {
	loader := func(ctx context.Context, k string) (int, error) {
		return 1, nil
	}
	c, err := NewKeyCache[string, int](loader, 16, time.Minute)
	if err != nil {
		t.Fatalf("NewKeyCache: %v", err)
	}
	_, _ = c.Get(context.Background(), "a") // miss
	_, _ = c.Get(context.Background(), "a") // hit
	_, _ = c.Get(context.Background(), "b") // miss
	c.Invalidate("a")

	hits, misses, inv := c.Stats()
	if hits != 1 {
		t.Errorf("hits: got %d, want 1", hits)
	}
	if misses != 2 {
		t.Errorf("misses: got %d, want 2", misses)
	}
	if inv != 1 {
		t.Errorf("invalidations: got %d, want 1", inv)
	}
}

func TestKeyCache_NilLoaderError(t *testing.T) {
	if _, err := NewKeyCache[string, int](nil, 16, time.Minute); err == nil {
		t.Error("expected error on nil loader")
	}
}

func TestKeyCache_BadCapacityError(t *testing.T) {
	loader := func(ctx context.Context, k string) (int, error) { return 0, nil }
	if _, err := NewKeyCache[string, int](loader, 0, time.Minute); err == nil {
		t.Error("expected error on capacity=0")
	}
}
