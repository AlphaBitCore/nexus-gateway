package aggregators

import (
	"bytes"
	"fmt"
	"github.com/goccy/go-json"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// ComplianceHookExecutionTimeoutSurge fires when the count of timed-out
// hook executions, grouped per hook_id, exceeds the threshold over the
// window. Distinct from proxy.hook_timeout_rate (which is per-thing
// rate); this is per-hook absolute count, useful when one specific
// hook (out of many) is misbehaving.
type ComplianceHookExecutionTimeoutSurge struct{}

func NewComplianceHookExecutionTimeoutSurge() *ComplianceHookExecutionTimeoutSurge {
	return &ComplianceHookExecutionTimeoutSurge{}
}

func (a *ComplianceHookExecutionTimeoutSurge) RuleID() string {
	return "compliance.hook_execution_timeout_surge"
}

func (a *ComplianceHookExecutionTimeoutSurge) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{
		alerteval.SourceAITraffic,
		alerteval.SourceCompliance,
		alerteval.SourceAgent,
	}
}

func (a *ComplianceHookExecutionTimeoutSurge) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 300)
}

// hookExecRecordWithID extracts hookId in addition to error fields used
// by proxy_hook_failure_rate.
type hookExecRecordWithID struct {
	HookID string `json:"hookId"`
	Error  string `json:"error"`
}

func walkHookTimeoutsByID(rawA, rawB any) map[string]int {
	out := make(map[string]int)
	for _, raw := range [...]any{rawA, rawB} {
		var arr []hookExecRecordWithID
		switch v := raw.(type) {
		case nil:
			continue
		case []byte:
			if len(v) == 0 {
				continue
			}
			// Fast path: no '{' → no hook-record object → empty/error unmarshal → no
			// timeouts. Skip the parse for the common empty/"[]" pipeline.
			if bytes.IndexByte(v, '{') < 0 {
				continue
			}
			if err := json.Unmarshal(v, &arr); err != nil {
				continue
			}
		case json.RawMessage:
			if len(v) == 0 {
				continue
			}
			// Fast path: no '{' → no hook-record object → empty/error unmarshal → no
			// timeouts. Skip the parse for the common empty/"[]" pipeline.
			if bytes.IndexByte(v, '{') < 0 {
				continue
			}
			if err := json.Unmarshal(v, &arr); err != nil {
				continue
			}
		default:
			continue
		}
		for _, r := range arr {
			if r.HookID == "" || r.Error == "" {
				continue
			}
			low := strings.ToLower(r.Error)
			if strings.Contains(low, "deadline exceeded") || strings.Contains(low, "timeout") {
				out[r.HookID]++
			}
		}
	}
	return out
}

func (a *ComplianceHookExecutionTimeoutSurge) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	timeouts := walkHookTimeoutsByID(t.RequestHooksPipeline, t.ResponseHooksPipeline)
	for hookID, n := range timeouts {
		w := rt.Window("hook:"+hookID, 600)
		w.Add(evt.Timestamp, float64(n), 0)
	}
}

func (a *ComplianceHookExecutionTimeoutSurge) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 300)
	thresholdCount := intParam(params, "thresholdCount", 20)
	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Hook timeout surge: %d timeouts / %ds — investigate hook backend", thresholdCount, windowSec)
		if d := EvalCountInWindow(rt, target, windowSec, thresholdCount, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
