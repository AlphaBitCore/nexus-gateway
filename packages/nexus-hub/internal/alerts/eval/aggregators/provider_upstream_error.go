package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/errorcode"
)

// ProviderUpstreamError fires when the share of requests an upstream itself
// rejected exceeds params.thresholdPct over params.windowSec, per provider.
//
// The question it has to answer is "did the upstream break, or did we reject
// this before ever calling it" — a provider must not be blamed for our own
// routing, quota or auth decisions. It used to answer that with
// `error_code IS NULL`, on the contract that Nexus leaves upstream failures
// unclassified and stamps a code only on its own rejects.
//
// The gateway does not honour that contract: it classifies every upstream
// failure too, so error_code was never NULL on one and this rule counted
// nothing. It had never fired in production — verified 2026-07-15 against the
// live database, where zero 5xx rows carry an empty error_code.
//
// The distinction is now drawn where it actually lives: which vocabulary the
// code belongs to. errorcode.IsUpstream reports whether the upstream answered
// and rejected us, as opposed to a gateway-side decision (ROUTING_NO_MATCH,
// QUOTA_EXCEEDED, …) or a routing failure that never reached one
// (no_compatible_provider). An empty code — a producer that forgot to classify
// — reads as not-upstream rather than silently counting against a provider.
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
	if derefInt(t.StatusCode) >= 500 && errorcode.IsUpstream(derefString(t.ErrorCode)) {
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
