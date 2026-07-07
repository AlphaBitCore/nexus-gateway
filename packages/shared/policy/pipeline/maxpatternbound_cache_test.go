package pipeline

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// TestMaxPatternBoundFor_CachePerGen pins the resolver memo: within one generation a
// repeat call is a cache hit, and a generation bump drops the previous generation's
// entries and recomputes — so a config reload can never serve a stale (too-small) bound.
func TestMaxPatternBoundFor_CachePerGen(t *testing.T) {
	r := NewPolicyResolver(nil, core.NewHookRegistry(), nil)
	hooks := []boundHook{bh(newPrescanHook(t, `[0-9]{16}`), "h1")}
	want, wantU := computeMaxPatternBound(hooks)
	if want == 0 {
		t.Fatal("setup: expected a bounded pattern (>0)")
	}

	// First call (gen 0): computes + caches under gen 0.
	if b, u := r.maxPatternBoundFor(hooks, 0); b != want || u != wantU {
		t.Fatalf("gen0: got (%d,%v) want (%d,%v)", b, u, want, wantU)
	}
	r.boundMu.Lock()
	n, g := len(r.boundByKey), r.boundGen
	r.boundMu.Unlock()
	if n != 1 || g != 0 {
		t.Fatalf("gen0 cache: len=%d gen=%d, want 1 / 0", n, g)
	}

	// Same gen: cache hit, identical values.
	if b, u := r.maxPatternBoundFor(hooks, 0); b != want || u != wantU {
		t.Fatalf("gen0 hit: got (%d,%v)", b, u)
	}

	// Generation bump: the gen-0 map is dropped, recomputed under gen 1.
	if b, _ := r.maxPatternBoundFor(hooks, 1); b != want {
		t.Fatalf("gen1: got %d want %d", b, want)
	}
	r.boundMu.Lock()
	g = r.boundGen
	r.boundMu.Unlock()
	if g != 1 {
		t.Fatalf("boundGen did not advance to 1 on a gen bump: %d", g)
	}
}

// TestMaxPatternBoundFor_AmbiguousKeyBypassesCache: a duplicate config ID makes the
// cache key ambiguous, so the bound is computed directly (correct value) and NOT cached
// — soundness over the optimisation, mirroring the union memo's fallback.
func TestMaxPatternBoundFor_AmbiguousKeyBypassesCache(t *testing.T) {
	r := NewPolicyResolver(nil, core.NewHookRegistry(), nil)
	dup := []boundHook{bh(newPrescanHook(t, `[0-9]{4}`), "x"), bh(newPrescanHook(t, `[a-f]{2}`), "x")}
	want, _ := computeMaxPatternBound(dup)
	if b, _ := r.maxPatternBoundFor(dup, 0); b != want {
		t.Fatalf("ambiguous-key direct compute: got %d want %d", b, want)
	}
	r.boundMu.Lock()
	n := len(r.boundByKey)
	r.boundMu.Unlock()
	if n != 0 {
		t.Fatalf("ambiguous key must not populate the cache, got %d entries", n)
	}
}

// TestMaxPatternBound_StampedVsLazy: a BuildPipeline-stamped pipeline returns the cached
// fields (O(1) hot path); a NewPipeline-built one computes lazily over its hooks.
func TestMaxPatternBound_StampedVsLazy(t *testing.T) {
	stamped := &Pipeline{boundComputed: true, maxBounded: 1234, anyUnbounded: true}
	if b, u := stamped.MaxPatternBound(); b != 1234 || !u {
		t.Fatalf("stamped: got (%d,%v) want (1234,true)", b, u)
	}
	lazy := newTestPipeline([]boundHook{bh(newPrescanHook(t, `[0-9]{8}`), "h")})
	if b, _ := lazy.MaxPatternBound(); b == 0 {
		t.Fatal("lazy path should compute a bounded value (>0)")
	}
}

