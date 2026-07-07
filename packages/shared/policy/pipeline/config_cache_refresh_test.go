package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// TestHookConfigCache_StaleResolverDoesNotBlock asserts the request path
// never waits on a TTL refresh: with a slow loader and an expired TTL,
// Resolver must return the current snapshot immediately instead of
// blocking behind the database load.
func TestHookConfigCache_StaleResolverDoesNotBlock(t *testing.T) {
	var calls atomic.Int32
	loader := func(ctx context.Context) ([]core.HookConfig, error) {
		if calls.Add(1) > 1 { // every call after the initial Start load is slow
			time.Sleep(200 * time.Millisecond)
		}
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := NewHookConfigCache(loader, builtins.Registry, 5*time.Millisecond, slog.Default())
	_ = cache.Start(ctx)
	time.Sleep(10 * time.Millisecond) // let the TTL expire

	start := time.Now()
	resolver := cache.Resolver(context.Background())
	elapsed := time.Since(start)

	if resolver == nil {
		t.Fatal("resolver nil")
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("Resolver blocked %v on a stale refresh; the request path must serve the current snapshot immediately", elapsed)
	}
	// The background refresh must still happen.
	if !waitFor(t, time.Second, func() bool { return calls.Load() >= 2 }) {
		t.Fatalf("expected a background TTL refresh, loader calls=%d", calls.Load())
	}
}

// TestHookConfigCache_StaleStampedeSingleFlight asserts that concurrent
// requests arriving while the snapshot is stale trigger AT MOST ONE
// reload — not one per request. The request path never loads; the serial
// backstop ticker holds the only in-flight load.
func TestHookConfigCache_StaleStampedeSingleFlight(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	loader := func(ctx context.Context) ([]core.HookConfig, error) {
		if calls.Add(1) > 1 { // calls after Start block until released
			<-release
		}
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := NewHookConfigCache(loader, builtins.Registry, 5*time.Millisecond, slog.Default())
	_ = cache.Start(ctx)
	time.Sleep(10 * time.Millisecond) // expire the TTL

	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cache.Resolver(context.Background())
		}()
	}
	wg.Wait()

	// All 50 concurrent requests must have triggered zero loads of their own;
	// only the backstop ticker's single in-flight load may have run.
	if got := calls.Load(); got > 2 {
		t.Fatalf("stampede: %d loader calls for %d concurrent stale requests (want <= 2 incl. Start)", got, n)
	}
	close(release)
}

// TestHookConfigCache_AsyncRefreshAppliesNewConfig asserts a background
// TTL refresh actually lands: after the refresh completes, the resolver
// serves the newly loaded config.
func TestHookConfigCache_AsyncRefreshAppliesNewConfig(t *testing.T) {
	var calls atomic.Int32
	loader := func(ctx context.Context) ([]core.HookConfig, error) {
		if calls.Add(1) == 1 {
			return nil, nil // initial: no hooks
		}
		return []core.HookConfig{
			{ID: "h1", ImplementationID: "keyword-filter", Name: "kw", Enabled: true, Stage: "request",
				Config: map[string]any{"patterns": []any{map[string]any{"pattern": "bad", "category": "test", "severity": "hard"}}}},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := NewHookConfigCache(loader, builtins.Registry, 5*time.Millisecond, slog.Default())
	_ = cache.Start(ctx)
	if cache.Resolver(context.Background()).HasHooks("request") {
		t.Fatal("precondition: no hooks after initial empty load")
	}

	time.Sleep(10 * time.Millisecond) // expire TTL
	_ = cache.Resolver(context.Background())

	if !waitFor(t, time.Second, func() bool {
		return cache.Resolver(context.Background()).HasHooks("request")
	}) {
		t.Fatal("background refresh did not apply the new config")
	}
}

// TestHookConfigCache_FailedRefreshRetriesNextStale asserts a failed
// backstop refresh does not wedge the loop: the next tick tries again
// and the previous snapshot stays active in between.
func TestHookConfigCache_FailedRefreshRetriesNextStale(t *testing.T) {
	var calls atomic.Int32
	loader := func(ctx context.Context) ([]core.HookConfig, error) {
		n := calls.Add(1)
		if n == 2 {
			return nil, errors.New("db down")
		}
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := NewHookConfigCache(loader, builtins.Registry, 5*time.Millisecond, slog.Default())
	_ = cache.Start(ctx)

	// Tick #1 fails (call 2). The loop must survive it…
	if !waitFor(t, time.Second, func() bool { return calls.Load() >= 2 }) {
		t.Fatalf("expected failing refresh attempt, calls=%d", calls.Load())
	}
	// …and tick #2 must retry (call 3).
	if !waitFor(t, time.Second, func() bool { return calls.Load() >= 3 }) {
		t.Fatalf("failed refresh wedged the backstop loop; calls=%d", calls.Load())
	}
}

// TestHookConfigCache_PushModeNeverAutoReloads asserts ttl=0 (Agent push
// mode) never triggers a load from the request path.
func TestHookConfigCache_PushModeNeverAutoReloads(t *testing.T) {
	var calls atomic.Int32
	loader := func(ctx context.Context) ([]core.HookConfig, error) {
		calls.Add(1)
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	_ = cache.Start(ctx)
	time.Sleep(10 * time.Millisecond)

	for range 10 {
		_ = cache.Resolver(context.Background())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("push mode must not auto-reload; loader calls=%d (want 1)", got)
	}
}

// TestHookConfigCache_ReloadNilContext_ErrorsNotPanics pins that a nil-context
// Reload fails gracefully (as the agent shadow-apply path relies on to keep the
// prior policy) instead of panicking inside context.WithTimeout.
func TestHookConfigCache_ReloadNilContext_ErrorsNotPanics(t *testing.T) {
	loader := func(ctx context.Context) ([]core.HookConfig, error) { return nil, nil }
	cache := NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())

	var nilCtx context.Context
	err := cache.Reload(nilCtx)
	if err == nil {
		t.Fatal("nil-context Reload must return an error, not succeed")
	}
	if !strings.Contains(err.Error(), "nil context") {
		t.Fatalf("error must identify the nil context; got %q", err.Error())
	}
}
