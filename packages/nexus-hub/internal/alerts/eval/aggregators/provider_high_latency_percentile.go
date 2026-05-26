package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// ProviderHighLatencyPercentile fires when the p95 latency for a
// provider in the recent alert window exceeds multiplier × baseline
// p95 AND meets the absolute floor (don't fire on tiny absolute
// values). Helps catch provider degradation before full 5xx outage.
type ProviderHighLatencyPercentile struct{}

func NewProviderHighLatencyPercentile() *ProviderHighLatencyPercentile {
	return &ProviderHighLatencyPercentile{}
}

func (a *ProviderHighLatencyPercentile) RuleID() string { return "provider.high_latency_percentile" }

func (a *ProviderHighLatencyPercentile) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAITraffic}
}

func (a *ProviderHighLatencyPercentile) MinWarmupSec(params map[string]any) int {
	return intParam(params, "baselineWindowSec", 3600) + intParam(params, "alertWindowSec", 300)
}

func (a *ProviderHighLatencyPercentile) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	lat := derefInt(t.LatencyMs)
	if lat <= 0 {
		return
	}
	provider := derefString(t.RoutedProviderID)
	if provider == "" {
		provider = derefString(t.ProviderID)
	}
	if provider == "" {
		return
	}
	w := rt.SampleWindow("provider:"+provider, 1000)
	w.Add(evt.Timestamp, float64(lat))
}

func (a *ProviderHighLatencyPercentile) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	pct := floatParam(params, "percentile", 95)
	alertSec := intParam(params, "alertWindowSec", 300)
	baselineSec := intParam(params, "baselineWindowSec", 3600)
	mult := floatParam(params, "multiplier", 2.0)
	floor := floatParam(params, "absFloorMs", 1000)
	minSamples := intParam(params, "minSamples", 50)

	var out []alerteval.Decision
	for _, target := range rt.SampleTargets() {
		msg := fmt.Sprintf("Provider p%.0f latency > %.1fx baseline (floor %.0fms, %ds window)", pct, mult, floor, alertSec)
		if d := EvalPercentileBaseline(rt, target, alertSec, baselineSec, pct, mult, floor, minSamples, 1000, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
