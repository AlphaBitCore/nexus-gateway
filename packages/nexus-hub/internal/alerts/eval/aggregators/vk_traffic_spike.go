package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// VKTrafficSpike fires when the count of requests over the recent
// alertWindow for a VK exceeds spikeMultiplier × baseline-avg AND meets an
// absolute floor.
//
// Cold-start gate is unusually long for this rule: the baseline window
// can be hours, so MinWarmupSec returns coldStartHours*3600 (defaulting
// to baselineWindow + alertWindow when coldStartHours is 0). On Hub
// restart the rule is muted until the baseline window has had a chance
// to fill (or until coldStartHours elapses, whichever the operator
// chose).
type VKTrafficSpike struct{}

// NewVKTrafficSpike constructs the aggregator.
func NewVKTrafficSpike() *VKTrafficSpike { return &VKTrafficSpike{} }

// RuleID returns the AlertRule.id this aggregator implements.
func (a *VKTrafficSpike) RuleID() string { return "vk.traffic_spike" }

// Sources lists the MQ subjects this aggregator subscribes to.
func (a *VKTrafficSpike) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{
		alerteval.SourceAITraffic,
		alerteval.SourceCompliance,
		alerteval.SourceAgent,
	}
}

// MinWarmupSec returns coldStartHours*3600 when set, else
// baselineWindowSec + alertWindowSec (the natural minimum).
func (a *VKTrafficSpike) MinWarmupSec(params map[string]any) int {
	cs := intParam(params, "coldStartHours", 24)
	if cs > 0 {
		return cs * 3600
	}
	return intParam(params, "baselineWindowSec", 3600) + intParam(params, "alertWindowSec", 300)
}

// OnEvent counts each VK-attributed traffic_event into a per-VK window
// sized to baseline+alert. Window cap is set at first OnEvent for a given
// VK; admin edits to baselineWindowSec take effect only on newly-seen VKs
// (existing windows keep their original cap until eviction).
func (a *VKTrafficSpike) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	if derefString(t.EntityType) != "vk" || derefString(t.EntityID) == "" {
		return
	}
	// Generous default cap so existing windows can hold the largest
	// reasonable baseline window the admin might pick later.
	const defaultCapSec = 3900
	w := rt.Window("vk:"+derefString(t.EntityID), defaultCapSec)
	w.Add(evt.Timestamp, 1, 0)
}

// Tick evaluates the rule for every active target.
func (a *VKTrafficSpike) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	alertSec := intParam(params, "alertWindowSec", 300)
	baselineSec := intParam(params, "baselineWindowSec", 3600)
	mult := floatParam(params, "spikeMultiplier", 10)
	floor := intParam(params, "absFloorReq", 50)

	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("VK traffic spike: last %ds count > %.1fx baseline (floor=%d)", alertSec, mult, floor)
		if d := EvalCompareToBaseline(rt, target, alertSec, baselineSec, mult, floor, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
