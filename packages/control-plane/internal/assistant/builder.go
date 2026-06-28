package assistant

import (
	"github.com/goccy/go-json"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/initiator"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/selfdispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/subagentmark"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/runtime"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// newCallerCPClient is the single construction chokepoint for a CALLER-
// authenticated CP client — the web agent's admin self-calls build through it,
// so the credential + transport semantics can never diverge. When a dispatcher
// (the CP echo router) is wired, admin self-calls are dispatched IN-PROCESS:
// no loopback HTTP hop, the audit AI-initiated stamp rides as an unforgeable
// context value (not a wire header), and the originating user's IP is
// preserved for the audit actor. Inference (a different host) still goes over
// the real network. A nil dispatcher → a plain network client (tests pointing
// CPBaseURL at an httptest server).
func newCallerCPClient(env core.Env, dispatcher http.Handler, authorization, sourceIP, requestID string) *core.Client {
	var httpc *http.Client
	if dispatcher != nil {
		httpc = &http.Client{Transport: selfdispatch.New(selfdispatch.Config{
			Handler:   dispatcher,
			CPBaseURL: env.CPBaseURL,
			Initiator: initiator.ViaAssistant,
			SourceIP:  sourceIP,
			RequestID: requestID,
		})}
	}
	return core.NewClient(env, newBearerTokenSource(authorization), httpc)
}

// WebAgentDeps configures one per-caller, per-turn web agent. Per the kernel's
// isolation contract, a fresh agent is built for each turn/session bound to this
// caller's bearer + Memory/Store — never shared across callers.
type WebAgentDeps struct {
	CallerAuthorization string // the web user's full Authorization header ("Bearer ...")
	CallerSourceIP      string // the web user's RealIP, stamped on in-process self-calls for the audit actor
	CallerRequestID     string // the originating chat turn's X-Nexus-Request-Id, propagated to self-calls for audit correlation
	CPBaseURL           string // this CP (self-call target for admin tools)
	AIGatewayURL        string // AI Gateway base URL (inference)
	SystemVK            string // backend system VK for inference — NEVER sent to the browser
	Model               string // inference model slug
	IsProd              bool
	// Dispatcher is the CP echo router. When set, the agent's admin self-calls are
	// dispatched in-process (no loopback HTTP, unforgeable AI-initiated audit
	// stamp); inference still goes over the network. nil → the core.Client uses a
	// plain network transport (tests pointing CPBaseURL at an httptest server).
	Dispatcher http.Handler

	Memory  agent.MemoryStore
	Store   agent.SessionStore
	Session *agent.Session
	Files   fileStore // per-user/session sandbox; nil → no write_file/read_file tools

	// SituationCache + SituationKey memoize the per-turn situation snapshot per
	// caller: when both are set the kernel situation is wrapped so a rapid
	// follow-up turn reuses the snapshot instead of re-issuing the ~8 admin
	// reads. nil cache → no caching (every turn rebuilds).
	SituationCache *situationCache
	SituationKey   string

	OnText      func(string)
	OnReasoning func(string)
	OnToolStart func(name string, input []byte)
	OnToolEnd   func(name string, output []byte, isError bool)
	OnUsage     func(agent.ContextStats)
	// OnCompact reports a transcript compaction (auto or forced) so the web
	// surface can tell the user older turns were condensed — silence here
	// rewrites the persisted transcript invisibly.
	OnCompact  func(agent.CompactStat)
	OnNavigate func(NavigateDirective)
	Confirm    agent.ConfirmFunc
	// Redactor scrubs tool output before it enters the prompt (data governance).
	// nil → no redaction (pool-less tests); production wires the PII redactor.
	Redactor agent.Redactor
	// DisableBodyReads withholds the raw-body read tools entirely.
	DisableBodyReads bool
}

// BuildWebAgent assembles the per-caller, per-turn web agent. The web profile
// registers gateway read/analyze tools + canvas navigation + mitigate (write)
// tools, but deliberately leaves EnableSystem FALSE — so run_command and the
// host-filesystem / TUI tools are NEVER reachable from the web assistant
// (binding isolation invariant; see runtime.NewAgentRegistry below, which sets
// only EnableCanvas + EnableMitigate). The mitigate writes are confirm-tier: the
// kernel gates each one through d.Confirm (the web two-phase confirm); a nil
// d.Confirm declines all writes. Privilege is NOT enforced by a per-tool
// allow-list field on this struct — there is no EnabledAssistantTools knob; the
// boundary is (a) which capability groups the registry enables here, (b) the
// confirm gate on write-tier tools, and (c) every admin self-call being
// IAM-checked at the API the tool hits under the caller's own bearer.
func BuildWebAgent(d WebAgentDeps) *agent.Agent {
	env := core.Env{
		Name:             "web",
		CPBaseURL:        d.CPBaseURL,
		AIGatewayBaseURL: d.AIGatewayURL,
		IsProd:           d.IsProd,
	}
	// One client serves both roles: admin self-calls (ts = caller bearer) and
	// inference (ChatToolStream takes the system VK as an explicit arg).
	client := newCallerCPClient(env, d.Dispatcher, d.CallerAuthorization, d.CallerSourceIP, d.CallerRequestID)

	// Web profile: gateway tools (read + analyze + mitigate writes) + canvas
	// (navigation). Mitigate writes are confirm-tier — the kernel gates them through
	// d.Confirm (the web two-phase confirm). Still NO system tools (run_command).
	canvas := newWebCanvas(d.OnNavigate)
	reg := runtime.NewAgentRegistry(client, canvas, runtime.AgentOptions{EnableCanvas: true, EnableMitigate: true})
	// Web file sandbox: spillstore-backed write_file/read_file, isolated to this
	// caller's userId — distinct from the CLI's local-filesystem system tools, which
	// stay disabled on web. Registered only when a backing store is wired.
	if d.Files != nil {
		reg.Register(writeFileTool{fs: d.Files})
		reg.Register(readFileTool{fs: d.Files})
	}
	// Governance posture: a deployment may withhold the raw-body read tools so
	// the assistant cannot reach raw traffic bodies / admin records at all. Removed
	// AFTER registration so they are neither advertised to the model nor callable.
	if d.DisableBodyReads {
		for _, name := range bodyReadToolNames {
			reg.Remove(name)
		}
	}
	model := runtime.NewModel(client, d.SystemVK, d.Model)
	var situation agent.SituationProvider = runtime.NewSituation(client)
	// Memoize the ~8-call situation snapshot per caller across a session's
	// turns. Keyed by the authenticated userId so no IAM-scoped view leaks across
	// principals; absent cache → unchanged (rebuild every turn).
	if d.SituationCache != nil && d.SituationKey != "" {
		situation = cachedSituation{inner: situation, cache: d.SituationCache, key: d.SituationKey}
	}
	gate := agent.NewGate(agent.NewCommandClassifier(), nil, false)
	// subagent_dispatch: fan heavy or parallel sub-tasks out to isolated, capped,
	// tool-scoped children on the shared kernel chassis (the same tool the CLI
	// registers — runtime.NewSubagentDispatchTool). Children reuse THIS agent's
	// model (same core.Client → parent billing identity) and gate; the parent
	// ConfirmFunc threads through so a confirm-tier tool inside a single
	// (non-parallel) child bubbles to the web confirm surface. DecorateChild
	// stamps the audit sub-agent marker so a child's tool calls audit as "via
	// assistant ▸ subagent N". Registered last so children inherit the full
	// read/analyze/mitigate set minus this tool.
	reg.Register(runtime.NewSubagentDispatchTool(runtime.SubagentDispatchConfig{
		Parent:        reg,
		Model:         model,
		Gate:          gate,
		Confirm:       d.Confirm,
		DecorateChild: subagentmark.With,
	}))
	// Size the token-budget compactor to the model's context window. The web handler
	// deals in model slugs (allowedModels) and has no per-model window metadata at
	// hand, so we pass 0 ("unknown") — NewCompactor then applies its conservative
	// default window (fallbackContextWindow), which compacts no later than the real
	// (larger) window would, so the model view never overflows. Threading a real
	// window here would mean an AdminModels catalog lookup on the chat hot path.
	compactor := agent.NewCompactor(model, 0)

	cfg := agent.Config{
		Model:       model,
		Registry:    reg,
		Gate:        gate,
		Memory:      d.Memory,
		Store:       d.Store,
		Compactor:   compactor,
		Situation:   situation,
		Session:     d.Session,
		Env:         "web",
		Surface:     "web", // web-flavored system-prompt persona (no "cockpit"/"file-backed"/"drift")
		IsProd:      d.IsProd,
		Confirm:     d.Confirm,  // two-phase web confirm gate; nil declines all writes
		Redactor:    d.Redactor, // scrub PII from tool output before prompt entry (when body reads enabled)
		OnText:      d.OnText,
		OnReasoning: d.OnReasoning,
		OnContext:   d.OnUsage,
		OnCompact:   d.OnCompact,
	}
	if d.OnToolStart != nil {
		cfg.OnToolStart = func(name string, input json.RawMessage) { d.OnToolStart(name, []byte(input)) }
	}
	if d.OnToolEnd != nil {
		cfg.OnToolEnd = func(name string, output json.RawMessage, isError bool) { d.OnToolEnd(name, []byte(output), isError) }
	}
	return agent.New(cfg)
}
