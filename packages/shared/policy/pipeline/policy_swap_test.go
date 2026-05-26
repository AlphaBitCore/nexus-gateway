package pipeline

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// Tests in this file verify the concurrent-safe snapshot/Swap behaviour of
// PolicyResolver (DB-backed hook config with Redis pub/sub invalidation). The goal is to catch data races between hot-path
// readers (Resolve*/Has*) and the background Swap triggered after a
// pub/sub invalidation message is received.
//
// Run with `go test -race ./internal/compliance/...` to exercise the race
// detector; the basic correctness assertions below also run under plain
// `go test`.
//
// The existing pipeline_test.go already declares stubHook and testLogger,
// so these tests reuse them without re-declaring.

// stubFactory returns a HookFactory that produces a fresh stubHook for
// every call. The factory signature is what PolicyResolver's registry
// expects.
func stubFactory() core.HookFactory {
	return func(_ *core.HookConfig) (core.Hook, error) {
		return &stubHook{decision: core.Approve}, nil
	}
}

// newResolverWith creates a resolver with the given hook names, each
// registered under an implementationID that matches its name. All hooks
// are enabled at the "request" stage.
func newResolverWith(names ...string) *PolicyResolver {
	configs := make([]core.HookConfig, len(names))
	registry := core.NewHookRegistry()
	for i, n := range names {
		configs[i] = core.HookConfig{
			ID:                n,
			ImplementationID:  n,
			Name:              n,
			Priority:          i,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-open",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
		}
		registry.Register(n, stubFactory())
	}
	registry.Freeze()
	return NewPolicyResolver(configs, registry, testLogger())
}

func TestPolicyResolver_NewDefensivelyCopiesInitialConfigs(t *testing.T) {
	// Mutating the slice passed to NewPolicyResolver after construction
	// must not affect the resolver's internal snapshot.
	configs := []core.HookConfig{
		{ID: "h1", ImplementationID: "h1", Name: "h1", Priority: 0, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}
	registry := core.NewHookRegistry()
	registry.Register("h1", stubFactory())
	registry.Freeze()
	r := NewPolicyResolver(configs, registry, testLogger())

	configs[0].Enabled = false
	configs[0].Name = "mutated-by-caller"

	snapshot := r.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("expected snapshot of 1 entry, got %d", len(snapshot))
	}
	if !snapshot[0].Enabled {
		t.Errorf("caller mutation leaked into resolver snapshot (Enabled)")
	}
	if snapshot[0].Name != "h1" {
		t.Errorf("caller mutation leaked into resolver snapshot (Name): %s", snapshot[0].Name)
	}
}

func TestPolicyResolver_SwapReplacesSnapshot(t *testing.T) {
	r := newResolverWith("a")
	if !r.HasHooks("request") {
		t.Fatalf("expected HasHooks(request)=true before swap")
	}

	r.Swap(nil)

	if r.HasHooks("request") {
		t.Errorf("expected HasHooks(request)=false after swap to empty")
	}

	// Swap back to a non-empty set — the resolver should immediately
	// observe the new snapshot on the next call.
	r.Swap([]core.HookConfig{
		{ID: "b", ImplementationID: "b", Name: "b", Priority: 0, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	})
	if !r.HasHooks("request") {
		t.Errorf("expected HasHooks(request)=true after swap to non-empty")
	}
}

func TestPolicyResolver_SwapDefensivelyCopiesNewConfigs(t *testing.T) {
	r := newResolverWith("a")

	newConfigs := []core.HookConfig{
		{ID: "x", ImplementationID: "x", Name: "x", Priority: 0, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}
	r.Swap(newConfigs)

	// Mutate the slice the caller owns. The resolver snapshot must not
	// see the mutation.
	newConfigs[0].Enabled = false
	newConfigs[0].Name = "mutated-after-swap"

	snap := r.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected snapshot of 1 entry, got %d", len(snap))
	}
	if !snap[0].Enabled {
		t.Errorf("post-Swap caller mutation leaked into resolver snapshot (Enabled)")
	}
	if snap[0].Name != "x" {
		t.Errorf("post-Swap caller mutation leaked into resolver snapshot (Name): %s", snap[0].Name)
	}
}

func TestPolicyResolver_ConcurrentReadsAndSwaps(t *testing.T) {
	// This is the race test: many goroutines read (HasHooks and
	// ResolveHooks) while another goroutine calls Swap in a loop.
	// Run with `go test -race` to prove there is no data race between
	// the atomic.Pointer load and store.
	r := newResolverWith("a", "b", "c")

	const readers = 8
	const readsPerReader = 2000
	const swaps = 500

	var wg sync.WaitGroup
	var readErrors atomic.Int64

	// Reader goroutines.
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range readsPerReader {
				_ = r.HasHooks("request")
				hooks, err := r.ResolveHooks("request", "COMPLIANCE_PROXY")
				if err != nil {
					readErrors.Add(1)
					return
				}
				// The returned slice is valid for the duration of this
				// iteration regardless of concurrent Swaps — we only
				// check that the length falls in the plausible range.
				if len(hooks) > 3 {
					readErrors.Add(1)
					return
				}
			}
		}()
	}

	// Writer goroutine: alternates between an empty snapshot, a 1-hook
	// snapshot, and a 3-hook snapshot so readers see churn.
	wg.Add(1)
	go func() {
		defer wg.Done()
		variants := [][]core.HookConfig{
			nil,
			{
				{ID: "only-a", ImplementationID: "a", Name: "a", Priority: 0, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
			},
			{
				{ID: "a", ImplementationID: "a", Name: "a", Priority: 0, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
				{ID: "b", ImplementationID: "b", Name: "b", Priority: 1, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
				{ID: "c", ImplementationID: "c", Name: "c", Priority: 2, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
			},
		}
		for i := range swaps {
			r.Swap(variants[i%len(variants)])
		}
	}()

	wg.Wait()

	if n := readErrors.Load(); n > 0 {
		t.Fatalf("concurrent readers saw %d unexpected errors / out-of-range results", n)
	}
}
