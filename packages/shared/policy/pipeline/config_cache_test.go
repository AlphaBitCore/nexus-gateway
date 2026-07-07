package pipeline

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

func TestHookConfigCache_StartAndResolve(t *testing.T) {
	loadCount := 0
	loader := func(ctx context.Context) ([]core.HookConfig, error) {
		loadCount++
		return []core.HookConfig{
			{ID: "h1", ImplementationID: "keyword-filter", Name: "test", Enabled: true, Stage: "request",
				Config: map[string]any{"patterns": []any{map[string]any{"pattern": "bad", "category": "test", "severity": "hard"}}}},
		}, nil
	}

	cache := NewHookConfigCache(loader, builtins.Registry, 1*time.Minute, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	if loadCount != 1 {
		t.Fatalf("expected 1 load, got %d", loadCount)
	}

	resolver := cache.Resolver(context.Background())
	if resolver == nil {
		t.Fatal("resolver nil")
	}
	if !resolver.HasHooks("request") {
		t.Fatal("expected request hooks after load")
	}
}

func TestHookConfigCache_TTLRefresh(t *testing.T) {
	var loadCount atomic.Int32
	loader := func(ctx context.Context) ([]core.HookConfig, error) {
		loadCount.Add(1)
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := NewHookConfigCache(loader, builtins.Registry, 10*time.Millisecond, slog.Default())
	_ = cache.Start(ctx)

	// The backstop ticker must refresh on its own — no request traffic needed.
	deadline := time.Now().Add(time.Second)
	for loadCount.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if loadCount.Load() < 2 {
		t.Fatalf("expected TTL backstop refresh, got %d loads", loadCount.Load())
	}
}

func TestHookConfigCache_ForceReload(t *testing.T) {
	loadCount := 0
	loader := func(ctx context.Context) ([]core.HookConfig, error) {
		loadCount++
		return nil, nil
	}

	cache := NewHookConfigCache(loader, builtins.Registry, 10*time.Minute, slog.Default())
	_ = cache.Start(context.Background())
	_ = cache.Reload(context.Background())

	if loadCount != 2 {
		t.Fatalf("expected 2 loads, got %d", loadCount)
	}
}
