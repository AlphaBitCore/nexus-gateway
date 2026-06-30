package pipeline

import (
	"context"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

// --- test hooks ---------------------------------------------------------------

// prescanHook is a content-scanning hook backed by a real matcher over its
// anchor-stripped patterns — a faithful stand-in for the production
// contentPrescan so the union==OR differential is meaningful.
type prescanHook struct {
	core.ChatOnly
	m        matcher.Matcher
	stripped []core.PrescanPattern
}

func newPrescanHook(t *testing.T, exprs ...string) *prescanHook {
	t.Helper()
	var pats []matcher.Pattern
	var exported []core.PrescanPattern
	for i, e := range exprs {
		s, err := matcher.StripAnchors(e)
		if err != nil {
			t.Fatalf("strip %q: %v", e, err)
		}
		pats = append(pats, matcher.Pattern{ID: i, Expr: s})
		exported = append(exported, core.PrescanPattern{Expr: s})
	}
	m, bad := matcher.CompileDefault(pats)
	if len(bad) > 0 {
		t.Fatalf("compile: %v", bad)
	}
	return &prescanHook{m: m, stripped: exported}
}

func (h *prescanHook) Execute(context.Context, *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Approve}, nil
}
func (h *prescanHook) ScansContent() bool { return true }
func (h *prescanHook) MayMatchRaw(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	return len(h.m.Scan([]string{string(body)}, true)) > 0
}
func (h *prescanHook) PrescanPatterns() []core.PrescanPattern { return h.stripped }

// metaHook is content-independent (rate limit / IP / size): never forces
// extraction and contributes nothing to the union.
type metaHook struct{ core.AnyEndpointAnyModality }

func (metaHook) Execute(context.Context, *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Approve}, nil
}
func (metaHook) ScansContent() bool      { return false }
func (metaHook) MayMatchRaw([]byte) bool { return false }

// unaccountedHook does not implement RawContentPrescanner at all → the per-hook
// loop forces "may match", so the union must NOT be used.
type unaccountedHook struct{ core.AnyEndpointAnyModality }

func (unaccountedHook) Execute(context.Context, *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Approve}, nil
}

// nilPatternsHook is content-scanning but could not build a prefilter, so it
// forces "may match" and opts out of the union.
type nilPatternsHook struct{ core.ChatOnly }

func (nilPatternsHook) Execute(context.Context, *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Approve}, nil
}
func (nilPatternsHook) ScansContent() bool                     { return true }
func (nilPatternsHook) MayMatchRaw([]byte) bool                { return true }
func (nilPatternsHook) PrescanPatterns() []core.PrescanPattern { return nil }

func bh(h core.Hook, id string) boundHook {
	return boundHook{hook: h, config: &core.HookConfig{ID: id}}
}

// perHookOR mirrors the original MayMatchRawContent loop semantics.
func perHookOR(hooks []boundHook, body []byte) bool {
	for i := range hooks {
		pre, ok := hooks[i].hook.(core.RawContentPrescanner)
		if !ok {
			return true
		}
		if pre.ScansContent() && pre.MayMatchRaw(body) {
			return true
		}
	}
	return false
}

// --- tests --------------------------------------------------------------------

// The load-bearing invariant: when a union is built, scanning it once yields the
// SAME boolean as OR-ing every hook's MayMatchRaw, across a corpus.
func TestUnion_EquivalentToPerHookOR(t *testing.T) {
	hooks := []boundHook{
		bh(newPrescanHook(t, `AKIA[A-Z0-9]{16}`, `\bconfidential\b`), "kw"),
		bh(newPrescanHook(t, `[a-z]+@[a-z]+\.[a-z]{2,}`), "pii"),
		bh(newPrescanHook(t, `(?i)how to (murder|kill)`), "cs"),
		bh(metaHook{}, "rate"), // ignored by both paths
	}
	union, ok := buildUnionPrescan(hooks)
	if !ok || union == nil {
		t.Fatal("expected a sound union for an all-content-prescanner set")
	}
	p := &Pipeline{hooks: hooks, unionPrescan: union}
	pLoop := &Pipeline{hooks: hooks} // unionPrescan nil → per-hook loop

	corpus := []string{
		"",
		"the weather is nice and the report looks fine",
		"contact alice@example.com about it",
		"key AKIAIOSFODNN7EXAMPLE leaked",
		"this is confidential do not share",
		"how to murder a competitor's pricing",
		"benign text with no triggers whatsoever 12345",
		"mixed: email bob@corp.io and AKIAABCDEFGHIJKLMNOP and confidential",
	}
	for _, s := range corpus {
		body := []byte(s)
		want := perHookOR(hooks, body)
		if got := p.MayMatchRawContent(body); got != want {
			t.Errorf("union MayMatchRawContent(%q)=%v, want %v (OR of per-hook)", s, got, want)
		}
		if got := pLoop.MayMatchRawContent(body); got != want {
			t.Errorf("loop MayMatchRawContent(%q)=%v, want %v", s, got, want)
		}
	}
}

