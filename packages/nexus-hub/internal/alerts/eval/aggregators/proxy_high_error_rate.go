package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// ProxyHighErrorRate fires when the percentage of 5xx responses exceeds
// params.thresholdPct over params.windowSec, per thing_key. Migrated from
// the data-plane in-process check at packages/ai-gateway/internal/alerting/
// (deleted in the same PR per spec §10).
type ProxyHighErrorRate struct{}

// NewProxyHighErrorRate constructs the aggregator.
func NewProxyHighErrorRate() *ProxyHighErrorRate { return &ProxyHighErrorRate{} }

// RuleID returns the AlertRule.id this aggregator implements.
func (a *ProxyHighErrorRate) RuleID() string { return "proxy.high_error_rate" }

// Sources lists the MQ subjects this aggregator subscribes to.
func (a *ProxyHighErrorRate) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{
		alerteval.SourceAITraffic,
		alerteval.SourceCompliance,
		alerteval.SourceAgent,
	}
}

// MinWarmupSec returns the cold-start gate duration.
func (a *ProxyHighErrorRate) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 300)
}

// OnEvent updates the per-thing window with (1, 1) for 5xx and (0, 1) otherwise.
func (a *ProxyHighErrorRate) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
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
	is5xx := derefInt(t.StatusCode) >= 500
	// Cap at 3600s so the window accommodates any reasonable windowSec param value.
	w := rt.Window("thing:"+thingKey, 3600)
	if is5xx {
		w.Add(evt.Timestamp, 1, 1)
	} else {
		w.Add(evt.Timestamp, 0, 1)
	}
}

// Tick evaluates the rule for every active target.
func (a *ProxyHighErrorRate) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 300)
	thresholdPct := intParam(params, "thresholdPct", 10)
	minSamples := intParam(params, "minSamples", 10)

	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("5xx error rate exceeded %d%% over %ds", thresholdPct, windowSec)
		if d := EvalRatioInWindow(rt, target, windowSec, thresholdPct, minSamples, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
