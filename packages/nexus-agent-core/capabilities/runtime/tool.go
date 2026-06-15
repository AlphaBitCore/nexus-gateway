package runtime

import (
	"context"
	"encoding/json"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// funcTool adapts a closure to agent.Tool so every concrete capability is one
// value (name/description/tier/schema/run) rather than a bespoke struct.
type funcTool struct {
	name   string
	desc   string
	tier   agent.Tier
	schema json.RawMessage
	run    func(ctx context.Context, input json.RawMessage) (agent.Result, error)
	// confirmDetail, when set on a confirm-tier tool, resolves the concrete write
	// (METHOD /path (operationId), or the named entity action) for the gate's Ask
	// reason — so the operator confirms an informed change. Optional.
	confirmDetail func(input json.RawMessage) string
	// impact, when set on a high-blast-radius confirm-tier tool, reads current state
	// and returns a structured impact preview (current → effect) for the confirm card
	// Read-only; may call the gateway, so it takes a ctx. Optional.
	impact func(ctx context.Context, input json.RawMessage) (any, error)
}

func (f *funcTool) Name() string        { return f.name }
func (f *funcTool) Description() string { return f.desc }
func (f *funcTool) Tier() agent.Tier    { return f.tier }

// ConfirmDetail implements agent.ConfirmDetailer when confirmDetail is set; an
// unset detailer returns "" so the gate falls back to its generic reason.
func (f *funcTool) ConfirmDetail(input json.RawMessage) string {
	if f.confirmDetail == nil {
		return ""
	}
	return f.confirmDetail(input)
}

// ImpactDetail implements agent.ImpactDetailer. An unset impact closure returns
// (nil, nil) — "no preview for this tool" — so only the tools that opt in (the
// high-blast-radius mitigations) surface an impact card.
func (f *funcTool) ImpactDetail(ctx context.Context, input json.RawMessage) (any, error) {
	if f.impact == nil {
		return nil, nil
	}
	return f.impact(ctx, input)
}

func (f *funcTool) Schema() json.RawMessage {
	if f.schema == nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return f.schema
}

func (f *funcTool) Run(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	return f.run(ctx, input)
}
