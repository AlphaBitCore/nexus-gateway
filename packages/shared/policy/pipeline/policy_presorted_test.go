package pipeline

// resolveFrom no longer sorts. It relies on the resolver's snapshot being
// ordered by Priority when stored, and on its own append loop preserving that
// order through the stage / ingress filter.
//
// That invariant is invisible at the call site: if a future construction path
// stores an unordered snapshot, resolveFrom keeps returning a pipeline and the
// hooks simply run in the wrong order — a silent behaviour change in which
// hook's Modify the merge sees last. These tests are what make it loud.

import (
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// unorderedConfigs deliberately declares priorities out of order, and gives two
// hooks the same priority so the stable-order promise is exercised too.
func unorderedConfigs() []core.HookConfig {
	mk := func(id string, prio int) core.HookConfig {
		return core.HookConfig{
			ID: id, Name: id, ImplementationID: "noop", Priority: prio,
			Enabled: true, Stage: "request", FailBehavior: "fail-open",
			TimeoutMs: 1000, ApplicableIngress: []string{"ALL"},
		}
	}
	return []core.HookConfig{
		mk("c-30", 30),
		mk("a-10", 10),
		mk("d-20-second", 20),
		mk("b-20-first", 20),
	}
}

func wantOrder() []string {
	// 10, then the two 20s in declaration order, then 30.
	return []string{"a-10", "d-20-second", "b-20-first", "c-30"}
}

func gotOrder(t *testing.T, bound []boundHook) []string {
	t.Helper()
	out := make([]string, 0, len(bound))
	for _, b := range bound {
		out = append(out, b.config.ID)
	}
	return out
}

func equalSeq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The constructor path.
func TestResolveFrom_PreservesPriorityOrderWithoutSorting(t *testing.T) {
	r := NewPolicyResolver(unorderedConfigs(), builtins.Registry, slog.Default())
	bound, _, err := r.resolveFrom(r.snapshot(), "request", "ALL", false)
	if err != nil {
		t.Fatalf("resolveFrom: %v", err)
	}
	if got, want := gotOrder(t, bound), wantOrder(); !equalSeq(got, want) {
		t.Fatalf("resolveFrom returned hooks out of priority order — the snapshot "+
			"it relies on is no longer sorted at store time\n  got:  %v\n  want: %v", got, want)
	}
}

// The reload path. Swap is the one that runs in production after the first
// config change, and it builds its own snapshot.
func TestSwap_StoresPrioritySortedSnapshot(t *testing.T) {
	r := NewPolicyResolver(nil, builtins.Registry, slog.Default())
	r.Swap(unorderedConfigs())
	bound, _, err := r.resolveFrom(r.snapshot(), "request", "ALL", false)
	if err != nil {
		t.Fatalf("resolveFrom: %v", err)
	}
	if got, want := gotOrder(t, bound), wantOrder(); !equalSeq(got, want) {
		t.Fatalf("Swap stored an unsorted snapshot\n  got:  %v\n  want: %v", got, want)
	}
}
