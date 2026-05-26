package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// ProxyCostSpike fires when sum(estimated_cost_usd) per VK over
// params.windowSec exceeds params.thresholdUsd. Migrated from the
// data-plane in-process check at packages/ai-gateway/internal/alerting/
// (deleted in the same PR per spec §10).
//
// Source: ai-traffic only (compliance-proxy doesn't compute cost; agent
// traffic isn't billable today).
type ProxyCostSpike struct{}

// NewProxyCostSpike constructs the aggregator.
func NewProxyCostSpike() *ProxyCostSpike { return &ProxyCostSpike{} }

// RuleID returns the AlertRule.id this aggregator implements.
func (a *ProxyCostSpike) RuleID() string { return "proxy.cost_spike" }

// Sources lists the MQ subjects this aggregator subscribes to.
func (a *ProxyCostSpike) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAITraffic}
}

// MinWarmupSec returns the cold-start gate duration.
func (a *ProxyCostSpike) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 3600)
}

// OnEvent sums cost into the per-VK window.
func (a *ProxyCostSpike) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	if derefString(t.EntityType) != "vk" || derefString(t.EntityID) == "" {
		return
	}
	cost := derefFloat(t.EstimatedCostUSD)
	if cost <= 0 {
		return
	}
	w := rt.Window("vk:"+derefString(t.EntityID), 3600)
	w.Add(evt.Timestamp, cost, 1)
}

// Tick evaluates the rule for every active target.
func (a *ProxyCostSpike) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 3600)
	thresholdUsd := floatParam(params, "thresholdUsd", 100.0)

	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Cost exceeded $%.2f over %ds", thresholdUsd, windowSec)
		if d := EvalSumInWindow(rt, target, windowSec, thresholdUsd, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
