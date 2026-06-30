package pipeline

import (
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// errFailClosedUnbuildable is the sentinel wrapped when a fail-closed hook
// cannot be built and the caller requested strict fail-closed handling. It lets
// callers (and tests) distinguish "mandatory enforcer unbuildable, must refuse"
// from arbitrary factory errors via errors.Is.
var errFailClosedUnbuildable = fmt.Errorf("fail-closed compliance hook could not be built; refusing to proceed")

// PolicyResolver determines which hooks apply to a given transaction.
//
// The hook config snapshot is held behind an atomic.Pointer so Swap can
// replace it concurrently with in-flight Resolve*/Has* calls — a reader
// keeps its loaded snapshot for the remainder of its call while the next
// caller sees the new one. Config invalidation is lazy and non-blocking.
// configSnapshot bundles the hook-config slice with the generation it was published
// under, so a reader gets a CONSISTENT (gen, configs) pair from ONE atomic load. The
// per-generation cache memos (union prefilter, MaxPatternBound) tag their entries with
// the gen the configs were read under — bundling closes the window where a request could
// read a new swapGen but the old config (or vice versa) and cache a bound/union derived
// from one generation's hooks under another's key. swapGen stays the monotonic counter
// (lockstep with .gen) that Prewarm's staleness recheck and the union close-dance use.
type configSnapshot struct {
	gen     uint64
	configs []core.HookConfig
}

type PolicyResolver struct {
	hookConfigs atomic.Pointer[configSnapshot]
	registry    *core.HookRegistry
	logger      *slog.Logger

	// hookCache caches instantiated Hook objects keyed by HookConfig.ID.
	// On Swap(), entries whose config content is unchanged are preserved;
	// rows that changed or were removed are evicted so the factory runs
	// again with the new config.
	hookMu    sync.RWMutex
	hookCache map[string]core.Hook

	// warnedUnknown deduplicates the "unknown implementationId" warning so
	// we log once per unique ID per reload epoch instead of once per row
	// per resolve() call. Reset on every Swap().
	warnedMu      sync.Mutex
	warnedUnknown map[string]struct{}

	// swapGen is incremented at the start of every Swap. Prewarm captures it and
	// refuses to cache a hook if a swap raced during its (lock-free) build, so a
	// background prewarm can never install a stale-config hook.
	swapGen atomic.Uint64

	// union memoises the folded raw-body prefilter (one matcher over every content
	// hook's anchor-stripped patterns) per resolved hook set, tagged with the
	// swapGen it was built under. See unionprescan.go.
	union unionState

	// patternBound memoises MaxPatternBound per resolved hook set, tagged with the
	// generation it was built under (see maxPatternBoundFor in unionprescan.go).
	boundMu    sync.Mutex
	boundGen   uint64
	boundByKey map[string]patternBound
}

// NewPolicyResolver creates a resolver with an initial hook config snapshot
// and a factory registry. The resolver stores a defensive copy of configs.
// For service-specific hooks, pass a registry cloned via Registry.Clone().
// Subsequent updates go through Swap.
func NewPolicyResolver(configs []core.HookConfig, registry *core.HookRegistry, logger *slog.Logger) *PolicyResolver {
	r := &PolicyResolver{
		registry:  registry,
		logger:    logger,
		hookCache: make(map[string]core.Hook),
	}
	snapshot := append([]core.HookConfig(nil), configs...)
	r.hookConfigs.Store(&configSnapshot{gen: 0, configs: snapshot})
	return r
}

// Swap replaces the current hook configuration with a new snapshot. It is
// safe to call concurrently with Resolve* and Has* readers. Callers that
// have already loaded the previous snapshot see the old data for the
// remainder of their call (Go GC keeps the old backing array alive as
// long as any pointer references it); the next call observes the new
// snapshot.
//
// A defensive copy is taken so the caller cannot mutate the live
// snapshot after Swap returns.
//
// The instantiated-hook cache is reduced by a content diff against the
// previous snapshot: rows whose ID+content are unchanged retain their
// Hook instance, so factory construction runs only for rows that
// actually changed (plus new rows). This keeps reload cost O(changed)
// rather than O(N) when most rows are stable.
func (r *PolicyResolver) Swap(configs []core.HookConfig) {
	prevGen := r.swapGen.Add(1) - 1
	snapshot := append([]core.HookConfig(nil), configs...)
	// Publish gen+configs as ONE atomic value (lockstep with swapGen) so cache
	// consumers reading hookConfigs get a consistent pair — see configSnapshot.
	oldPtr := r.hookConfigs.Swap(&configSnapshot{gen: prevGen + 1, configs: snapshot})

	oldByID := map[string]*core.HookConfig{}
	if oldPtr != nil {
		old := oldPtr.configs
		for i := range old {
			oldByID[old[i].ID] = &old[i]
		}
	}

	r.hookMu.Lock()
	preserved := make(map[string]core.Hook, len(r.hookCache))
	for i := range snapshot {
		cfg := &snapshot[i]
		oldCfg, ok := oldByID[cfg.ID]
		if !ok || !reflect.DeepEqual(oldCfg, cfg) {
			continue
		}
		if h, cached := r.hookCache[cfg.ID]; cached {
			preserved[cfg.ID] = h
		}
	}
	// Hooks built for changed or removed rows are dropped here. Collect them so
	// any that own native resources (the Vectorscan matcher's compiled database
	// and scratch) are released — GC alone never frees cgo memory. Closing runs
	// after the lock so the matcher's in-flight-scan drain cannot stall readers.
	var evicted []core.Hook
	for id, h := range r.hookCache {
		if _, kept := preserved[id]; !kept {
			evicted = append(evicted, h)
		}
	}
	r.hookCache = preserved
	r.hookMu.Unlock()

	// Close immediately. The matcher's own in-flight-scan drain makes this safe
	// for scans already running on the evicted hook. A request that resolved the
	// old hook in the microseconds before this swap but has not yet called Scan
	// will get a no-op (Approve) for that one request — acceptable for the agent,
	// which is the mandated fail-open caller. If the proxies ever ship the
	// Vectorscan tag, switch this to a grace-period deferred close so that
	// resolve→Scan window drains too.
	for _, h := range evicted {
		if c, ok := h.(io.Closer); ok {
			if err := c.Close(); err != nil && r.logger != nil {
				r.logger.Warn("policy: error closing evicted hook", "error", err)
			}
		}
	}

	// Close the PREVIOUS generation's union prefilters (separate compiled
	// databases that copied the old hooks' patterns, so closing the hooks above
	// does not free them). Gen-scoped to prevGen so a concurrent new-generation
	// BuildPipeline that already reset the cache is never disturbed (see
	// closeUnionsIfGen). Drain-safe like the evicted hooks; if a new-gen request
	// already advanced the cache, those stale matchers are freed by its lazy
	// reset instead.
	r.closeUnionsIfGen(prevGen)

	// Reset warn-dedup state so a re-appearing unknown implementationId
	// will log once on the first resolve() after this reload.
	r.warnedMu.Lock()
	r.warnedUnknown = nil
	r.warnedMu.Unlock()
}

// SwapIfChanged replaces the hook config snapshot only if the provided slice
// header differs from the one most recently stored. This avoids clearing the
// hook cache on every request when configs are returned from a TTL cache that
// hands out the same slice. Returns true if a swap occurred.
func (r *PolicyResolver) SwapIfChanged(configs []core.HookConfig) bool {
	cur := r.hookConfigs.Load()
	if cur != nil && len(cur.configs) == len(configs) && len(configs) > 0 {
		// Fast pointer check: if the backing array is the same, skip.
		if &cur.configs[0] == &configs[0] {
			return false
		}
	}
	r.Swap(configs)
	return true
}

// snapshot returns the current hook config slice. Callers MUST capture
// the return value in a local variable and operate on that local slice
// for the remainder of their call — re-reading via snapshot() mid-loop
// could cross a Swap and yield inconsistent results.
func (r *PolicyResolver) snapshot() []core.HookConfig {
	p := r.hookConfigs.Load()
	if p == nil {
		return nil
	}
	return p.configs
}

// loadSnapshot returns the current (gen, configs) pair in one atomic load. Callers
// that build a per-generation cache entry (BuildPipeline → union/bound memos) MUST use
// THIS gen — not a separate r.swapGen.Load() — so the entry is tagged with the gen its
// configs were actually read under (closing the cross-generation cache window).
func (r *PolicyResolver) loadSnapshot() (uint64, []core.HookConfig) {
	p := r.hookConfigs.Load()
	if p == nil {
		return 0, nil
	}
	return p.gen, p.configs
}

// ResolveHooks returns hooks to run for the given stage and ingress type, sorted
// by priority. Filters by: applicableIngress, stage, enabled=true.
//
// strictFailClosed controls how an UNBUILDABLE hook (unknown implementationId,
// factory build error, connection-stage-incompatible) is handled when that hook
// is configured FailBehavior=="fail-closed":
//   - strictFailClosed=true  → such a hook returns an error instead of being
//     skipped, so a mandatory enforcer that cannot be built refuses the request
//     rather than silently becoming a no-op. Used by callers that can SAFELY
//     refuse: the ai-gateway reverse proxy ("refuse" = a 500 to an API client)
//     AND the compliance-proxy forward-proxy appliance (it already
//     returns 403 for disallowed CONNECTs, so refusing an uninspectable request
//     is safe and honours the admin's fail-closed intent).
//   - strictFailClosed=false → the historical skip+log fail-open behavior is
//     preserved for EVERY hook regardless of FailBehavior. REQUIRED ONLY for the
//     genuine host-outbound-packet-path caller: the agent NE proxy (AGENT
//     ingress via tlsbump). There a build error must never refuse/close, which
//     would take down the host's DNS/DHCP/outbound networking. NOTE: tlsbump is
//     shared by both the agent NE proxy and the compliance-proxy; the strictness
//     is now threaded per-caller via tlsbump.WithStrictFailClosed (set by the
//     compliance-proxy, unset by the agent), so "compliance-proxy" is no longer
//     lumped in with the host-path exemption.
//
// Fail-open hooks (and all hooks when strictFailClosed=false) are still skipped
// with a log warning, preserving availability-first graceful degradation.
func (r *PolicyResolver) ResolveHooks(stage, ingressType string, strictFailClosed bool) ([]boundHook, error) {
	return r.resolve(stage, ingressType, strictFailClosed)
}

// resolve filters configs by stage, ingress, and enabled, then instantiates core.
// It captures the current snapshot once (so a concurrent Swap cannot change the set
// mid-call) and delegates to resolveFrom. Callers that ALSO need the generation the
// configs were read under (BuildPipeline, for the per-gen caches) call loadSnapshot +
// resolveFrom directly so the hooks and the cache gen come from the SAME atomic load.
func (r *PolicyResolver) resolve(stage, ingressType string, strictFailClosed bool) ([]boundHook, error) {
	return r.resolveFrom(r.snapshot(), stage, ingressType, strictFailClosed)
}

// resolveFrom is resolve over an already-captured config snapshot. Pointers taken into
// `configs` remain valid for the lifetime of the returned boundHook slice (Go GC keeps
// the backing array alive as long as any pointer references it).
func (r *PolicyResolver) resolveFrom(configs []core.HookConfig, stage, ingressType string, strictFailClosed bool) ([]boundHook, error) {
	var out []boundHook

	for i := range configs {
		cfg := &configs[i]

		if !cfg.Enabled {
			continue
		}

		if !strings.EqualFold(cfg.Stage, stage) {
			continue
		}

		if !r.matchesIngress(cfg, ingressType) {
			continue
		}

		factory := r.registry.Get(cfg.ImplementationID)
		if factory == nil {
			if strictFailClosed && strings.EqualFold(cfg.FailBehavior, "fail-closed") {
				return nil, fmt.Errorf("hook %q (impl %q): unknown implementationId (no factory registered) and FailBehavior=fail-closed: %w",
					cfg.ID, cfg.ImplementationID, errFailClosedUnbuildable)
			}
			r.warnUnknownImpl(cfg.ImplementationID, cfg.ID, cfg.Name)
			continue
		}

		// Check cache first (read lock).
		r.hookMu.RLock()
		cached, cacheHit := r.hookCache[cfg.ID]
		r.hookMu.RUnlock()

		if cacheHit {
			out = append(out, boundHook{hook: cached, config: cfg})
			continue
		}

		// Cache miss: acquire write lock and double-check to avoid TOCTOU race
		// where two goroutines both miss the RLock check simultaneously.
		r.hookMu.Lock()
		if existing, ok := r.hookCache[cfg.ID]; ok {
			r.hookMu.Unlock()
			out = append(out, boundHook{hook: existing, config: cfg})
			continue
		}

		hook, err := factory(cfg)
		if err != nil {
			r.hookMu.Unlock()
			if strictFailClosed && strings.EqualFold(cfg.FailBehavior, "fail-closed") {
				return nil, fmt.Errorf("hook %q (impl %q): factory build error and FailBehavior=fail-closed: %w",
					cfg.ID, cfg.ImplementationID, err)
			}
			// Availability-first graceful degradation: a single hook whose
			// factory fails (bad config, uncompilable rule pattern, etc.) is
			// skipped+logged rather than aborting the entire pipeline build.
			// Aborting would degrade ALL compliance to off (or 500-storm the
			// data plane) for one broken rule; skipping degrades only "that
			// hook off". Mirrors the unknown-implementationId continue above
			// and the per-hook fail-open posture in pipeline.executeOneHook.
			r.warnSkippedHook(cfg.ImplementationID, cfg.ID, cfg.Name, err)
			continue
		}

		if strings.EqualFold(cfg.Stage, "connection") {
			if _, ok := hook.(core.ConnectionStageCompatible); !ok {
				r.hookMu.Unlock()
				// The hook was built but is being dropped (not cached), so free
				// any native resources it holds (a Vectorscan matcher's cgo DB).
				closeHook(hook)
				if strictFailClosed && strings.EqualFold(cfg.FailBehavior, "fail-closed") {
					return nil, fmt.Errorf("hook %q (impl %q): not connection-stage compatible (connection stage forbids MODIFY-capable hooks) and FailBehavior=fail-closed: %w",
						cfg.ID, cfg.ImplementationID, errFailClosedUnbuildable)
				}
				// Same availability-first posture: a connection-stage hook that
				// is not connection-compatible is a misconfiguration of one
				// hook, not grounds to take down the connection-stage pipeline.
				r.warnSkippedHook(cfg.ImplementationID, cfg.ID, cfg.Name,
					fmt.Errorf("not connection-stage compatible; connection stage forbids MODIFY-capable hooks"))
				continue
			}
		}

		r.hookCache[cfg.ID] = hook
		r.hookMu.Unlock()

		out = append(out, boundHook{hook: hook, config: cfg})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].config.Priority < out[j].config.Priority
	})

	return out, nil
}

