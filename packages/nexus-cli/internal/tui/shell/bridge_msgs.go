package shell

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	capabilities "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/runtime"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// bridge_msgs.go — the bridge's wire vocabulary: the CLI-facing build seam
// (AgentRunner / AgentStream / AgentBuildFunc) and the Bubble Tea messages the
// bridge pumps into the loop. Split from bridge.go along the contract seam so
// bridge.go holds only the goroutine/channel machinery.

// AgentRunner is the bridge's view of a built agent. *agent.Agent satisfies it;
// tests inject a fake so the bridge plumbing runs without a model. It is exported
// so the CLI's BuildAgent seam (cli/root.go) can name it as the return type.
type AgentRunner interface {
	Turn(ctx context.Context, userText, activeView string) (string, error)
	// Compact summarizes the session's older transcript now (the manual /compact
	// path). It reports the stat, whether it acted (false = nothing to compact), and
	// any error.
	Compact(ctx context.Context) (agent.CompactStat, bool, error)
}

// AgentStream bundles the streaming callbacks the TUI hands the CLI's agent
// builder — one struct instead of a positional list, so a new channel (a run
// progress tap, a richer prompt seam) extends the bundle without re-threading
// every implementor.
type AgentStream struct {
	OnText      func(delta string)
	OnReasoning func(delta string)
	OnToolStart func(name string, input []byte)
	OnToolEnd   func(name string, output []byte, isError bool)
	OnContext   func(stats agent.ContextStats, window int)
	OnCompact   func(stat agent.CompactStat)
}

// AgentBuildFunc is the seam the CLI implements to construct the gateway agent
// the conversation drives. The TUI owns the bridge and hands the CLI the canvas
// (view-driving), the blocking confirm gate, the streaming callbacks, and the
// session to resume (nil = start fresh; non-nil = the /sessions pick, so the
// next turn appends to that same persisted session); the CLI supplies what only
// it knows (model / VK / env / on-disk memory + session paths) and calls
// capabilities.BuildAgent. A nil seam disables the conversation.
type AgentBuildFunc func(canvas capabilities.Canvas, confirm agent.ConfirmFunc, stream AgentStream, resume *agent.Session) (AgentRunner, error)

// --- messages the bridge emits into the Bubble Tea loop ---

// agentTextMsg is one streamed assistant text delta.
type agentTextMsg struct{ delta string }

// agentReasoningMsg is one streamed reasoning/thinking delta — display-only, shown
// in a distinct dim style and never persisted to the agent transcript.
type agentReasoningMsg struct{ delta string }

// agentToolMsg announces a tool call starting (name + input args).
type agentToolMsg struct {
	name  string
	input []byte
}

// agentToolResultMsg reports a tool call's result (raw output + error flag), for the
// transcript's dim result peek + the expandable full I/O.
type agentToolResultMsg struct {
	name    string
	output  []byte
	isError bool
}

// agentNavMsg / agentShowMsg / agentHighlightMsg are canvas drives: the agent
// asked to open a view / show an event / highlight a row. The root model applies
// them so the cockpit follows the agent live.
type agentNavMsg struct {
	view   string
	filter core.TrafficFilter
}
type agentShowMsg struct{ id string }
type agentHighlightMsg struct{ ref string }

// agentConfirmMsg asks the human to authorize a mitigation. The root model raises
// the confirm gate; the reply rides back on the bridge's reply channel.
type agentConfirmMsg struct {
	tool   string
	reason string
}

// contextStatsMsg carries the post-turn context-usage stats + the current model's
// context window to the conversation pane's context indicator.
type contextStatsMsg struct {
	stats  agent.ContextStats
	window int
}

// agentCompactMsg announces a compaction so the conversation can surface a visible
// notice (like a tool call) and refresh the context indicator. acted=false carries
// no stat — it is the manual /compact "nothing to compact" outcome.
type agentCompactMsg struct {
	stat  agent.CompactStat
	acted bool
}

// agentDoneMsg is the terminal result of one Turn (or a manual /compact).
type agentDoneMsg struct {
	final string
	err   error
}
