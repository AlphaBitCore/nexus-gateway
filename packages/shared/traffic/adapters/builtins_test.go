package adapters

import (
	"slices"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestBuiltinTrafficAdapterIDs_SortedAndMatchesRegistry(t *testing.T) {
	t.Parallel()
	ids := BuiltinTrafficAdapterIDs()
	if !slices.IsSorted(ids) {
		t.Errorf("BuiltinTrafficAdapterIDs not sorted: %v", ids)
	}
	reg := traffic.NewAdapterRegistry("test")
	RegisterBuiltins(reg)
	reg.Freeze()
	got := reg.All()
	slices.Sort(got)
	if len(got) != len(ids) {
		t.Fatalf("len mismatch: catalog=%d registry=%d", len(ids), len(got))
	}
	for i := range ids {
		if ids[i] != got[i] {
			t.Errorf("index %d: catalog %q registry %q", i, ids[i], got[i])
		}
	}
}
