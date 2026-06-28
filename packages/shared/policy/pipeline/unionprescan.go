package pipeline

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

// SwapIfContentChanged swaps only when configs differ in content from the current
// snapshot, returning true if a swap occurred. The hook-config cache fires its
// reload callback on EVERY load — including the 2-minute TTL backstop refresh that
// exists only to recover a missed Hub push — so calling this instead of Swap makes
// a no-change reload a true no-op: no generation bump, no hook/union recompile.
// The recompile then fires only on the two legitimate entry points: the startup
// load (nil→config) and a real config-change sync (Hub OnConfigChanged delta),
// both of which differ from the current snapshot. Swap itself stays unconditional
// (one call == one reload epoch) for callers that mean "reload now".
func (r *PolicyResolver) SwapIfContentChanged(configs []core.HookConfig) bool {
	if r.sameAsCurrent(configs) {
		return false
	}
	r.Swap(configs)
	return true
}

// sameAsCurrent reports whether configs is content-identical to the current
// snapshot, treated as an unordered set keyed by HookConfig.ID (load order from
// the DB / push is not significant). A differing length, a changed/added/removed
// ID, or any field difference (including the per-hook Config map of patterns)
// returns false.
func (r *PolicyResolver) sameAsCurrent(configs []core.HookConfig) bool {
	cur := r.snapshot()
	if len(cur) != len(configs) {
		return false
	}
	curByID := make(map[string]*core.HookConfig, len(cur))
	for i := range cur {
		curByID[cur[i].ID] = &cur[i]
	}
	for i := range configs {
		prev, ok := curByID[configs[i].ID]
		if !ok || !reflect.DeepEqual(prev, &configs[i]) {
			return false
		}
	}
	return true
}

