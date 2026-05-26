package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// ProviderUpstreamError fires when status_code >= 500 with no Nexus-side
// classification (error_code IS NULL) exceeds params.thresholdPct over
// params.windowSec, per provider. The error_code IS NULL clause is what
// distinguishes upstream errors from Nexus pre-flight rejections — the
// latter always carry a structured code (RATE_LIMITED / QUOTA_EXCEEDED /
// etc.). NULL = "we passed all our checks, the upstream broke".
type ProviderUpstreamError struct{}

func NewProviderUpstreamError() *ProviderUpstreamError { return &ProviderUpstreamError{} }

func (a *ProviderUpstreamError) RuleID() string { return "provider.upstream_error" }

func (a *ProviderUpstreamError) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAITraffic}
}

func (a *ProviderUpstreamError) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 300)
}

func (a *ProviderUpstreamError) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	provider := derefString(t.RoutedProviderID)
	if provider == "" {
		provider = derefString(t.ProviderID)
	}
	if provider == "" {
		return
	}
	w := rt.Window("provider:"+provider, 600)
	if derefInt(t.StatusCode) >= 500 && derefString(t.ErrorCode) == "" {
		w.Add(evt.Timestamp, 1, 1)
	} else {
		w.Add(evt.Timestamp, 0, 1)
	}
}

func (a *ProviderUpstreamError) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 300)
	thresholdPct := intParam(params, "thresholdPct", 10)
	minSamples := intParam(params, "minSamples", 20)
	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Upstream provider error rate >= %d%% over %ds (excluding Nexus rejects)", thresholdPct, windowSec)
		if d := EvalRatioInWindow(rt, target, windowSec, thresholdPct, minSamples, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
