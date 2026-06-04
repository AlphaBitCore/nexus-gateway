package runtime

import (
	"context"
	"encoding/json"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/skills"
)

// MCPOptions configures the remote (MCP) registry profile.
type MCPOptions struct {
	EnableMitigate bool
	VKSecret       string // VK the simulate tool forwards under
}

// NewMCPRegistry builds the registry exposed by `nexus mcp serve`: gateway tools
// only (read/analyze/simulate, plus mitigate iff EnableMitigate). No canvas or
// system tools — a remote MCP client has no interactive permission gate, so
// local-operator tools are intentionally absent (see plan §0.2.1).
func NewMCPRegistry(gw Gateway, opts MCPOptions) *agent.Registry {
	reg := agent.NewRegistry()
	for _, t := range gatewayTools(gw, opts.VKSecret, opts.EnableMitigate) {
		reg.Register(t)
	}
	return reg
}

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
// run_command/host-file tools (binding invariant). The kernel's
// use_skill/remember/forget builtins are added by agent.New, not here.
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

	SkillDir   string // "" => built-ins only
	MemoryDir  string // ~/.config/nexus/memory (scope-split global/ + <env>/ inside)
	SessionDir string // ~/.config/nexus/sessions/<env>

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
// agent.Agent. It loads skills, opens memory + the session store, builds the
// agent-profile registry, and constructs the gateway-backed Model + Situation.
func BuildAgent(_ context.Context, d AgentDeps) (*agent.Agent, error) {
	skillSet, err := skills.Load(d.SkillDir)
	if err != nil {
		return nil, err
	}
	mem := agent.OpenMemoryStore(d.MemoryDir, d.Env)
	store := agent.OpenStoreAt(d.SessionDir)
	session := agent.NewSession(d.Env)
	reg := NewAgentRegistry(d.Gateway, d.Canvas, AgentOptions{
		EnableMitigate: d.EnableMitigate, EnableCanvas: true, EnableSystem: true, VKSecret: d.VKSecret,
	})
	model := NewModel(d.Streamer, d.VKSecret, d.Model)
	situation := NewSituation(d.Gateway)
	gate := agent.NewGate(agent.NewCommandClassifier(), d.AllowList, d.Yolo)
	compactor := agent.NewCompactor(model, d.ContextWindow) // token budget sized to the model window

	cfg := agent.Config{
		Model: model, Registry: reg, Gate: gate, Skills: skillSet,
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
