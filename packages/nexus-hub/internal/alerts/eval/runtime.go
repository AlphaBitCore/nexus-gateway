package alerteval

import (
	"sync"
	"time"
)

// Runtime is per-Aggregator state owned by the Engine. Aggregator methods
// receive it as a handle so per-target bookkeeping (windows, cooldowns,
// cold-start gating) is abstracted out of rule code.
type Runtime struct {
	ruleID    string
	startTime time.Time

	mu            sync.Mutex
	windows       map[string]*Window
	sampleWindows map[string]*SampleWindow
	cooldownUntil map[string]time.Time
}

// NewRuntime constructs a Runtime stamped at startTime.
func NewRuntime(ruleID string, startTime time.Time) *Runtime {
	return &Runtime{
		ruleID:        ruleID,
		startTime:     startTime,
		windows:       make(map[string]*Window),
		sampleWindows: make(map[string]*SampleWindow),
		cooldownUntil: make(map[string]time.Time),
	}
}

// RuleID returns the rule id this Runtime is bound to.
func (rt *Runtime) RuleID() string { return rt.ruleID }

// Window returns the per-target Window, lazily constructing one with the
// requested capacity. Existing windows are NOT resized — capSeconds is set
// at first use; later changes to rule.params.windowSec take effect for new
// target_keys but existing ones keep their original capacity until eviction.
func (rt *Runtime) Window(targetKey string, capSeconds int) *Window {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	w, ok := rt.windows[targetKey]
	if !ok {
		w = NewWindow(capSeconds)
		rt.windows[targetKey] = w
	}
	return w
}

// EvictWindow removes the per-target window. Aggregators implementing
// memory-bound eviction (e.g. vk_traffic_spike after long zero-traffic)
// call this from Tick.
func (rt *Runtime) EvictWindow(targetKey string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.windows, targetKey)
	delete(rt.sampleWindows, targetKey)
	delete(rt.cooldownUntil, targetKey)
}

// SampleWindow returns the per-target SampleWindow used by percentile-
// based aggregators (latency p95 etc). capSamples is set at first use;
// later changes do not resize existing windows.
func (rt *Runtime) SampleWindow(targetKey string, capSamples int) *SampleWindow {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	w, ok := rt.sampleWindows[targetKey]
	if !ok {
		w = NewSampleWindow(capSamples)
		rt.sampleWindows[targetKey] = w
	}
	return w
}

// SampleTargets returns the snapshot of target_keys with sample-based
// state (separate from numeric-bucket Targets()).
func (rt *Runtime) SampleTargets() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]string, 0, len(rt.sampleWindows))
	for k := range rt.sampleWindows {
		out = append(out, k)
	}
	return out
}

// IsCooldown reports whether the target_key is currently within its cooldown
// window. The Engine checks this before firing a Raise.
func (rt *Runtime) IsCooldown(targetKey string, now time.Time) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	until, ok := rt.cooldownUntil[targetKey]
	if !ok {
		return false
	}
	return now.Before(until)
}

// SetCooldown stamps the next-allowed-fire time for the target_key.
func (rt *Runtime) SetCooldown(targetKey string, until time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.cooldownUntil[targetKey] = until
}

// HasFired returns whether a fire happened recently enough that the
// cooldown_until entry is still recorded. Used by Aggregators to decide
// whether to emit a Resolve decision (only meaningful when there's an
// active fire to resolve).
func (rt *Runtime) HasFired(targetKey string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	_, ok := rt.cooldownUntil[targetKey]
	return ok
}

// Targets returns the snapshot of active target_keys (those with windows).
// Order is unspecified.
func (rt *Runtime) Targets() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]string, 0, len(rt.windows))
	for k := range rt.windows {
		out = append(out, k)
	}
	return out
}

// WarmupRemaining returns how many seconds remain on the cold-start gate.
// Returns 0 once the gate is satisfied.
func (rt *Runtime) WarmupRemaining(minWarmupSec int, now time.Time) int {
	if minWarmupSec <= 0 {
		return 0
	}
	elapsed := int(now.Sub(rt.startTime).Seconds())
	if elapsed >= minWarmupSec {
		return 0
	}
	return minWarmupSec - elapsed
}
