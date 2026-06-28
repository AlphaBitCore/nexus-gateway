package runtime

import (
	"context"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// AgentOptions configures the in-process agent registry profile.
type AgentOptions struct {
	EnableMitigate bool
	// EnableCanvas registers the TUI navigation canvas tools. CLI/TUI only.
	EnableCanvas bool
	// EnableSystem registers the local shell/file tools (run_command/read_file/
	// write_file), which execute on the HOST. CLI/TUI ONLY — a web/server profile
	// MUST leave this false (binding invariant: no run_command on the web face;
	// file ops there are an S3 sandbox, never the host FS). run_command is also
	// TierAuto (no confirm gate), so leaking it server-side is a direct RCE.
	EnableSystem bool
	VKSecret     string
}

// NewAgentRegistry builds the in-process agent's registry: the gateway tools,
// plus the canvas tools (TUI navigation) when opts.EnableCanvas, plus the local
// system tools (shell/file) when opts.EnableSystem. Canvas + system are CLI/TUI
// ONLY: a web/server profile leaves both false so the model can never reach
// run_command/host-file tools (binding invariant). The kernel's memory
// builtins (recall/remember/update_memory/forget) are added by agent.New, not here.
func NewAgentRegistry(gw Gateway, canvas Canvas, opts AgentOptions) *agent.Registry {
	reg := agent.NewRegistry()
	for _, t := range gatewayTools(gw, opts.VKSecret, opts.EnableMitigate) {
		reg.Register(t)
	}
	if opts.EnableCanvas && canvas != nil {
		for _, t := range canvasTools(canvas) {
			reg.Register(t)
		}
	}
	if opts.EnableSystem {
		for _, t := range systemTools(newOSRunner()) {
			reg.Register(t)
		}
	}
	return reg
}

// AgentDeps are the inputs BuildAgent needs to assemble a live agent.Agent. The
// Streamer/Gateway/Canvas/Confirm seams let Layer 3 (or tests) inject fakes.
type AgentDeps struct {
	Streamer chatStreamer
	Gateway  Gateway
	Canvas   Canvas
	Confirm  agent.ConfirmFunc

	VKSecret string
	Model    string
	Env      string
	IsProd   bool

	// ContextWindow is the selected model's max context tokens (0 = unknown). It sizes
	// the compactor's token budget so auto-compaction keeps the prompt under the window.
	ContextWindow int

	MemoryDir  string // ~/.config/nexus/memory (scope-split global/ + <env>/ inside)
	SessionDir string // ~/.config/nexus/sessions/<env>

	// Session is the conversation to continue: the agent appends its turns to
	// this transcript and persists under its id (resuming a past conversation).
	// Nil starts a fresh empty session.
	Session *agent.Session

	EnableMitigate bool
	AllowList      []string // pre-approved command/path patterns
	Yolo           bool

	OnText      func(string)
	OnReasoning func(string)
	OnToolStart func(name string, input []byte)
	OnToolEnd   func(name string, output []byte, isError bool)
	OnContext   func(agent.ContextStats)
	OnCompact   func(agent.CompactStat)
}

// BuildAgent wires the concrete capabilities into the kernel and returns a ready
// agent.Agent. It opens memory + the session store, builds the agent-profile
// registry, and constructs the gateway-backed Model + Situation.
func BuildAgent(_ context.Context, d AgentDeps) (*agent.Agent, error) {
	mem := agent.OpenMemoryStore(d.MemoryDir, d.Env)
	store := agent.OpenStoreAt(d.SessionDir)
	session := d.Session
	if session == nil {
		session = agent.NewSession(d.Env)
	}
	reg := NewAgentRegistry(d.Gateway, d.Canvas, AgentOptions{
		EnableMitigate: d.EnableMitigate, EnableCanvas: true, EnableSystem: true, VKSecret: d.VKSecret,
	})
	model := NewModel(d.Streamer, d.VKSecret, d.Model)
	situation := NewSituation(d.Gateway)
	gate := agent.NewGate(agent.NewCommandClassifier(), d.AllowList, d.Yolo)
	// subagent_dispatch: fan heavy/parallel sub-tasks out to isolated, capped
	// children on the shared kernel chassis. Children reuse THIS agent's model
	// (same billing identity) + gate; the host ConfirmFunc threads through for
	// single-dispatch confirm bubbling. Registered last so children inherit the full
	// tool set minus this tool. The CLI/Agent surface wires no audit decorator
	// (a control-plane concern).
	reg.Register(NewSubagentDispatchTool(SubagentDispatchConfig{Parent: reg, Model: model, Gate: gate, Confirm: d.Confirm}))
	compactor := agent.NewCompactor(model, d.ContextWindow) // token budget sized to the model window

	cfg := agent.Config{
		Model: model, Registry: reg, Gate: gate,
		Memory: mem, Store: store, Compactor: compactor, Situation: situation,
		Session: session, Env: d.Env, IsProd: d.IsProd, Confirm: d.Confirm,
		OnText: d.OnText, OnReasoning: d.OnReasoning, OnContext: d.OnContext, OnCompact: d.OnCompact,
	}
	if d.OnToolStart != nil {
		cfg.OnToolStart = func(name string, input json.RawMessage) { d.OnToolStart(name, []byte(input)) }
	}
	if d.OnToolEnd != nil {
		cfg.OnToolEnd = func(name string, output json.RawMessage, isError bool) { d.OnToolEnd(name, []byte(output), isError) }
	}
	return agent.New(cfg), nil
}
