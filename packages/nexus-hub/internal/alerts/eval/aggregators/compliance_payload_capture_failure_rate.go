package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// CompliancePayloadCaptureFailureRate fires when the ratio of captured
// bodies marked Truncated (size hit the per-request cap) exceeds
// threshold per thing_key. A truncation surge means the captured audit
// trail is no longer complete — either the cap is too low for the
// real traffic shape or the spillstore backend is unreachable.
//
// Source: traffic_event.{request,response}_body audit.Body.Truncated
// flag (the data-plane writers stamp it). Spilled bodies are also
// "truncated" when the producer hit its read cap before spilling, so
// the same Truncated flag covers both inline + spill cases.
type CompliancePayloadCaptureFailureRate struct{}

func NewCompliancePayloadCaptureFailureRate() *CompliancePayloadCaptureFailureRate {
	return &CompliancePayloadCaptureFailureRate{}
}

func (a *CompliancePayloadCaptureFailureRate) RuleID() string {
	return "compliance.payload_capture_failure_rate"
}

func (a *CompliancePayloadCaptureFailureRate) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{
		alerteval.SourceAITraffic,
		alerteval.SourceCompliance,
		alerteval.SourceAgent,
	}
}

func (a *CompliancePayloadCaptureFailureRate) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 600)
}

func (a *CompliancePayloadCaptureFailureRate) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
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

	// A row is "truncated" when EITHER direction's Body.Truncated is true
	// AND the capture was attempted (Kind != absent).
	reqAttempted := t.RequestBody.Kind != "" && t.RequestBody.Kind != "absent"
	respAttempted := t.ResponseBody.Kind != "" && t.ResponseBody.Kind != "absent"
	if !reqAttempted && !respAttempted {
		return // payload capture disabled for this row — don't count
	}
	truncated := (reqAttempted && t.RequestBody.Truncated) || (respAttempted && t.ResponseBody.Truncated)

	w := rt.Window("thing:"+thingKey, 1200)
	if truncated {
		w.Add(evt.Timestamp, 1, 1)
	} else {
		w.Add(evt.Timestamp, 0, 1)
	}
}

func (a *CompliancePayloadCaptureFailureRate) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 600)
	thresholdPct := intParam(params, "thresholdPct", 10)
	minSamples := intParam(params, "minSamples", 20)
	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Payload truncation > %d%% over %ds — audit trail incomplete", thresholdPct, windowSec)
		if d := EvalRatioInWindow(rt, target, windowSec, thresholdPct, minSamples, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
