package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// ProxyRateLimitExceeded fires when the count of rate-limited requests
// (error_code='RATE_LIMITED', set by ai-gateway's writeDetailedErr when
// a VK exceeds rateLimitRpm) exceeds params.thresholdCount over
// params.windowSec. groupBy = vk | ip | all.
type ProxyRateLimitExceeded struct{}

func NewProxyRateLimitExceeded() *ProxyRateLimitExceeded { return &ProxyRateLimitExceeded{} }

func (a *ProxyRateLimitExceeded) RuleID() string { return "proxy.rate_limit_exceeded" }

func (a *ProxyRateLimitExceeded) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAITraffic}
}

func (a *ProxyRateLimitExceeded) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 300)
}

func (a *ProxyRateLimitExceeded) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	if derefString(evt.Traffic.ErrorCode) != "RATE_LIMITED" {
		return
	}
	t := evt.Traffic
	// Index by all 3 group keys; Tick filters by configured groupBy.
	w := rt.Window("rl:all", 600)
	w.Add(evt.Timestamp, 1, 0)
	if vk := derefString(t.EntityID); vk != "" && derefString(t.EntityType) == "vk" {
		w := rt.Window("rl:vk:"+vk, 600)
		w.Add(evt.Timestamp, 1, 0)
	}
	if ip := derefString(t.SourceIP); ip != "" {
		w := rt.Window("rl:ip:"+ip, 600)
		w.Add(evt.Timestamp, 1, 0)
	}
}

func (a *ProxyRateLimitExceeded) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 300)
	thresholdCount := intParam(params, "thresholdCount", 30)
	groupBy := stringParam(params, "groupBy", "vk")

	prefix := "rl:vk:"
	switch groupBy {
	case "all":
		prefix = "rl:all"
	case "ip":
		prefix = "rl:ip:"
	}

	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		matchesAll := groupBy == "all" && target == prefix
		matchesPrefix := groupBy != "all" && len(target) > len(prefix) && target[:len(prefix)] == prefix
		if !matchesAll && !matchesPrefix {
			continue
		}
		msg := fmt.Sprintf("Rate limit exceeded: %d hits / %ds (groupBy=%s)", thresholdCount, windowSec, groupBy)
		if d := EvalCountInWindow(rt, target, windowSec, thresholdCount, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
