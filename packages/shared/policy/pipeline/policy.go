package pipeline

import (
	"fmt"
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
type PolicyResolver struct {
	hookConfigs atomic.Pointer[[]core.HookConfig]
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
	r.hookConfigs.Store(&snapshot)
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
	snapshot := append([]core.HookConfig(nil), configs...)
	oldPtr := r.hookConfigs.Swap(&snapshot)

	oldByID := map[string]*core.HookConfig{}
	if oldPtr != nil {
		old := *oldPtr
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
	r.hookCache = preserved
	r.hookMu.Unlock()

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
	if cur != nil && len(*cur) == len(configs) && len(configs) > 0 {
		// Fast pointer check: if the backing array is the same, skip.
		if &(*cur)[0] == &configs[0] {
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
	return *p
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
func (r *PolicyResolver) resolve(stage, ingressType string, strictFailClosed bool) ([]boundHook, error) {
	var out []boundHook

	// Capture the current snapshot once so that a concurrent Swap does
	// not change the set of configs we iterate over mid-call. Pointers
	// taken into this slice remain valid for the lifetime of the
	// returned boundHook slice because Go GC keeps the backing array
	// alive as long as any pointer references it.
	configs := r.snapshot()

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

// matchesIngress checks whether a hook config applies to the given ingress type.
// Semantics: if any entry in ApplicableIngress matches the current ingressType,
// return true.
//
// Named aliases:
//   - "ALL"                   → matches every ingress type
//   - "AI_GATEWAY"        → matches "AI_GATEWAY" only
//   - "COMPLIANCE_PROXY"  → matches "COMPLIANCE_PROXY" only
//   - "AGENT"             → matches "AGENT" only
//
// Any other value is matched case-insensitively against the ingressType.
func (r *PolicyResolver) matchesIngress(cfg *core.HookConfig, ingressType string) bool {
	if len(cfg.ApplicableIngress) == 0 {
		return true
	}

	for _, ing := range cfg.ApplicableIngress {
		upper := strings.ToUpper(ing)
		if upper == "ALL" {
			return true
		}
		if upper == "AI_GATEWAY" && strings.EqualFold(ingressType, "AI_GATEWAY") {
			return true
		}
		if upper == "COMPLIANCE_PROXY" && strings.EqualFold(ingressType, "COMPLIANCE_PROXY") {
			return true
		}
		if upper == "AGENT" && strings.EqualFold(ingressType, "AGENT") {
			return true
		}
		if strings.EqualFold(upper, ingressType) {
			return true
		}
	}
	return false
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
	candidates, err := r.ResolveHooks(stage, ingressType, strictFailClosed)
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
	return NewPipeline(filtered, perHookTimeout, totalTimeout, parallel, logger), nil
}

// warnUnknownImpl logs a warning for an implementationId that is advertised in
// the database but has no factory registered. The warning fires at most once
// per unique implementationId per reload epoch — Swap() resets the dedup set,
// so a subsequent reload that still references an unknown id will log again.
// The hookId / hookName of the first-seen row are included so operators can
// locate the offending row without searching.
func (r *PolicyResolver) warnUnknownImpl(implID, hookID, hookName string) {
	r.warnedMu.Lock()
	if _, seen := r.warnedUnknown[implID]; seen {
		r.warnedMu.Unlock()
		return
	}
	if r.warnedUnknown == nil {
		r.warnedUnknown = make(map[string]struct{})
	}
	r.warnedUnknown[implID] = struct{}{}
	r.warnedMu.Unlock()

	r.logger.Warn("unknown hook implementation, skipping",
		"implementationId", implID,
		"hookId", hookID,
		"hookName", hookName,
	)
}

// warnSkippedHook logs that a hook was skipped during pipeline build because
// its factory failed or it was stage-incompatible. Deduplicated per hookId
// per reload epoch (Swap resets the dedup set) so a persistently-broken hook
// logs once per reload instead of once per resolve() call. This is the
// availability-first degradation path: the offending hook is dropped; the
// rest of the pipeline still builds and runs.
func (r *PolicyResolver) warnSkippedHook(implID, hookID, hookName string, cause error) {
	r.warnedMu.Lock()
	dedupKey := "skip:" + hookID
	if _, seen := r.warnedUnknown[dedupKey]; seen {
		r.warnedMu.Unlock()
		return
	}
	if r.warnedUnknown == nil {
		r.warnedUnknown = make(map[string]struct{})
	}
	r.warnedUnknown[dedupKey] = struct{}{}
	r.warnedMu.Unlock()

	r.logger.Warn("compliance hook skipped during pipeline build (degrading to this hook off)",
		"implementationId", implID,
		"hookId", hookID,
		"hookName", hookName,
		"error", cause,
	)
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
