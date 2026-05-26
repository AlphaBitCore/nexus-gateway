package aggregators

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// hookExecRecord mirrors the per-stage struct that ai-gateway and
// compliance-proxy both serialize into traffic_event.{request,response}_
// hooks_pipeline. The Error field is non-empty when a hook errored
// (runtime error or timeout); empty for successful executions.
type hookExecRecord struct {
	Stage string `json:"stage"`
	Error string `json:"error"`
}

// walkHooks decodes both per-stage JSONB pipelines and returns whether
// any hook ran (denominator) and whether any hook reported a non-empty
// Error string (numerator for failure_rate). For timeout_rate the caller
// supplies a stricter predicate via the second return.
func walkHooks(rawA, rawB any) (hasAnyHook, hasFailure, hasTimeout bool) {
	for _, raw := range [...]any{rawA, rawB} {
		var arr []hookExecRecord
		switch v := raw.(type) {
		case nil:
			continue
		case []byte:
			if len(v) == 0 {
				continue
			}
			if err := json.Unmarshal(v, &arr); err != nil {
				continue
			}
		case json.RawMessage:
			if len(v) == 0 {
				continue
			}
			if err := json.Unmarshal(v, &arr); err != nil {
				continue
			}
		default:
			continue
		}
		for _, r := range arr {
			hasAnyHook = true
			if r.Error != "" {
				hasFailure = true
				if isTimeoutErr(r.Error) {
					hasTimeout = true
				}
			}
		}
	}
	return
}

// isTimeoutErr matches the canonical timeout markers emitted by the hook
// runner (context.DeadlineExceeded.Error() == "context deadline exceeded";
// some callers wrap with "timeout"). Pinned here so the aggregator stays
// independent of the hook package's exact error strings.
func isTimeoutErr(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "deadline exceeded") || strings.Contains(low, "timeout")
}

// ProxyHookFailureRate fires when the ratio of traffic_event rows whose
// hooks_pipeline contains any record with Error != "" exceeds
// params.thresholdPct over params.windowSec.
type ProxyHookFailureRate struct{}

// NewProxyHookFailureRate constructs the aggregator.
func NewProxyHookFailureRate() *ProxyHookFailureRate { return &ProxyHookFailureRate{} }

// RuleID returns the AlertRule.id.
func (a *ProxyHookFailureRate) RuleID() string { return "proxy.hook_failure_rate" }

// Sources lists the MQ subjects this aggregator subscribes to.
func (a *ProxyHookFailureRate) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{
		alerteval.SourceAITraffic,
		alerteval.SourceCompliance,
		alerteval.SourceAgent,
	}
}

// MinWarmupSec returns the cold-start gate duration.
func (a *ProxyHookFailureRate) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 300)
}

// OnEvent updates the per-thing window.
func (a *ProxyHookFailureRate) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	hasAnyHook, hasFailure, _ := walkHooks(t.RequestHooksPipeline, t.ResponseHooksPipeline)
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
	if hasFailure {
		w.Add(evt.Timestamp, 1, 1)
	} else {
		w.Add(evt.Timestamp, 0, 1)
	}
}

// Tick evaluates the rule for every active target.
func (a *ProxyHookFailureRate) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 300)
	thresholdPct := intParam(params, "thresholdPct", 20)
	minSamples := intParam(params, "minSamples", 10)

	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Hook failure rate exceeded %d%% over %ds", thresholdPct, windowSec)
		if d := EvalRatioInWindow(rt, target, windowSec, thresholdPct, minSamples, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