func TestUnion_FallbackCases(t *testing.T) {
	cases := []struct {
		name  string
		hooks []boundHook
	}{
		{"unaccounted hook forces fallback", []boundHook{
			bh(newPrescanHook(t, `secret`), "a"),
			bh(unaccountedHook{}, "u"),
		}},
		{"nil-patterns content hook forces fallback", []boundHook{
			bh(newPrescanHook(t, `secret`), "a"),
			bh(nilPatternsHook{}, "n"),
		}},
		{"no content hook at all", []boundHook{
			bh(metaHook{}, "rate"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := buildUnionPrescan(tc.hooks)
			if ok || m != nil {
				t.Errorf("expected no union (ok=false), got ok=%v m=%v", ok, m)
			}
		})
	}
}

// Cache: same set returns the same matcher within a generation; a Swap bumps the
// generation and rebuilds (the old entry is gone).
func TestUnion_CacheAndGeneration(t *testing.T) {
	r := NewPolicyResolver(nil, core.NewHookRegistry(), nil)
	hooks := []boundHook{bh(newPrescanHook(t, `secret`, `\btoken\b`), "a")}

	m1 := r.unionPrescanFor(hooks, r.swapGen.Load())
	if m1 == nil {
		t.Fatal("expected a union matcher")
	}
	m2 := r.unionPrescanFor(hooks, r.swapGen.Load())
	if m1 != m2 {
		t.Error("expected the cached matcher on the second call within one generation")
	}

	prev := r.swapGen.Add(1) - 1 // simulate a Swap (increment, capture pre-gen)
	r.closeUnionsIfGen(prev)     // Swap closes only the previous generation's unions
	m3 := r.unionPrescanFor(hooks, r.swapGen.Load())
	if m3 == nil {
		t.Fatal("expected a rebuilt union after generation bump")
	}
	if m3 == m1 {
		t.Error("expected a freshly built matcher after a generation change")
	}
}

// Hardening: an empty or duplicate hook ID makes the cache key ambiguous, so the
// resolver must fall back to the per-hook loop (return nil) rather than risk
// serving one set's union to a different set.
func TestUnion_EmptyOrDupIDFallsBack(t *testing.T) {
	r := NewPolicyResolver(nil, core.NewHookRegistry(), nil)

	emptyID := []boundHook{{hook: newPrescanHook(t, `secret`), config: &core.HookConfig{ID: ""}}}
	if m := r.unionPrescanFor(emptyID, r.swapGen.Load()); m != nil {
		t.Error("empty hook ID must fall back to the per-hook loop (nil union)")
	}
	nilCfg := []boundHook{{hook: newPrescanHook(t, `secret`), config: nil}}
	if m := r.unionPrescanFor(nilCfg, r.swapGen.Load()); m != nil {
		t.Error("nil hook config must fall back (nil union)")
	}
	dupID := []boundHook{bh(newPrescanHook(t, `a`), "x"), bh(newPrescanHook(t, `b`), "x")}
	if m := r.unionPrescanFor(dupID, r.swapGen.Load()); m != nil {
		t.Error("duplicate hook ID must fall back (nil union)")
	}
	// Sanity: a clean, unique-ID set still unions.
	ok := []boundHook{bh(newPrescanHook(t, `a`), "x"), bh(newPrescanHook(t, `b`), "y")}
	if m := r.unionPrescanFor(ok, r.swapGen.Load()); m == nil {
		t.Error("unique non-empty IDs should produce a union")
	}
}

// Reproduces the config-reload race that the load test surfaced: concurrent
// unionPrescanFor calls interleaved with Swaps (which increment swapGen then
// close the previous generation's unions). With the unconditional close this
// panicked with "assignment to entry in nil map" when a new-generation builder's
// post-build re-check passed but Swap had nil'd the map. Run under -race.
func TestUnion_ConcurrentSwapNoPanic(t *testing.T) {
	r := NewPolicyResolver(nil, core.NewHookRegistry(), nil)
	mk := func(id string) []boundHook {
		return []boundHook{bh(newPrescanHook(t, `secret`, `\btoken\b`, `AKIA[A-Z0-9]{16}`), id)}
	}

	stop := make(chan struct{})
	var swappers, builders sync.WaitGroup

	// Swappers: hammer generation changes (mirrors Swap: Add then gen-scoped close).
	for range 3 {
		swappers.Add(1)
		go func() {
			defer swappers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					prev := r.swapGen.Add(1) - 1
					r.closeUnionsIfGen(prev)
				}
			}
		}()
	}
	// Builders: hammer unionPrescanFor (and Scan the result) across a few sets.
	for b := range 8 {
		builders.Add(1)
		id := []string{"a", "b", "c"}[b%3]
		go func(id string) {
			defer builders.Done()
			hooks := mk(id)
			for range 5000 {
				if m := r.unionPrescanFor(hooks, r.swapGen.Load()); m != nil {
					_ = m.Scan([]string{"benign body without any token here"}, true)
				}
			}
		}(id)
	}
	builders.Wait()
	close(stop)
	swappers.Wait()
}