// BuildPipeline resolves hooks for the given stage and ingress type and returns a
// ready-to-execute Pipeline. Returns nil (no error) if no hooks are applicable.
//
// endpointType and modalities are applied after the Enabled/Stage/Ingress gates.
// Pass an empty endpointType ("") to skip the endpoint gate (backward-compatible
// for connection-stage hooks and callers that have not yet classified the
// endpoint). Pass nil/empty modalities to skip the modality gate. Hooks that do
// not support the endpoint or modality are excluded and PipelineSkippedTotal is
// incremented.
//
// strictFailClosed is forwarded to ResolveHooks: pass true for dedicated-proxy
// callers that can safely REFUSE uninspectable traffic — the reverse-proxy
// ai-gateway (refuses with a 500) and the compliance-proxy forward-proxy
// appliance (refuses the CONNECT / request / response with a 403/451) — so a
// fail-closed hook that cannot be built returns an error rather than silently
// degrading to a no-op. Pass false ONLY for host-network in-path callers (agent
// NE proxy, and tlsbump when driven by that path) where a build error must stay
// fail-open to avoid taking down host networking (CLAUDE.md NE safety rule).
// See ResolveHooks for the full contract.
func (r *PolicyResolver) BuildPipeline(
	stage, ingressType string,
	endpointType core.EndpointType,
	modalities []core.Modality,
	perHookTimeout, totalTimeout time.Duration,
	parallel bool,
	strictFailClosed bool,
	logger *slog.Logger,
) (*Pipeline, error) {
	// Load the (gen, configs) pair ONCE so the hooks we resolve and the generation
	// we tag the per-gen caches with come from the same atomic snapshot.
	gen, configs := r.loadSnapshot()
	candidates, err := r.resolveFrom(configs, stage, ingressType, strictFailClosed)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Apply endpoint + modality gates.
	//
	// Embedding response gate: TextOnlyContentScanning returns
	// SupportsEndpoint=true for EndpointTypeEmbeddings to allow request-side
	// scanning. However, embedding responses contain only float vectors — no
	// scannable text. Skip all text-scanning hooks on the embedding response
	// stage to avoid misleading APPROVE audit rows and wasted hook CPU.
	isEmbeddingResponseStage := stage == "response" && endpointType == core.EndpointTypeEmbeddings

	filtered := make([]boundHook, 0, len(candidates))
	for _, bh := range candidates {
		// Drop text-scanning hooks on embedding response stage (float vectors
		// contain no scannable text).
		if isEmbeddingResponseStage {
			if _, isTextOnly := bh.hook.(core.TextOnlyContentScanningMarker); isTextOnly {
				PipelineSkippedTotal.WithLabelValues(string(endpointType), "embedding_response_no_text", stage).Inc()
				continue
			}
		}

		if endpointType != "" && !bh.hook.SupportsEndpoint(endpointType) {
			PipelineSkippedTotal.WithLabelValues(string(endpointType), "unsupported_endpoint", stage).Inc()
			continue
		}
		if len(modalities) > 0 {
			anyMatch := false
			for _, m := range modalities {
				if bh.hook.SupportsModality(m) {
					anyMatch = true
					break
				}
			}
			if !anyMatch {
				PipelineSkippedTotal.WithLabelValues(string(endpointType), "unsupported_modality", stage).Inc()
				continue
			}
		}
		filtered = append(filtered, bh)
	}

	if len(filtered) == 0 {
		return nil, nil
	}
	p := NewPipeline(filtered, perHookTimeout, totalTimeout, parallel, logger)
	// Thread the same strict posture forwarded to ResolveHooks onto the runtime
	// pipeline so an enforcing hook's ERROR/TIMEOUT/PANIC fails closed on strict
	// (non-packet-path) callers — matching the build-time UNBUILDABLE posture.
	p.SetStrictFailClosed(strictFailClosed)
	// Fold every content hook's raw-body prefilter into one shared scan for this
	// resolved set (cached per generation). nil => use the per-hook loop. The gen is
	// the one the configs were loaded under (above), not a fresh read.
	p.unionPrescan = r.unionPrescanFor(filtered, gen)
	// Pre-stamp the per-generation cached MaxPatternBound so the streaming hot path
	// reads O(1) instead of re-walking every hook regex per request. Only the response
	// stage consumes the bound (Model-A streaming), so skip the cache lookup + signature
	// work on request/connection-stage builds that never read it. EqualFold for parity
	// with resolveFrom's stage match (a non-canonical "Response" would otherwise silently
	// fall back to the per-request lazy compute — the exact cost being optimized away).
	if strings.EqualFold(stage, "response") {
		p.maxBounded, p.anyUnbounded = r.maxPatternBoundFor(filtered, gen)
		p.boundComputed = true
	}
	return p, nil
}

// HasHooks returns true if any enabled hooks exist for the given stage.
func (r *PolicyResolver) HasHooks(stage string) bool {
	configs := r.snapshot()
	for i := range configs {
		if configs[i].Enabled && strings.EqualFold(configs[i].Stage, stage) {
			return true
		}
	}
	return false
}
