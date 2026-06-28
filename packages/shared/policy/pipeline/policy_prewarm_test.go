package pipeline

// Prewarm builds the (potentially expensive) hooks off the request path so the
// first request after startup / a config change does not pay the factory's
// compile cost. These tests pin: enabled request/response hooks are built
// eagerly, connection-stage hooks are skipped, prewarm is idempotent, and a
// concurrent swap makes prewarm abort rather than cache a stale hook.

import (
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

func prewarmFactory(builds *atomic.Int32) core.HookFactory {
	return func(_ *core.HookConfig) (core.Hook, error) {
		builds.Add(1)
		return &stubHook{decision: core.Approve}, nil
	}
}

func reqCfg(id string) core.HookConfig {
	return core.HookConfig{
		ID: id, ImplementationID: "cf", Name: id, Enabled: true, Stage: "request",
		FailBehavior: "fail-open", TimeoutMs: 1000, ApplicableIngress: []string{"ALL"},
	}
}

func newCountingResolver(t *testing.T, builds *atomic.Int32, cfgs ...core.HookConfig) *PolicyResolver {
	t.Helper()
	registry := core.NewHookRegistry()
	registry.Register("cf", prewarmFactory(builds))
	registry.Freeze()
	return NewPolicyResolver(cfgs, registry, testLogger())
}

func TestPolicyResolver_Prewarm_BuildsEnabledHooks(t *testing.T) {
	var builds atomic.Int32
	r := newCountingResolver(t, &builds, reqCfg("a"), reqCfg("b"))

	r.Prewarm()
	if builds.Load() != 2 {
		t.Fatalf("prewarm should build both enabled hooks, got %d builds", builds.Load())
	}
	// resolve() must now hit the cache — no further factory calls.
	if _, err := r.ResolveHooks("request", "COMPLIANCE_PROXY", false); err != nil {
		t.Fatalf("ResolveHooks: %v", err)
	}
	if builds.Load() != 2 {
		t.Fatalf("resolve after prewarm must hit cache, got %d builds", builds.Load())
	}
}

func TestPolicyResolver_Prewarm_SkipsDisabledAndConnection(t *testing.T) {
	var builds atomic.Int32
	disabled := reqCfg("off")
	disabled.Enabled = false
	conn := reqCfg("conn")
	conn.Stage = "connection"
	r := newCountingResolver(t, &builds, reqCfg("on"), disabled, conn)

	r.Prewarm()
	if builds.Load() != 1 {
		t.Fatalf("prewarm should build only the enabled request hook, got %d builds", builds.Load())
	}
}

func TestPolicyResolver_Prewarm_Idempotent(t *testing.T) {
	var builds atomic.Int32
	r := newCountingResolver(t, &builds, reqCfg("a"))
	r.Prewarm()
	r.Prewarm()
	if builds.Load() != 1 {
		t.Fatalf("second prewarm must hit cache, got %d builds", builds.Load())
	}
}

func TestPolicyResolver_Prewarm_EmptySnapshotBuildsNothing(t *testing.T) {
	var builds atomic.Int32
	r := newCountingResolver(t, &builds, reqCfg("a"))
	r.Swap(nil) // snapshot now empty
	r.Prewarm()
	if builds.Load() != 0 {
		t.Fatalf("prewarm over an empty snapshot must build nothing, got %d", builds.Load())
	}
	if r.HasHooks("request") {
		t.Fatalf("no hooks should be present after swap-to-empty")
	}
}

func TestPolicyResolver_Prewarm_AbortsWhenSwapRacesBuild(t *testing.T) {
	// A Swap landing during a prewarm build (lock-free) advances swapGen; the
	// built hook must then be discarded (and Closed to free its resources)
	// instead of cached stale.
	var builds, closes atomic.Int32
	var r *PolicyResolver
	registry := core.NewHookRegistry()
	registry.Register("cf", func(_ *core.HookConfig) (core.Hook, error) {
		builds.Add(1)
		// Simulate a config swap arriving mid-build.
		r.Swap([]core.HookConfig{reqCfg("a")})
		return &closeableStubHook{stubHook: stubHook{decision: core.Approve}, closed: &closes}, nil
	})
	registry.Freeze()
	r = NewPolicyResolver([]core.HookConfig{reqCfg("a")}, registry, testLogger())

	r.Prewarm()

	if builds.Load() < 1 {
		t.Fatalf("expected the factory to run")
	}
	if closes.Load() < 1 {
		t.Fatalf("a hook discarded due to a racing swap must be Closed, got %d closes", closes.Load())
	}
	// The racing-aborted prewarm cached nothing; resolve() rebuilds cleanly.
	r.hookMu.RLock()
	cached := len(r.hookCache)
	r.hookMu.RUnlock()
	if cached != 0 {
		t.Fatalf("aborted prewarm must not cache a stale hook, found %d cached", cached)
	}
}
