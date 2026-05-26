package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// VKLatencyDegradation is the per-VK twin of provider.high_latency_percentile —
// catches VK-specific slowness (one customer's auth overhead, hook chain
// depth) that wouldn't show on provider-aggregated metrics.
type VKLatencyDegradation struct{}

func NewVKLatencyDegradation() *VKLatencyDegradation { return &VKLatencyDegradation{} }

func (a *VKLatencyDegradation) RuleID() string { return "vk.latency_degradation" }

func (a *VKLatencyDegradation) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAITraffic}
}

func (a *VKLatencyDegradation) MinWarmupSec(params map[string]any) int {
	return intParam(params, "baselineWindowSec", 3600) + intParam(params, "alertWindowSec", 300)
}

func (a *VKLatencyDegradation) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	if derefString(t.EntityType) != "vk" || derefString(t.EntityID) == "" {
		return
	}
	lat := derefInt(t.LatencyMs)
	if lat <= 0 {
		return
	}
	w := rt.SampleWindow("vk:"+derefString(t.EntityID), 1000)
	w.Add(evt.Timestamp, float64(lat))
}

func (a *VKLatencyDegradation) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	pct := floatParam(params, "percentile", 95)
	alertSec := intParam(params, "alertWindowSec", 300)
	baselineSec := intParam(params, "baselineWindowSec", 3600)
	mult := floatParam(params, "multiplier", 2.0)
	floor := floatParam(params, "absFloorMs", 1000)
	minSamples := intParam(params, "minSamples", 30)

	var out []alerteval.Decision
	for _, target := range rt.SampleTargets() {
		msg := fmt.Sprintf("VK p%.0f latency > %.1fx baseline (floor %.0fms, %ds window)", pct, mult, floor, alertSec)
		if d := EvalPercentileBaseline(rt, target, alertSec, baselineSec, pct, mult, floor, minSamples, 1000, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
