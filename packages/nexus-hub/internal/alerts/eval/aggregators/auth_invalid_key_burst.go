package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// AuthInvalidKeyBurst fires when a single source_ip racks up
// AUTH_INVALID / AUTH_KEY_EXPIRED rejections, indicating brute-force VK
// guessing or a stale client.
type AuthInvalidKeyBurst struct{}

func NewAuthInvalidKeyBurst() *AuthInvalidKeyBurst { return &AuthInvalidKeyBurst{} }

func (a *AuthInvalidKeyBurst) RuleID() string { return "auth.invalid_key_burst" }

func (a *AuthInvalidKeyBurst) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAITraffic}
}

func (a *AuthInvalidKeyBurst) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 300)
}

func (a *AuthInvalidKeyBurst) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	code := derefString(evt.Traffic.ErrorCode)
	if code != "AUTH_INVALID" && code != "AUTH_KEY_EXPIRED" {
		return
	}
	ip := derefString(evt.Traffic.SourceIP)
	if ip == "" {
		return
	}
	w := rt.Window("ip:"+ip, 600)
	w.Add(evt.Timestamp, 1, 0)
}

func (a *AuthInvalidKeyBurst) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 300)
	thresholdCount := intParam(params, "thresholdCount", 20)
	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Invalid-key burst: %d auth failures / %ds from one IP — possible brute-force", thresholdCount, windowSec)
		if d := EvalCountInWindow(rt, target, windowSec, thresholdCount, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
