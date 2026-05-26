package provbuiltins_test

import (
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// TestRegister_CoversAllFormats asserts that [provbuiltins.Register]
// seeds the registry with exactly the nine declared Formats and no
// duplicates. Anything short of this is a wiring regression.
func TestRegister_CoversAllFormats(t *testing.T) {
	reg := provcore.NewRegistry()
	provbuiltins.Register(reg, nil, slog.Default())
	reg.Freeze()

	want := provcore.AllFormats()
	got := reg.List()
	if len(got) != len(want) {
		t.Fatalf("registered %d, want %d", len(got), len(want))
	}
	seen := make(map[provcore.Format]bool, len(got))
	for _, f := range got {
		seen[f] = true
	}
	for _, f := range want {
		if !seen[f] {
			t.Errorf("missing built-in format %s", f)
		}
		adapter, ok := reg.Get(f)
		if !ok || adapter == nil {
			t.Errorf("registry.Get(%q) returned no adapter", f)
			continue
		}
		if adapter.Format() != f {
			t.Errorf("adapter for %q reports Format()=%q", f, adapter.Format())
		}
	}
}
