package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// HookRejectRate fires when the percentage of traffic_event rows whose
// request OR response hook decision is REJECT_HARD or BLOCK_SOFT exceeds
// params.thresholdPct over params.windowSec, per thing_key.
type HookRejectRate struct{}

// NewHookRejectRate constructs the aggregator.
func NewHookRejectRate() *HookRejectRate { return &HookRejectRate{} }

// RuleID returns the AlertRule.id this aggregator implements.
func (a *HookRejectRate) RuleID() string { return "hook.reject_rate" }

// Sources lists the MQ subjects this aggregator subscribes to.
func (a *HookRejectRate) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{
		alerteval.SourceAITraffic,
		alerteval.SourceCompliance,
		alerteval.SourceAgent,
	}
}

// MinWarmupSec returns the cold-start gate duration.
func (a *HookRejectRate) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 300)
}

// OnEvent updates the per-thing window.
func (a *HookRejectRate) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	thingKey := derefString(t.SourceProcess)
	if thingKey == "" {
		thingKey = t.Source
	}
	if thingKey == "" {
		return
	}
	// Hardcoded because OnEvent has no params (TOCTOU risk); all hook rejections count.
	rejectTypes := []string{"REJECT_HARD", "BLOCK_SOFT"}
	isReject := stringInSlice(derefString(t.RequestHookDecision), rejectTypes) ||
		stringInSlice(derefString(t.ResponseHookDecision), rejectTypes)
	// Cap at 3600s so the window accommodates any reasonable windowSec param value.
	w := rt.Window("thing:"+thingKey, 3600)
	if isReject {
		w.Add(evt.Timestamp, 1, 1)
	} else {
		w.Add(evt.Timestamp, 0, 1)
	}
}

// Tick evaluates the rule for every active target.
func (a *HookRejectRate) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 300)
	thresholdPct := intParam(params, "thresholdPct", 5)
	minSamples := intParam(params, "minSamples", 20)

	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Hook reject rate exceeded %d%% over %ds", thresholdPct, windowSec)
		if d := EvalRatioInWindow(rt, target, windowSec, thresholdPct, minSamples, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
