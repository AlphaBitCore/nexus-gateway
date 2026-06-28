package runtime

import (
	"context"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// Canvas is the seam the agent drives to operate the TUI cockpit: open a view
// (optionally filtered), show a specific event, or highlight a row. The Layer-3
// TUI implements it by emitting Bubble Tea messages; Layer 2 ships the tools +
// a test double. All methods are synchronous and return an error the tool
// surfaces as a recoverable tool_result (never a hang — design §6 autonomy).
type Canvas interface {
	Navigate(view string, filter core.TrafficFilter) error
	ShowEvent(id string) error
	Highlight(ref string) error
}

// canvasTools builds the navigate / show_event / highlight auto-tier tools.
func canvasTools(c Canvas) []agent.Tool {
	return []agent.Tool{
		&funcTool{name: "navigate", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"view":{"type":"string","description":"the view to open: overview, radar, cost, slo, nodes, alerts, compliance, jobs, sync, models, keys, rules, kill"},"status":{"type":"string","description":"optional traffic status filter: 4xx, 5xx, error"},"model":{"type":"string","description":"optional model-slug filter"}},"required":["view"]}`),
			desc:   "Open a view in the operator cockpit, optionally filtered. Use this to show the human what you are looking at as you reason.",
			run: func(_ context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					View   string `json:"view"`
					Status string `json:"status"`
					Model  string `json:"model"`
				}
				_ = json.Unmarshal(in, &a)
				if a.View == "" {
					return errResult("view is required"), nil
				}
				if err := c.Navigate(a.View, core.TrafficFilter{StatusRange: a.Status, ModelUsed: a.Model}); err != nil {
					return errResult("could not open %q: %s", a.View, err), nil
				}
				return agent.Result{Content: "opened " + a.View}, nil
			}},

		&funcTool{name: "show_event", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
			desc:   "Open the event drill-down for a specific traffic event id in the cockpit.",
			run: func(_ context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(in, &a)
				if a.ID == "" {
					return errResult("id is required"), nil
				}
				if err := c.ShowEvent(a.ID); err != nil {
					return errResult("could not show event %q: %s", a.ID, err), nil
				}
				return agent.Result{Content: "showing event " + a.ID}, nil
			}},

		&funcTool{name: "highlight", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string","description":"a row reference in the active view (event id, provider name, node id, etc.)"}},"required":["ref"]}`),
			desc:   "Highlight a row in the active view to draw the human's attention.",
			run: func(_ context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Ref string `json:"ref"`
				}
				_ = json.Unmarshal(in, &a)
				if a.Ref == "" {
					return errResult("ref is required"), nil
				}
				if err := c.Highlight(a.Ref); err != nil {
					return errResult("could not highlight %q: %s", a.Ref, err), nil
				}
				return agent.Result{Content: "highlighted " + a.Ref}, nil
			}},
	}
}
