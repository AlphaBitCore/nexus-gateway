package wiring

import (
	"context"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
)

// TestShadowProbeAdapter_ZeroTimeReturnsZeroAge verifies the nil-last-report
// branch of LastReportAge.
func TestShadowProbeAdapter_ZeroTimeReturnsZeroAge(t *testing.T) {
	tc := newTestThingClient(t)
	adapter := &shadowProbeAdapter{client: tc}
	if age := adapter.LastReportAge(); age != 0 {
		t.Errorf("got %v; want 0 for zero last-reported time", age)
	}
}

// TestInitHealthHandler_IntrospectSources_AllSourcesInvokable calls
// introspectReg.Snapshot to invoke every registered source function
// (covering health.go closures at lines 88-91, 104-106, 116-119).
func TestInitHealthHandler_IntrospectSources_AllSourcesInvokable(t *testing.T) {
	d := buildHealthDeps(t)

	// Use a non-nil HookConfigCache to cover the HookConfigCache != nil branch
	// (lines 101-106). A minimal pipeline.HookConfigCache suffices.
	d.HookConfigCache = buildTestHookConfigCache(t)

	_, introspectReg := InitHealthHandler(d)

	// Snapshot invokes all registered Fn closures concurrently.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp := introspectReg.Snapshot(ctx)
	// We don't assert on specific values; the test passes if none of the
	// source functions panic. Results may have errors (unregistered loaders etc.)
	// which is expected.
	if resp.Sources == nil {
		t.Error("expected non-nil sources in introspect response")
	}
}

// buildTestHookConfigCache creates a minimal pipeline.HookConfigCache that
// can be used in health tests. It has no DB loader — Snapshot returns empty.
func buildTestHookConfigCache(t *testing.T) *pipeline.HookConfigCache {
	t.Helper()
	hcc := pipeline.NewHookConfigCache(
		func(_ context.Context) ([]core.HookConfig, error) {
			return nil, nil
		},
		builtins.Registry,
		2*time.Minute,
		testLogger(),
	)
	return hcc
}

// TestInitHealthHandler_IntrospectSources_CacheLoadersRegistered registers
// working loaders on the cache manager so the success branches of
// cache.allowlists (lines 88-91) and cache.observability (lines 116-119)
// are exercised when introspectReg.Snapshot is called.
func TestInitHealthHandler_IntrospectSources_CacheLoadersRegistered(t *testing.T) {
	d := buildHealthDeps(t)
	d.HookConfigCache = buildTestHookConfigCache(t)

	// Register no-op loaders so Get succeeds instead of returning "no loader" error.
	d.CacheManager.RegisterLoader(cache.CategoryAllowlists, func(_ context.Context) (interface{}, error) {
		return []string{"example.com"}, nil
	})
	d.CacheManager.RegisterLoader(cache.CategoryObservability, func(_ context.Context) (interface{}, error) {
		return map[string]any{"logging": "verbose"}, nil
	})

	_, introspectReg := InitHealthHandler(d)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp := introspectReg.Snapshot(ctx)
	if resp.Sources == nil {
		t.Error("expected non-nil sources")
	}
}
