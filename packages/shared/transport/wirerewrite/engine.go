package wirerewrite

import (
	"fmt"
	"log/slog"
	"sync/atomic"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Engine is the top-level wire rewriter. Construct once at startup with New,
// then call Reload whenever the system_metadata config changes.
type Engine struct {
	log      *slog.Logger
	compiled atomic.Pointer[resolvedConfig]
}

// resolvedConfig is the pre-compiled, immutable snapshot derived from a
// Config + bundled rules. Replaced atomically on every Reload.
type resolvedConfig struct {
	enabled bool // gates NormalizeUpstream; NormalizeKey always runs

	// keyRules maps adapter_type → rules that are safe for L0 key normalisation.
	keyRules map[AdapterType][]ruleEntry
	// upstreamRules maps adapter_type → all rules (L3 body modification).
	upstreamRules map[AdapterType][]ruleEntry

	// Per-provider and global settings for L4 marker injection.
	providerInjectEnabled map[string]bool // providerID → inject enabled
	providerBoundary3     map[string]bool // providerID → boundary3 enabled
}

// ruleEntry pairs a Rule with its circuit breaker (shared across reloads
// via the Engine's per-rule breaker map, so error history is preserved on
// config hot-swap).
type ruleEntry struct {
	rule    Rule
	breaker *circuitBreaker
}

// safeRun executes the rule's transformation, recovering from panics and
// recording errors in the circuit breaker. Returns original body on failure.
func (e *ruleEntry) safeRun(body []byte) (out []byte, count int, removed int) {
	defer func() {
		if rec := recover(); rec != nil {
			e.breaker.recordError()
			out = body
			count = 0
			removed = 0
		}
	}()
	return e.run(body)
}

func (e *ruleEntry) run(body []byte) (out []byte, count int, removed int) {
	switch e.rule.Type {
	case RuleTypeStrip:
		return applyStripRule(body, e.rule.BodyPath, &e.rule)
	default:
		return body, 0, 0
	}
}

// New constructs an Engine. It loads the bundled rules with their default
// enabled states so the engine is operational before any config push arrives.
func New(log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{
		log: log,
	}
	e.Reload(Config{})
	return e
}

// Reload applies a new Config, rebuilding the compiled rule snapshot
// atomically. Safe to call concurrently; in-flight normalisation calls
// complete against the old snapshot.
func (e *Engine) Reload(cfg Config) {
	bundles := bundledRules()
	// Per-rule circuit breakers are preserved across reloads so error
	// history survives config changes. Build a map from the old snapshot
	// if one exists.
	breakers := map[string]*circuitBreaker{}
	if old := e.compiled.Load(); old != nil {
		for _, rules := range old.keyRules {
			for _, re := range rules {
				breakers[re.rule.ID] = re.breaker
			}
		}
		for _, rules := range old.upstreamRules {
			for _, re := range rules {
				if _, exists := breakers[re.rule.ID]; !exists {
					breakers[re.rule.ID] = re.breaker
				}
			}
		}
	}

	providerInject := make(map[string]bool, len(cfg.Providers))
	providerBoundary3 := make(map[string]bool, len(cfg.Providers))
	for pid, pc := range cfg.Providers {
		providerInject[pid] = pc.CacheMarkerInjectEnabled
		providerBoundary3[pid] = pc.CacheMarkerBoundary3Enabled
	}

	resolved := &resolvedConfig{
		enabled:               cfg.NormaliserEnabled,
		keyRules:              make(map[AdapterType][]ruleEntry),
		upstreamRules:         make(map[AdapterType][]ruleEntry),
		providerInjectEnabled: providerInject,
		providerBoundary3:     providerBoundary3,
	}

	for _, r := range bundles {
		// Apply operator override if present.
		enabled := r.EnabledByDefault
		dryRun := r.DryRunAlways
		if adapterOverrides, ok := cfg.Rules[r.AdapterType]; ok {
			if ro, ok := adapterOverrides[r.ID]; ok {
				if ro.Enabled != nil {
					enabled = *ro.Enabled
				}
				if ro.DryRunAlways != nil {
					dryRun = *ro.DryRunAlways
				}
			}
		}
		if !enabled {
			continue
		}
		rule := r
		rule.Enabled = true
		rule.DryRunAlways = dryRun

		// Reuse existing circuit breaker or create new one.
		cb, ok := breakers[r.ID]
		if !ok {
			cb = newCircuitBreaker()
		}

		entry := ruleEntry{rule: rule, breaker: cb}

		if rule.KeyNormalizeSafe {
			resolved.keyRules[rule.AdapterType] = append(resolved.keyRules[rule.AdapterType], entry)
		}
		resolved.upstreamRules[rule.AdapterType] = append(resolved.upstreamRules[rule.AdapterType], entry)
	}

	e.compiled.Store(resolved)
	e.log.Debug("wirerewrite reloaded",
		"upstream_enabled", cfg.NormaliserEnabled,
		"rule_count", countRules(resolved),
	)
}

// NormalizeKey strips key-safe volatile fields from body and returns the
// modified bytes for use in Cache.BuildKey ONLY. The upstream body is
// unchanged. Always fail-open: any error returns the original body.
func (e *Engine) NormalizeKey(format AdapterType, body []byte) []byte {
	resolved := e.compiled.Load()
	if resolved == nil {
		return body
	}
	rules := resolved.keyRules[format]
	if len(rules) == 0 {
		return body
	}
	current := body
	for i := range rules {
		re := &rules[i]
		if re.breaker.isOpen() {
			continue
		}
		if re.rule.DryRunAlways {
			continue
		}
		modified, _, _ := re.safeRun(current)
		current = modified
	}
	return current
}

// NormalizeUpstream strips and injects bytes in the body that WILL be
// forwarded to the upstream provider. Returns the modified body and a
// Result summary for audit. Gated by Config.NormaliserEnabled.
// providerID is the Provider row UUID, used for per-Provider L4 settings.
// Always fail-open: any error returns the original body.
func (e *Engine) NormalizeUpstream(format AdapterType, providerID string, body []byte) ([]byte, Result) {
	resolved := e.compiled.Load()
	if resolved == nil || !resolved.enabled {
		return body, Result{}
	}

	result := Result{}
	current := body

	// L3: strip rules.
	rules := resolved.upstreamRules[format]
	allDryRun := len(rules) > 0
	for i := range rules {
		re := &rules[i]
		if re.breaker.isOpen() {
			continue
		}
		if re.rule.DryRunAlways {
			_, c, b := re.safeRun(current)
			result.StripCount += c
			result.StripBytes += b
			if c > 0 {
				result.TransformSpans = append(result.TransformSpans, normalize.TransformSpan{
					Source:   normalize.SourceCacheNormaliser,
					SourceID: re.rule.ID,
					Action:   normalize.ActionStrip,
					// Coarse offsets — cache normaliser strips operate on
					// JSON paths inside the upstream body, not on the
					// canonical NormalizedPayload text projection. The
					// audit reader gets attribution + byte-count via End.
					Start:  0,
					End:    b,
					Reason: "dry-run",
				})
			}
			continue
		}
		allDryRun = false
		modified, c, b := re.safeRun(current)
		current = modified
		result.StripCount += c
		result.StripBytes += b
		if c > 0 {
			result.TransformSpans = append(result.TransformSpans, normalize.TransformSpan{
				Source:   normalize.SourceCacheNormaliser,
				SourceID: re.rule.ID,
				Action:   normalize.ActionStrip,
				Start:    0,
				End:      b,
			})
		}
	}
	if len(rules) == 0 {
		allDryRun = false
	}
	result.DryRun = allDryRun

	// L4: cache_control marker injection (Anthropic and Bedrock-Claude wire).
	// Bedrock Claude uses the identical Anthropic Messages format on the wire,
	// so the same injection logic applies.
	if (format == AdapterAnthropic || format == AdapterBedrock) && resolved.providerInjectEnabled[providerID] {
		injected, injErr := injectCacheMarkers(current, "ephemeral", resolved.providerBoundary3[providerID])
		if injErr == nil {
			n := countInjectedMarkers(current, injected)
			result.MarkersInjected = n
			current = injected
			if n > 0 {
				result.TransformSpans = append(result.TransformSpans, normalize.TransformSpan{
					Source:   normalize.SourceCacheControlInject,
					SourceID: "cache_control",
					Action:   normalize.ActionInject,
					// Inject markers are scattered across the body in
					// canonical-JSON addressable locations; without a
					// per-marker offset we record one summary span.
					Start:  0,
					End:    0,
					Reason: fmt.Sprintf("%d markers injected", n),
				})
			}
		}
	}

	return current, result
}

func countRules(r *resolvedConfig) int {
	n := 0
	seen := map[string]struct{}{}
	for _, rules := range r.upstreamRules {
		for _, re := range rules {
			if _, ok := seen[re.rule.ID]; !ok {
				seen[re.rule.ID] = struct{}{}
				n++
			}
		}
	}
	return n
}