// bytesView returns a read-only string view over b without copying. The matcher
// only reads the segment during Scan and never retains it, so aliasing is safe
// and avoids a per-request 50 KB allocation on the hot path. Mirrors the
// validators' bytesView used by the per-hook prefilter.
func bytesView(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// unionPrescanEnabled gates the folded raw-body prefilter. Default ON — the
// optimal posture, so the win needs no configuration. Set
// NEXUS_HOOK_UNION_PRESCAN=0 (or false/off/no) to fall back to the per-hook loop
// for an A/B comparison on the SAME binary (restart to pick up the change). Read
// once at process start; the fallback path is the original, always-correct
// behaviour, so flipping it can only trade throughput, never correctness.
var unionPrescanEnabled = func() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NEXUS_HOOK_UNION_PRESCAN"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}()

// Union raw-body prefilter.
//
// MayMatchRawContent's per-hook loop runs one cgo Vectorscan scan of the whole
// raw body for EVERY content-scanning hook — the profiled hot spot under
// hooks-on load (3 enabled content hooks => 3 scans of the same 50 KB). This
// file folds all those per-hook prefilters into ONE matcher: the boolean result
// of scanning the union is identical to OR-ing every hook's MayMatchRaw, so the
// extraction-skip decision is unchanged while the scan runs once.
//
// SOUNDNESS is the load-bearing invariant: MayMatchRawContent may return false
// (skip the structured scan) ONLY when no hook could match. The union is used
// ONLY when it provably represents every hook the per-hook loop would consult
// (buildUnionPrescan returns ok=false otherwise) and the per-request hook set is
// byte-identical to the set the union was built from (signature match). Any
// doubt falls back to the per-hook loop, which is the original, always-correct
// behaviour.

// unionEntry caches one compiled union prefilter (or a negative result, m==nil)
// for a specific resolved hook set within one reload generation. A cached nil
// means "this set cannot be unioned — use the per-hook loop", so a miss is never
// confused with "the union found no match".
type unionEntry struct {
	m matcher.Matcher
}

// unionState is the per-resolver memo of compiled union prefilters, keyed by the
// resolved hook-set signature and tagged with the swapGen it was built under. A
// generation change closes every stale matcher (cgo memory is never GC'd) and
// clears the map. `building` single-flights compilation: only the first request
// to miss a key compiles it (a ~100ms+ cgo build); concurrent requests for the
// same key fall back to the per-hook loop until the build is cached, instead of
// each compiling their own copy (the "thundering herd" that stalled the request
// path for seconds after every cache reset).
type unionState struct {
	mu       sync.Mutex
	gen      uint64
	byKey    map[string]*unionEntry
	building map[string]struct{}
}

// hookSetSignature returns a stable key for a resolved+filtered hook set: the
// sorted HookConfig IDs. Within one reload generation the resolver rebuilds any
// hook whose config changed (Swap evicts it and bumps swapGen), so identical IDs
// in the same generation are guaranteed identical hooks — the signature need not
// hash pattern contents.
func hookSetSignature(hooks []boundHook) string {
	ids := make([]string, 0, len(hooks))
	for i := range hooks {
		id := ""
		if hooks[i].config != nil {
			id = hooks[i].config.ID
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, "\x00")
}

// stableHookIDs reports whether every hook carries a non-empty, set-unique
// config ID. The per-generation union cache is keyed only by these IDs, so the
// equivalence "same signature ⟹ same hook membership" holds ONLY when IDs are
// non-empty and unique (which DB-sourced HookConfig.ID — a primary key — always
// is). If the invariant is ever violated, the union is skipped (per-hook loop),
// because a colliding signature could serve one set's union to a different set
// and miss a match. Soundness over the optimisation.
func stableHookIDs(hooks []boundHook) bool {
	seen := make(map[string]struct{}, len(hooks))
	for i := range hooks {
		if hooks[i].config == nil || hooks[i].config.ID == "" {
			return false
		}
		id := hooks[i].config.ID
		if _, dup := seen[id]; dup {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

// buildUnionPrescan compiles a single prefilter over every content hook's
// anchor-stripped pattern set. It returns ok=false (caller falls back to the
// per-hook loop) when the set cannot be soundly unioned:
//
//   - a hook that is not a RawContentPrescanner — the loop forces "may match"
//     for it, so no union could skip extraction on its behalf;
//   - a content-scanning hook that is not a PrescanPatternSource, or whose
//     PrescanPatterns() is nil — its own prefilter could not be built, so it
//     conservatively forces "may match";
//   - no content-scanning hook at all (nothing to fold; the loop returns false
//     cheaply without a scan);
//   - a pattern that fails to compile into the union.
//
// Metadata hooks (RawContentPrescanner with ScansContent()==false: rate limit,
// IP, size) are skipped — they never force extraction.
func buildUnionPrescan(hooks []boundHook) (matcher.Matcher, bool) {
	var pats []matcher.Pattern
	id := 0
	contentHooks := 0
	for i := range hooks {
		pre, ok := hooks[i].hook.(core.RawContentPrescanner)
		if !ok {
			return nil, false // unaccounted hook forces true; no union helps
		}
		if !pre.ScansContent() {
			continue // metadata hook: never forces extraction
		}
		src, ok := hooks[i].hook.(core.PrescanPatternSource)
		if !ok {
			return nil, false // content hook can't export its prefilter
		}
		pp := src.PrescanPatterns()
		if pp == nil {
			return nil, false // nil prefilter => hook conservatively forces true
		}
		contentHooks++
		for _, p := range pp {
			pats = append(pats, matcher.Pattern{ID: id, Expr: p.Expr, Flags: p.Flags})
			id++
		}
	}
	if contentHooks == 0 || len(pats) == 0 {
		return nil, false // nothing to fold; per-hook loop returns false cheaply
	}
	m, bad := matcher.CompileDefault(pats)
	if len(bad) > 0 {
		// A stripped pattern that the per-hook prefilter compiled but the union
		// did not is not provably sound — fall back rather than risk a miss.
		if c, ok := m.(interface{ Close() error }); ok {
			_ = c.Close()
		}
		return nil, false
	}
	return m, true
}

// unionPrescanFor returns the cached union prefilter for the given resolved hook
// set, compiling it ONCE per generation under single-flight. Returns nil when the
// set cannot be unioned, while another goroutine is already compiling this key,
// or while a build is in progress — in all those cases the caller uses the
// per-hook loop (the original, always-correct path), so a cache miss never blocks
// the request path on the ~100ms+ compile and a reset never triggers a herd of
// concurrent compiles.
func (r *PolicyResolver) unionPrescanFor(hooks []boundHook) matcher.Matcher {
	if !unionPrescanEnabled {
		return nil // A/B opt-out: per-hook loop
	}
	if !stableHookIDs(hooks) {
		return nil // ambiguous cache key → fall back rather than risk a wrong-set union
	}
	gen := r.swapGen.Load()
	key := hookSetSignature(hooks)

	r.union.mu.Lock()
	stale := r.resetUnionsLockedIfGenChanged(gen) // detaches old matchers (closed off-lock below)
	if e, ok := r.union.byKey[key]; ok {
		m := e.m
		r.union.mu.Unlock()
		closeAll(stale)
		return m
	}
	if _, building := r.union.building[key]; building {
		r.union.mu.Unlock()
		closeAll(stale)
		return nil // single-flight: another goroutine is compiling → per-hook loop
	}
	r.union.building[key] = struct{}{} // claim the single-flight build
	r.union.mu.Unlock()
	closeAll(stale) // close the previous generation's matchers OFF the lock

	// Compile once, off the lock.
	m, _ := buildUnionPrescan(hooks)

	r.union.mu.Lock()
	delete(r.union.building, key) // release the claim (building map may have been
	// replaced by a racing reset — delete is then a harmless no-op)
	// A Swap raced our build (generation advanced): discard it. The next request
	// rebuilds for the new generation.
	if r.swapGen.Load() != gen || r.union.gen != gen {
		r.union.mu.Unlock()
		if m != nil {
			closeMatcher(m)
		}
		return nil
	}
	r.union.byKey[key] = &unionEntry{m: m}
	r.union.mu.Unlock()
	return m
}

// resetUnionsLockedIfGenChanged re-initialises the cache when the generation
// advanced (or it is uninitialised), returning the previous generation's matchers
// for the caller to Close AFTER releasing r.union.mu. Caller holds r.union.mu.
func (r *PolicyResolver) resetUnionsLockedIfGenChanged(gen uint64) []matcher.Matcher {
	if r.union.gen == gen && r.union.byKey != nil {
		return nil
	}
	stale := r.detachUnionsLocked()
	r.union.byKey = make(map[string]*unionEntry)
	r.union.building = make(map[string]struct{})
	r.union.gen = gen
	return stale
}

// detachUnionsLocked removes every cached matcher from the map and returns them
// so the caller can Close them OUTSIDE r.union.mu. Closing a Vectorscan matcher
// blocks on its in-flight-scan drain; doing that under the cache lock would stall
// every concurrent BuildPipeline. Caller holds r.union.mu.
func (r *PolicyResolver) detachUnionsLocked() []matcher.Matcher {
	var ms []matcher.Matcher
	for _, e := range r.union.byKey {
		if e != nil && e.m != nil {
			ms = append(ms, e.m)
		}
	}
	r.union.byKey = nil
	return ms
}

// closeUnionsIfGen closes the cached union matchers ONLY if the cache still holds
// the given generation — called from Swap with the PRE-increment generation so a
// reload promptly releases the OLD generation's compiled databases without ever
// touching the new generation's entries. Closing happens OFF the lock.
//
// Why gen-scoped (not unconditional): Swap increments swapGen at its START but
// closes here at its END. In that window a concurrent BuildPipeline can capture
// the NEW generation, reset the cache (union.gen = new), and build+insert for it.
// An unconditional close would nil that fresh map underneath the new-gen builder
// — whose post-build re-check passes (swapGen and union.gen both equal its gen)
// — causing an "assignment to entry in nil map" panic, and would also close a
// just-built same-gen matcher a caller is about to scan. Gating on
// union.gen == gen confines the close to the genuine generation being superseded.
func (r *PolicyResolver) closeUnionsIfGen(gen uint64) {
	r.union.mu.Lock()
	var stale []matcher.Matcher
	if r.union.gen == gen {
		stale = r.detachUnionsLocked()
	}
	r.union.mu.Unlock()
	closeAll(stale)
}

func closeAll(ms []matcher.Matcher) {
	for _, m := range ms {
		closeMatcher(m)
	}
}

func closeMatcher(m matcher.Matcher) {
	if c, ok := m.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}
