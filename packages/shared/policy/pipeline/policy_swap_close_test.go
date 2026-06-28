package pipeline

// Close-on-evict: when Swap drops a built hook (config changed or removed), any
// hook that owns native resources (the Vectorscan matcher's compiled database)
// must have Close called so cgo memory is freed — GC alone never reclaims it.
// Retained (unchanged) hooks must NOT be closed.

import (
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

type closeableStubHook struct {
	stubHook
	closed *atomic.Int32
}

func (h *closeableStubHook) Close() error {
	h.closed.Add(1)
	return nil
}

func closeableFactory(closed *atomic.Int32) core.HookFactory {
	return func(_ *core.HookConfig) (core.Hook, error) {
		return &closeableStubHook{stubHook: stubHook{decision: core.Approve}, closed: closed}, nil
	}
}

func closeableCfg(version int) core.HookConfig {
	return core.HookConfig{
		ID:                "ch",
		ImplementationID:  "ch",
		Name:              "ch",
		Enabled:           true,
		Stage:             "request",
		FailBehavior:      "fail-open",
		TimeoutMs:         1000,
		ApplicableIngress: []string{"ALL"},
		Config:            map[string]any{"v": version},
	}
}

func newCloseableResolver(t *testing.T, closed *atomic.Int32) *PolicyResolver {
	t.Helper()
	registry := core.NewHookRegistry()
	registry.Register("ch", closeableFactory(closed))
	registry.Freeze()
	r := NewPolicyResolver([]core.HookConfig{closeableCfg(1)}, registry, testLogger())
	// Build + cache the hook so there is an instance to evict.
	if _, err := r.ResolveHooks("request", "COMPLIANCE_PROXY", false); err != nil {
		t.Fatalf("ResolveHooks: %v", err)
	}
	return r
}

func TestPolicyResolver_Swap_ClosesChangedHook(t *testing.T) {
	var closed atomic.Int32
	r := newCloseableResolver(t, &closed)
	if closed.Load() != 0 {
		t.Fatalf("hook closed before any swap")
	}
	// Same ID, different content → DeepEqual false → old instance evicted+closed.
	r.Swap([]core.HookConfig{closeableCfg(2)})
	if closed.Load() != 1 {
		t.Fatalf("changed hook must be closed exactly once on swap, got %d", closed.Load())
	}
}

func TestPolicyResolver_Swap_ClosesRemovedHook(t *testing.T) {
	var closed atomic.Int32
	r := newCloseableResolver(t, &closed)
	// Remove the hook entirely → old instance evicted+closed.
	r.Swap(nil)
	if closed.Load() != 1 {
		t.Fatalf("removed hook must be closed once, got %d", closed.Load())
	}
}

func TestPolicyResolver_Resolve_ClosesConnectionIncompatibleHook(t *testing.T) {
	// A hook built at the connection stage that is not ConnectionStageCompatible
	// is dropped by resolve() without caching. Its native resources (a Vectorscan
	// matcher's cgo DB) must still be freed — closeHook on the drop path.
	var closed atomic.Int32
	registry := core.NewHookRegistry()
	registry.Register("ch", closeableFactory(&closed))
	registry.Freeze()
	cfg := core.HookConfig{
		ID: "ch", ImplementationID: "ch", Name: "ch", Enabled: true,
		Stage: "connection", FailBehavior: "fail-open", TimeoutMs: 1000,
		ApplicableIngress: []string{"ALL"},
	}
	r := NewPolicyResolver([]core.HookConfig{cfg}, registry, testLogger())

	hooks, err := r.ResolveHooks("connection", "COMPLIANCE_PROXY", false)
	if err != nil {
		t.Fatalf("ResolveHooks: %v", err)
	}
	if len(hooks) != 0 {
		t.Fatalf("connection-incompatible hook must not be returned, got %d", len(hooks))
	}
	if closed.Load() != 1 {
		t.Fatalf("dropped connection-incompatible hook must be Closed once, got %d", closed.Load())
	}
}

func TestPolicyResolver_Swap_DoesNotCloseUnchangedHook(t *testing.T) {
	var closed atomic.Int32
	r := newCloseableResolver(t, &closed)
	// Identical config → DeepEqual true → instance retained, NOT closed.
	r.Swap([]core.HookConfig{closeableCfg(1)})
	if closed.Load() != 0 {
		t.Fatalf("unchanged hook must be retained, not closed; got %d", closed.Load())
	}
	// And it must still resolve to the SAME cached instance (no rebuild).
	hooks, err := r.ResolveHooks("request", "COMPLIANCE_PROXY", false)
	if err != nil {
		t.Fatalf("ResolveHooks after no-op swap: %v", err)
	}
	if len(hooks) != 1 {
		t.Fatalf("expected 1 resolved hook, got %d", len(hooks))
	}
}