func TestUnion_SignatureStableRegardlessOfOrder(t *testing.T) {
	a := []boundHook{bh(metaHook{}, "x"), bh(metaHook{}, "y")}
	b := []boundHook{bh(metaHook{}, "y"), bh(metaHook{}, "x")}
	if hookSetSignature(a) != hookSetSignature(b) {
		t.Error("signature must be order-independent")
	}
	c := []boundHook{bh(metaHook{}, "x")}
	if hookSetSignature(a) == hookSetSignature(c) {
		t.Error("different hook sets must have different signatures")
	}
}

// SwapIfContentChanged is a true no-op when content is unchanged (no generation
// bump → hook matchers + union prefilters are not recompiled) and swaps on a real
// change. This is what makes the 2-minute TTL backstop refresh harmless, while
// Swap itself stays unconditional (one call == one epoch).
func TestSwapIfContentChanged_NoOpOnUnchangedContent(t *testing.T) {
	reg := core.NewHookRegistry()
	cfgs := []core.HookConfig{
		{ID: "a", ImplementationID: "noop", Stage: "request", Enabled: true},
		{ID: "b", ImplementationID: "noop", Stage: "response", Enabled: true},
	}
	r := NewPolicyResolver(cfgs, reg, nil)
	g0 := r.swapGen.Load()

	// Identical content (different slice + reordered) → no swap.
	if swapped := r.SwapIfContentChanged([]core.HookConfig{cfgs[1], cfgs[0]}); swapped {
		t.Error("unchanged content should not swap")
	}
	if g := r.swapGen.Load(); g != g0 {
		t.Errorf("unchanged reload bumped swapGen %d->%d", g0, g)
	}
	// A real change → swap once.
	changed := append([]core.HookConfig(nil), cfgs...)
	changed[0].Enabled = false
	if swapped := r.SwapIfContentChanged(changed); !swapped {
		t.Error("changed content should swap")
	}
	if g := r.swapGen.Load(); g != g0+1 {
		t.Errorf("changed reload should bump swapGen once: %d->%d", g0, g)
	}
	// Direct Swap stays unconditional (one call == one epoch) even on same content.
	r.Swap(changed)
	if g := r.swapGen.Load(); g != g0+2 {
		t.Errorf("direct Swap must always bump: %d->%d", g0, g)
	}
}
