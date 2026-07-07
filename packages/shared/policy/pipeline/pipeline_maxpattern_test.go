package pipeline

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// prescanStub is a content hook that exports a fixed PrescanPattern set, so a test can
// drive Pipeline.MaxPatternBound through the PrescanPatternSource path.
type prescanStub struct {
	stubHook
	pats []core.PrescanPattern
}

func (p *prescanStub) ScansContent() bool                     { return true }
func (p *prescanStub) MayMatchRaw([]byte) bool                { return true }
func (p *prescanStub) PrescanPatterns() []core.PrescanPattern { return p.pats }

// scanOnlyStub scans content but does NOT export its patterns (not a
// PrescanPatternSource) — the pipeline must treat it conservatively as best-effort.
type scanOnlyStub struct {
	stubHook
}

func (*scanOnlyStub) ScansContent() bool      { return true }
func (*scanOnlyStub) MayMatchRaw([]byte) bool { return true }

func contentHook(exprs ...string) boundHook {
	pats := make([]core.PrescanPattern, len(exprs))
	for i, e := range exprs {
		pats[i] = core.PrescanPattern{Expr: e}
	}
	return boundHook{hook: &prescanStub{pats: pats}, config: &core.HookConfig{ID: "h", Name: "h"}}
}

// TestPipeline_MaxPatternBound pins the Model-A lookahead derivation: the aggregate
// returns the largest finite bound across content hooks and flags any unbounded pattern
// (best-effort surface). Under-derivation would reopen the streaming boundary leak.
func TestPipeline_MaxPatternBound(t *testing.T) {
	t.Run("short bounded pattern", func(t *testing.T) {
		b, u := newTestPipeline([]boundHook{contentHook(`[0-9]{16}`)}).MaxPatternBound()
		if b == 0 || u {
			t.Fatalf("bound=%d unbounded=%v, want >0 / false", b, u)
		}
	})
	t.Run("long bounded concat exceeds the 4096 floor", func(t *testing.T) {
		// Tight ASCII byte-width = 1/char, so exceeding the 4096 floor needs >4096 chars:
		// five {1000} concats = 5000 bytes. This is the case the derivation exists for.
		b, _ := newTestPipeline([]boundHook{contentHook(`[A-Za-z0-9]{1000}[A-Za-z0-9]{1000}[A-Za-z0-9]{1000}[A-Za-z0-9]{1000}[A-Za-z0-9]{1000}`)}).MaxPatternBound()
		if b <= 4096 {
			t.Fatalf("bound=%d, want > 4096 (the long concat must size the lookahead above the default floor)", b)
		}
	})
	t.Run("max across hooks", func(t *testing.T) {
		b, _ := newTestPipeline([]boundHook{contentHook(`[0-9]{16}`), contentHook(`[A-Za-z]{500}`)}).MaxPatternBound()
		if b < 500 {
			t.Fatalf("bound=%d, want the max across hooks (≥ 500)", b)
		}
	})
	t.Run("unbounded pattern flagged", func(t *testing.T) {
		b, u := newTestPipeline([]boundHook{contentHook(`[a-z]+`)}).MaxPatternBound()
		if !u {
			t.Fatalf("unbounded `+` pattern not flagged (bound=%d)", b)
		}
	})
	t.Run("non-content hook skipped", func(t *testing.T) {
		b, u := newTestPipeline([]boundHook{boundWithAction("meta", "redact")}).MaxPatternBound()
		if b != 0 || u {
			t.Fatalf("non-content hook: bound=%d unbounded=%v, want 0 / false (no patterns to bound)", b, u)
		}
	})
	t.Run("content hook without pattern source flagged best-effort", func(t *testing.T) {
		b, u := newTestPipeline([]boundHook{{hook: &scanOnlyStub{}, config: &core.HookConfig{ID: "x", Name: "x"}}}).MaxPatternBound()
		if !u {
			t.Fatalf("scan-only content hook not flagged unbounded (bound=%d) — it can't be bounded so must be best-effort", b)
		}
	})
	t.Run("nil pattern set flagged best-effort", func(t *testing.T) {
		b, u := newTestPipeline([]boundHook{{hook: &prescanStub{pats: nil}, config: &core.HookConfig{ID: "n", Name: "n"}}}).MaxPatternBound()
		if !u {
			t.Fatalf("nil-pattern content hook not flagged unbounded (bound=%d)", b)
		}
	})
}
