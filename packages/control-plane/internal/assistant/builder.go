package assistant

import (
	"encoding/json"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/runtime"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/skills"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// WebAgentDeps configures one per-caller, per-turn web agent. Per the kernel's
// isolation contract, a fresh agent is built for each turn/session bound to this
// caller's bearer + Memory/Store — never shared across callers.
type WebAgentDeps struct {
	CallerAuthorization string // the web user's full Authorization header ("Bearer ...")
	CallerSourceIP      string // the web user's RealIP, stamped on in-process self-calls for the audit actor (R3)
	CallerRequestID     string // the originating chat turn's X-Nexus-Request-Id, propagated to self-calls for audit correlation
	CPBaseURL           string // this CP (self-call target for admin tools)
	AIGatewayURL        string // AI Gateway base URL (inference)
	SystemVK            string // backend system VK for inference — NEVER sent to the browser
	Model               string // inference model slug
	IsProd              bool
	// Dispatcher is the CP echo router. When set, the agent's admin self-calls are
	// dispatched in-process (no loopback HTTP, unforgeable AI-initiated audit stamp —
	// R3 / #18b H1); inference still goes over the network. nil → the core.Client
	// uses a plain network transport (tests pointing CPBaseURL at an httptest server).
	Dispatcher http.Handler

	Memory  agent.MemoryStore
	Store   agent.SessionStore
	Session *agent.Session
	Files   fileStore // per-user/session sandbox; nil → no write_file/read_file tools

	// SituationCache + SituationKey memoize the per-turn situation snapshot per caller
	// (NFR-11): when both are set the kernel situation is wrapped so a rapid follow-up
	// turn reuses the snapshot instead of re-issuing the ~8 admin reads. nil cache →
	// no caching (every turn rebuilds, the pre-NFR-11 behavior).
	SituationCache *situationCache
	SituationKey   string

	OnText      func(string)
	OnReasoning func(string)
	OnToolStart func(name string, input []byte)
	OnToolEnd   func(name string, output []byte, isError bool)
	OnUsage     func(agent.ContextStats)
	OnNavigate  func(NavigateDirective)
	Confirm     agent.ConfirmFunc
	// Redactor scrubs tool output before it enters the prompt (§8 data governance).
	// nil → no redaction (pool-less tests); production wires the PII redactor.
	Redactor agent.Redactor
	// DisableBodyReads withholds the raw-body read tools entirely (§8 posture).
	DisableBodyReads bool
}

// BuildWebAgent assembles a read-only web agent for P2: gateway read/analyze tools
// only. The web profile leaves EnableSystem/EnableCanvas/EnableMitigate false, so
// run_command and the host-file/TUI tools are never reachable (binding invariant).
// Confirm is nil because no confirm-tier (write) tools are registered yet — write
// tools + the confirm gate land in P5.
func BuildWebAgent(d WebAgentDeps) (*agent.Agent, error) {
	env := core.Env{
		Name:             "web",
		CPBaseURL:        d.CPBaseURL,
		AIGatewayBaseURL: d.AIGatewayURL,
		IsProd:           d.IsProd,
	}
	// One client serves both roles: admin self-calls (ts = caller bearer) and
	// inference (ChatToolStream takes the system VK as an explicit arg). When a
	// Dispatcher (the CP router) is wired, the admin self-calls are dispatched
	// IN-PROCESS: no loopback HTTP hop, the audit AI-initiated stamp rides as an
	// unforgeable context value (not a wire header), and the originating user's IP is
	// preserved for the audit actor (R3 / #18b H1). Inference (a different host) still
	// goes over the real network. A nil Dispatcher → a plain network client (tests).
	var httpc *http.Client
	if d.Dispatcher != nil {
		httpc = &http.Client{Transport: newInProcessTransport(d.Dispatcher, d.CPBaseURL, d.CallerSourceIP, d.CallerRequestID, nil)}
	}
	client := core.NewClient(env, newBearerTokenSource(d.CallerAuthorization), httpc)

	skillSet, err := skills.Load("")
	if err != nil {
		return nil, err
	}
	// Web profile: gateway tools (read + analyze + mitigate writes) + canvas
	// (navigation). Mitigate writes are confirm-tier — the kernel gates them through
	// d.Confirm (the web two-phase confirm). Still NO system tools (run_command).
	canvas := newWebCanvas(d.OnNavigate)
	reg := runtime.NewAgentRegistry(client, canvas, runtime.AgentOptions{EnableCanvas: true, EnableMitigate: true})
	// Web file sandbox (P7): spillstore-backed write_file/read_file, isolated to this
	// caller's userId — distinct from the CLI's local-filesystem system tools, which
	// stay disabled on web. Registered only when a backing store is wired.
	if d.Files != nil {
		reg.Register(writeFileTool{fs: d.Files})
		reg.Register(readFileTool{fs: d.Files})
	}
	// §8 governance posture: a deployment may withhold the raw-body read tools so
	// the assistant cannot reach raw traffic bodies / admin records at all. Removed
	// AFTER registration so they are neither advertised to the model nor callable.
	if d.DisableBodyReads {
		for _, name := range bodyReadToolNames {
			reg.Remove(name)
		}
	}
	model := runtime.NewModel(client, d.SystemVK, d.Model)
	var situation agent.SituationProvider = runtime.NewSituation(client)
	// NFR-11: memoize the ~8-call situation snapshot per caller across a session's
	// turns. Keyed by the authenticated userId so no IAM-scoped view leaks across
	// principals; absent cache → unchanged (rebuild every turn).
	if d.SituationCache != nil && d.SituationKey != "" {
		situation = cachedSituation{inner: situation, cache: d.SituationCache, key: d.SituationKey}
	}
	gate := agent.NewGate(agent.NewCommandClassifier(), nil, false)
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
		Skills:      skillSet,
		Memory:      d.Memory,
		Store:       d.Store,
		Compactor:   compactor,
		Situation:   situation,
		Session:     d.Session,
		Env:         "web",
		Surface:     "web", // FR-20: web-flavored system-prompt persona (no "cockpit"/"file-backed"/"drift")
		IsProd:      d.IsProd,
		Confirm:     d.Confirm,  // two-phase web confirm gate (P5); nil declines all writes
		Redactor:    d.Redactor, // §8: scrub PII from tool output before prompt entry (when body reads enabled)
		OnText:      d.OnText,
		OnReasoning: d.OnReasoning,
		OnContext:   d.OnUsage,
	}
	if d.OnToolStart != nil {
		cfg.OnToolStart = func(name string, input json.RawMessage) { d.OnToolStart(name, []byte(input)) }
	}
	if d.OnToolEnd != nil {
		cfg.OnToolEnd = func(name string, output json.RawMessage, isError bool) { d.OnToolEnd(name, []byte(output), isError) }
	}
	return agent.New(cfg), nil
}
