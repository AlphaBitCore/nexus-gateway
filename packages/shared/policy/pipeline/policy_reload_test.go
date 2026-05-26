package pipeline

import (
	"bytes"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// countingFactory wraps stubFactory so tests can count how often the
// factory ran. PolicyResolver's hook cache should make this count stay
// flat across Swap() calls whose content is unchanged.
type countingFactory struct {
	calls atomic.Int32
}

func (c *countingFactory) Factory() core.HookFactory {
	return func(_ *core.HookConfig) (core.Hook, error) {
		c.calls.Add(1)
		return &stubHook{decision: core.Approve}, nil
	}
}

// newResolverWithCounting builds a resolver whose sole registered factory
// is tracked by the returned counter, so tests can assert cache-hit
// behavior across reloads.
func newResolverWithCounting(implID string, configs []core.HookConfig) (*PolicyResolver, *countingFactory) {
	c := &countingFactory{}
	registry := core.NewHookRegistry()
	registry.Register(implID, c.Factory())
	registry.Freeze()
	return NewPolicyResolver(configs, registry, testLogger()), c
}

// TestPolicyResolver_SwapPreservesUnchangedHookInstances verifies that
// Swap() with a new snapshot whose row content is byte-identical to the
// old one does NOT re-instantiate the factory. This is the delta-reducer
// guarantee: reload cost is O(changed rows), not O(N).
func TestPolicyResolver_SwapPreservesUnchangedHookInstances(t *testing.T) {
	configs := []core.HookConfig{
		{ID: "h1", ImplementationID: "h1", Name: "h1", Priority: 0, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
		{ID: "h2", ImplementationID: "h1", Name: "h2", Priority: 1, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}
	r, counter := newResolverWithCounting("h1", configs)

	// First resolve instantiates both hooks (cache miss × 2).
	if _, err := r.ResolveHooks("request", "AI_GATEWAY"); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}
	if got := counter.calls.Load(); got != 2 {
		t.Fatalf("after first resolve: factory calls = %d, want 2", got)
	}

	// Swap with a byte-identical snapshot. Both rows should be preserved
	// in the cache; next resolve must not re-run the factory.
	unchanged := append([]core.HookConfig(nil), configs...)
	r.Swap(unchanged)
	if _, err := r.ResolveHooks("request", "AI_GATEWAY"); err != nil {
		t.Fatalf("post-swap resolve: %v", err)
	}
	if got := counter.calls.Load(); got != 2 {
		t.Errorf("after unchanged Swap: factory calls = %d, want 2 (cache should be preserved)", got)
	}
}

// TestPolicyResolver_SwapEvictsChangedRows verifies that a row whose
// config content has changed (even just a priority bump) evicts its
// cached hook so the factory runs again with the new config.
func TestPolicyResolver_SwapEvictsChangedRows(t *testing.T) {
	configs := []core.HookConfig{
		{ID: "h1", ImplementationID: "h1", Name: "h1", Priority: 0, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
		{ID: "h2", ImplementationID: "h1", Name: "h2", Priority: 1, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}
	r, counter := newResolverWithCounting("h1", configs)

	if _, err := r.ResolveHooks("request", "AI_GATEWAY"); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}
	if got := counter.calls.Load(); got != 2 {
		t.Fatalf("after first resolve: factory calls = %d, want 2", got)
	}

	// Only h2's priority changed. h1 should be preserved, h2 re-built.
	mutated := []core.HookConfig{
		{ID: "h1", ImplementationID: "h1", Name: "h1", Priority: 0, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
		{ID: "h2", ImplementationID: "h1", Name: "h2", Priority: 99, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}
	r.Swap(mutated)
	if _, err := r.ResolveHooks("request", "AI_GATEWAY"); err != nil {
		t.Fatalf("post-swap resolve: %v", err)
	}
	if got := counter.calls.Load(); got != 3 {
		t.Errorf("after mutating-h2 Swap: factory calls = %d, want 3 (h1 preserved, h2 rebuilt)", got)
	}
}

// TestPolicyResolver_SwapEvictsRemovedRows verifies that a row dropped
// from the new snapshot no longer occupies a slot in the hook cache, so
// re-adding an identical row later does not incorrectly resurrect a
// stale cached instance.
func TestPolicyResolver_SwapEvictsRemovedRows(t *testing.T) {
	configs := []core.HookConfig{
		{ID: "h1", ImplementationID: "h1", Name: "h1", Priority: 0, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}
	r, counter := newResolverWithCounting("h1", configs)

	if _, err := r.ResolveHooks("request", "AI_GATEWAY"); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}
	if got := counter.calls.Load(); got != 1 {
		t.Fatalf("after first resolve: factory calls = %d, want 1", got)
	}

	// Row removed entirely.
	r.Swap(nil)

	// Row re-added — must cache-miss and re-instantiate.
	r.Swap(configs)
	if _, err := r.ResolveHooks("request", "AI_GATEWAY"); err != nil {
		t.Fatalf("resolve after re-add: %v", err)
	}
	if got := counter.calls.Load(); got != 2 {
		t.Errorf("after remove+re-add: factory calls = %d, want 2 (second instantiation)", got)
	}
}

func newLogCapture() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	return slog.New(h), buf
}

// TestPolicyResolver_WarnsOncePerUnknownImplPerReload asserts that the
// "unknown hook implementation, skipping" warning fires at most once per
// unique implementationId per reload epoch, no matter how many rows
// reference it or how many resolve() calls happen in between.
func TestPolicyResolver_WarnsOncePerUnknownImplPerReload(t *testing.T) {
	// Three rows share the same unknown impl id. Prior behavior: one warn
	// per row per resolve() (6 warnings across 2 resolves). New behavior:
	// one warn per impl id per reload epoch (2 warnings: one initial load,
	// one after the explicit Swap).
	configs := []core.HookConfig{
		{ID: "h1", ImplementationID: "ghost", Name: "h1", Priority: 0, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
		{ID: "h2", ImplementationID: "ghost", Name: "h2", Priority: 1, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
		{ID: "h3", ImplementationID: "ghost", Name: "h3", Priority: 2, Enabled: true, Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}

	logger, buf := newLogCapture()
	registry := core.NewHookRegistry()
	registry.Freeze() // empty — "ghost" is deliberately unregistered
	r := NewPolicyResolver(configs, registry, logger)

	for i := range 4 {
		if _, err := r.ResolveHooks("request", "AI_GATEWAY"); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	got := strings.Count(buf.String(), "unknown hook implementation")
	if got != 1 {
		t.Errorf("expected 1 warning for unknown impl within one epoch, got %d\nlog:\n%s", got, buf.String())
	}

	// After Swap(), dedup state resets — the unknown impl should warn
	// again exactly once, even though the rows are unchanged content-wise.
	r.Swap(configs)
	for i := range 4 {
		if _, err := r.ResolveHooks("request", "AI_GATEWAY"); err != nil {
			t.Fatalf("post-swap resolve %d: %v", i, err)
		}
	}
	got = strings.Count(buf.String(), "unknown hook implementation")
	if got != 2 {
		t.Errorf("expected 2 warnings across 2 epochs (1 per reload), got %d\nlog:\n%s", got, buf.String())
	}
}
