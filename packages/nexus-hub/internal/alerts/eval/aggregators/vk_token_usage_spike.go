package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// VKTokenUsageSpike fires when total_tokens consumed by a VK in the
// recent window exceeds params.thresholdTokens. Token quota exhaustion
// often precedes cost spikes; this rule catches it earlier than
// proxy.cost_spike (cost lags real usage by the metering pass).
type VKTokenUsageSpike struct{}

func NewVKTokenUsageSpike() *VKTokenUsageSpike { return &VKTokenUsageSpike{} }

func (a *VKTokenUsageSpike) RuleID() string { return "vk.token_usage_spike" }

func (a *VKTokenUsageSpike) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAITraffic}
}

func (a *VKTokenUsageSpike) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 3600)
}

func (a *VKTokenUsageSpike) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	if derefString(t.EntityType) != "vk" || derefString(t.EntityID) == "" {
		return
	}
	tokens := derefInt(t.TotalTokens)
	if tokens <= 0 {
		return
	}
	w := rt.Window("vk:"+derefString(t.EntityID), 7200)
	w.Add(evt.Timestamp, float64(tokens), 1)
}

func (a *VKTokenUsageSpike) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 3600)
	thresholdTokens := intParam(params, "thresholdTokens", 1000000)
	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("VK token usage > %d tokens / %ds — quota exhaustion ahead", thresholdTokens, windowSec)
		if d := EvalSumInWindow(rt, target, windowSec, float64(thresholdTokens), now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
