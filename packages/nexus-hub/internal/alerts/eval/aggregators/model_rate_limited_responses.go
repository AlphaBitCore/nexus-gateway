package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// ModelRateLimitedResponses fires when the count of 429 responses with
// no Nexus-side classification (so it's an upstream 429, not the
// gateway's own RATE_LIMITED) exceeds the threshold per model. Surfaces
// when an upstream provider is throttling Nexus's combined traffic for
// a particular model, distinct from per-VK Nexus-side rate-limiting.
type ModelRateLimitedResponses struct{}

func NewModelRateLimitedResponses() *ModelRateLimitedResponses {
	return &ModelRateLimitedResponses{}
}

func (a *ModelRateLimitedResponses) RuleID() string { return "model.rate_limited_responses" }

func (a *ModelRateLimitedResponses) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAITraffic}
}

func (a *ModelRateLimitedResponses) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 300)
}

func (a *ModelRateLimitedResponses) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	if derefInt(t.StatusCode) != 429 || derefString(t.ErrorCode) != "" {
		return // not an upstream 429
	}
	model := derefString(t.RoutedModelID)
	if model == "" {
		model = derefString(t.ModelID)
	}
	if model == "" {
		return
	}
	w := rt.Window("model:"+model, 600)
	w.Add(evt.Timestamp, 1, 0)
}

func (a *ModelRateLimitedResponses) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 300)
	thresholdCount := intParam(params, "thresholdCount", 10)
	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Upstream 429 throttling: %d responses / %ds for one model", thresholdCount, windowSec)
		if d := EvalCountInWindow(rt, target, windowSec, thresholdCount, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
