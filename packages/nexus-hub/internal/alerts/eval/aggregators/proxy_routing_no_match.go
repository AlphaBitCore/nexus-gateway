package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// ProxyRoutingNoMatch fires when ai-gateway fails to find a routing
// target for the requested model+endpoint, accumulated globally (not
// per-target) — a sustained no-match rate is almost always a customer
// config issue (deleted rule, model rename, alias drift) and one alert
// is plenty.
type ProxyRoutingNoMatch struct{}

func NewProxyRoutingNoMatch() *ProxyRoutingNoMatch { return &ProxyRoutingNoMatch{} }

func (a *ProxyRoutingNoMatch) RuleID() string { return "proxy.routing_no_match" }

func (a *ProxyRoutingNoMatch) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAITraffic}
}

func (a *ProxyRoutingNoMatch) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 600)
}

func (a *ProxyRoutingNoMatch) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	// Both codes are this aggregator's subject, and the second is the one an
	// operator is likelier to have caused: a rule that MATCHED and resolved
	// nothing, because its target model was deleted or its provider disabled.
	// The doc above already names that population — "deleted rule, model
	// rename, alias drift" — and matching only the older literal left the
	// newer, more specific failure with no alert at all.
	switch derefString(evt.Traffic.ErrorCode) {
	case "ROUTING_NO_MATCH", "ROUTING_RULES_RESOLVED_NOTHING":
	default:
		return
	}
	w := rt.Window("global", 1200)
	w.Add(evt.Timestamp, 1, 0)
}

func (a *ProxyRoutingNoMatch) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 600)
	thresholdCount := intParam(params, "thresholdCount", 20)
	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Routing no-match: %d unrouted requests / %ds — check rule config", thresholdCount, windowSec)
		if d := EvalCountInWindow(rt, target, windowSec, thresholdCount, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