// TestBuildPipeline_StampsBoundResponseStageOnly: the response-stage build pre-stamps the
// cached bound (gen threaded from the same atomic snapshot) and populates the cache; a
// request-stage build does NOT stamp (only Model-A streaming on the response stage reads
// the bound), so it stays lazy.
func TestBuildPipeline_StampsBoundResponseStageOnly(t *testing.T) {
	reg := core.NewHookRegistry()
	reg.Register("prescan-impl", func(_ *core.HookConfig) (core.Hook, error) {
		return newPrescanHook(t, `[0-9]{16}`), nil
	})
	mkCfg := func(stage string) core.HookConfig {
		return core.HookConfig{ID: "h-" + stage, ImplementationID: "prescan-impl", Name: "h", Enabled: true, Stage: stage, ApplicableIngress: []string{"ALL"}}
	}
	r := NewPolicyResolver([]core.HookConfig{mkCfg("response"), mkCfg("request")}, reg, testLogger())

	resp, err := r.BuildPipeline("response", "AI_GATEWAY", "", nil, time.Second, 5*time.Second, false, true, testLogger())
	if err != nil || resp == nil {
		t.Fatalf("response BuildPipeline: err=%v nil=%v", err, resp == nil)
	}
	if !resp.boundComputed {
		t.Fatal("response-stage build must pre-stamp the bound")
	}
	if b, _ := resp.MaxPatternBound(); b != mustBound(t, resp.hooks) {
		t.Fatalf("stamped bound %d != computed %d", b, mustBound(t, resp.hooks))
	}
	r.boundMu.Lock()
	cached := len(r.boundByKey)
	r.boundMu.Unlock()
	if cached == 0 {
		t.Fatal("response build should populate the bound cache")
	}

	req, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil, time.Second, 5*time.Second, false, true, testLogger())
	if err != nil || req == nil {
		t.Fatalf("request BuildPipeline: err=%v nil=%v", err, req == nil)
	}
	if req.boundComputed {
		t.Fatal("request-stage build must NOT pre-stamp the bound (only response consumes it)")
	}
}

func mustBound(t *testing.T, hooks []boundHook) int {
	t.Helper()
	b, _ := computeMaxPatternBound(hooks)
	return b
}

// TestMaxPatternBoundFor_ConcurrentGenBumpsAlwaysCorrect hammers maxPatternBoundFor with
// monotonically advancing generations from many goroutines (mirroring concurrent
// BuildPipeline calls racing config reloads). Run under -race, it exercises the
// reset-on-read + off-lock-compute + store-guard (r.boundGen == gen) interleavings and
// asserts every call returns the value its own hook set computes — a stale-gen result can
// never be served. The bound is a pure function of the hooks, so correctness is gen-independent.
func TestMaxPatternBoundFor_ConcurrentGenBumpsAlwaysCorrect(t *testing.T) {
	r := NewPolicyResolver(nil, core.NewHookRegistry(), nil)
	hooks := []boundHook{bh(newPrescanHook(t, `[0-9]{16}`), "h1")}
	want, wantU := computeMaxPatternBound(hooks)

	var gen atomic.Uint64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				g := gen.Load()
				if i%50 == 0 {
					g = gen.Add(1) // some goroutines advance the generation
				}
				if b, u := r.maxPatternBoundFor(hooks, g); b != want || u != wantU {
					t.Errorf("concurrent maxPatternBoundFor returned (%d,%v), want (%d,%v)", b, u, want, wantU)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestConfigSnapshot_GenLockstepWithSwapGen guards the bundle invariant: after a Swap the
// atomically-published configSnapshot.gen equals the monotonic swapGen counter. A future
// edit that bumps one without the other would mis-tag cache entries; this pins it.
func TestConfigSnapshot_GenLockstepWithSwapGen(t *testing.T) {
	r := NewPolicyResolver(nil, core.NewHookRegistry(), nil)
	if g, _ := r.loadSnapshot(); g != r.swapGen.Load() {
		t.Fatalf("initial: configSnapshot.gen=%d != swapGen=%d", g, r.swapGen.Load())
	}
	for i := range 3 {
		r.Swap([]core.HookConfig{{ID: "h", ImplementationID: "x", Name: "h", Enabled: true, Stage: "response"}})
		g, _ := r.loadSnapshot()
		if sg := r.swapGen.Load(); g != sg {
			t.Fatalf("after Swap %d: configSnapshot.gen=%d != swapGen=%d", i, g, sg)
		}
	}
}
