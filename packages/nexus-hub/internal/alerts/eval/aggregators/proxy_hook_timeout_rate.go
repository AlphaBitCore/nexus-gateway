package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// ProxyHookTimeoutRate fires when the ratio of traffic_event rows whose
// hooks_pipeline contains any record with a timeout-flavored Error string
// exceeds params.thresholdPct over params.windowSec. Migrated from the
// data-plane in-process check.
type ProxyHookTimeoutRate struct{}

// NewProxyHookTimeoutRate constructs the aggregator.
func NewProxyHookTimeoutRate() *ProxyHookTimeoutRate { return &ProxyHookTimeoutRate{} }

// RuleID returns the AlertRule.id.
func (a *ProxyHookTimeoutRate) RuleID() string { return "proxy.hook_timeout_rate" }

// Sources lists the MQ subjects this aggregator subscribes to.
func (a *ProxyHookTimeoutRate) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{
		alerteval.SourceAITraffic,
		alerteval.SourceCompliance,
		alerteval.SourceAgent,
	}
}

// MinWarmupSec returns the cold-start gate duration.
func (a *ProxyHookTimeoutRate) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 300)
}

// OnEvent updates the per-thing window.
func (a *ProxyHookTimeoutRate) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	hasAnyHook, _, hasTimeout := walkHooks(t.RequestHooksPipeline, t.ResponseHooksPipeline)
	if !hasAnyHook {
		return
	}
	thingKey := derefString(t.SourceProcess)
	if thingKey == "" {
		thingKey = t.Source
	}
	if thingKey == "" {
		return
	}
	// Cap at 3600s so the window accommodates any reasonable windowSec param value.
	w := rt.Window("thing:"+thingKey, 3600)
	if hasTimeout {
		w.Add(evt.Timestamp, 1, 1)
	} else {
		w.Add(evt.Timestamp, 0, 1)
	}
}

// Tick evaluates the rule for every active target.
func (a *ProxyHookTimeoutRate) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 300)
	thresholdPct := intParam(params, "thresholdPct", 10)
	minSamples := intParam(params, "minSamples", 10)

	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Hook timeout rate exceeded %d%% over %ds", thresholdPct, windowSec)
		if d := EvalRatioInWindow(rt, target, windowSec, thresholdPct, minSamples, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
