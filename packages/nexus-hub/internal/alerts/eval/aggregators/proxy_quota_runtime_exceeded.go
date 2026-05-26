package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// ProxyQuotaRuntimeExceeded fires when the count of QUOTA_EXCEEDED
// rejections per VK in the recent window meets/exceeds the threshold.
// Distinct from quota.threshold (rollup-based 80%/95% soft warning) —
// this fires on hard runtime rejections that already cost the caller a
// 4xx, indicating the soft threshold's notice was missed or ignored.
type ProxyQuotaRuntimeExceeded struct{}

func NewProxyQuotaRuntimeExceeded() *ProxyQuotaRuntimeExceeded {
	return &ProxyQuotaRuntimeExceeded{}
}

func (a *ProxyQuotaRuntimeExceeded) RuleID() string { return "proxy.quota_runtime_exceeded" }

func (a *ProxyQuotaRuntimeExceeded) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAITraffic}
}

func (a *ProxyQuotaRuntimeExceeded) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 300)
}

func (a *ProxyQuotaRuntimeExceeded) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	if derefString(evt.Traffic.ErrorCode) != "QUOTA_EXCEEDED" {
		return
	}
	if derefString(evt.Traffic.EntityType) != "vk" || derefString(evt.Traffic.EntityID) == "" {
		return
	}
	w := rt.Window("vk:"+derefString(evt.Traffic.EntityID), 600)
	w.Add(evt.Timestamp, 1, 0)
}

func (a *ProxyQuotaRuntimeExceeded) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 300)
	thresholdCount := intParam(params, "thresholdCount", 10)
	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Quota runtime exceeded: %d rejections / %ds", thresholdCount, windowSec)
		if d := EvalCountInWindow(rt, target, windowSec, thresholdCount, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
