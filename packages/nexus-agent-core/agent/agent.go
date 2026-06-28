package agent

import (
	"context"
	"github.com/goccy/go-json"
	"strings"
)

// Config wires the kernel units into an Agent. Model, Registry, Gate,
// Memory, Store, Compactor, Situation, and Session are required.
//
// CONCURRENCY & ISOLATION CONTRACT (load-bearing for the web face): an Agent is
// NOT concurrency-safe and is scoped to a single conversation/caller — Turn
// mutates Session without locking. Memory and Store are seams whose
// IMPLEMENTATION owns per-user isolation: the CLI binds file-backed single-user
// impls; the web MUST bind impls scoped to the calling user (a per-caller
// instance, or an impl that derives the user from its own construction). A web
// backend therefore builds ONE Agent (with that caller's Memory/Store/Session)
// per turn/session — sharing an Agent, or a user-bound Memory/Store, across
// callers would cross-contaminate sessions and memory.
type Config struct {
	Model     Model
	Registry  *Registry
	Gate      *Gate
	Memory    MemoryStore
	Store     SessionStore
	Compactor *Compactor
	Situation SituationProvider
	Session   *Session

	Env    string
	IsProd bool
	// Surface is the face the agent runs behind ("web" for the web assistant, ""
	// for the CLI/TUI). It only re-words the system-prompt persona; it never
	// changes capabilities or safety. See PromptInput.Surface.
	Surface string

	// Confirm authorizes confirm-tier tools (TUI binds it; nil declines all).
	Confirm ConfirmFunc
	// OnText / OnReasoning / OnToolStart stream progress to the UI (optional).
	// OnReasoning carries the model's thinking channel for distinct display; it
	// is never persisted to the transcript.
	OnText      func(string)
	OnReasoning func(string)
	OnToolStart func(name string, input json.RawMessage)
	OnToolEnd   func(name string, output json.RawMessage, isError bool)
	// OnContext reports the context-usage stats after each turn (optional), for the
	// conversation pane's context indicator.
	OnContext func(ContextStats)
	// OnCompact reports that the transcript was compacted (auto, at a turn's start, or
	// manual via /compact), so the UI can surface a visible "context compacted" notice
	// the same way it announces a tool call (optional).
	OnCompact func(CompactStat)

	// Redactor, when non-nil, scrubs each tool result BEFORE it enters the
	// conversation the model sees (and the UI peek). Host-injected: the web
	// assistant binds a PII redactor so raw traffic bodies (request/response
	// content returned by observe_traffic_event / resource_read) are scrubbed
	// before prompt entry — a data-governance requirement. The CLI leaves it nil
	// (passthrough): the operator sees real data in their own terminal.
	Redactor Redactor

	// Domain is standing per-host concept text appended to the system prompt's
	// domain-context section every turn (static — prompt-cache friendly); empty
	// = no section.
	Domain string
}

// Redactor scrubs tool output before it enters the prompt. Implementations must
// be safe for concurrent use is NOT required — the loop calls it serially on the
// loop goroutine after the parallel tool batch joins. The kernel owns no PII
// rules; the host supplies the policy (the web assistant bridges to the
// product's pii-detector).
type Redactor interface {
	// RedactToolOutput returns text with PII scrubbed. toolName lets an
	// implementation scope redaction (e.g. only body-bearing tools). It must
	// return the input unchanged when there is nothing to redact.
	RedactToolOutput(toolName, text string) string
}

// DefaultStepCap bounds the tool rounds in one turn. An operator turn routinely
// chains many rounds — resource_search → resource_describe → resource_read across
// several resources, plus the analytics/observe tools — so the cap must be generous
// enough that a normal multi-resource investigation finishes in one turn (the old
// cap of 8 cut real "show me hooks, the pipeline, and the routing rules" tasks off
// mid-stream). It stays bounded as a runaway-cost guard; on overflow the loop
// returns ErrStepCap and the conversation invites the operator to continue.
const DefaultStepCap = 40

// Agent is the kernel's public entry point: it drives one conversation, taking
// user turns to completion through the loop with full context assembly,
// compaction, memory, and session persistence.
type Agent struct {
	cfg     Config
	loop    *Loop
	Memory  MemoryStore
	Store   SessionStore
	Session *Session
}

// New builds an Agent and registers the kernel builtin memory tools so the model
// can recall/remember/update/forget durable facts.
func New(cfg Config) *Agent {
	cfg.Registry.Register(newRecallTool(cfg.Memory))
	cfg.Registry.Register(newRememberTool(cfg.Memory))
	cfg.Registry.Register(newUpdateMemoryTool(cfg.Memory))
	cfg.Registry.Register(newForgetTool(cfg.Memory))
	loop := &Loop{
		Model:       cfg.Model,
		Registry:    cfg.Registry,
		Gate:        cfg.Gate,
		StepCap:     DefaultStepCap,
		Confirm:     cfg.Confirm,
		Compactor:   cfg.Compactor,
		OnCompact:   cfg.OnCompact,
		OnText:      cfg.OnText,
		OnReasoning: cfg.OnReasoning,
		OnToolStart: cfg.OnToolStart,
		OnToolEnd:   cfg.OnToolEnd,
		Redactor:    cfg.Redactor,
	}
	return &Agent{cfg: cfg, loop: loop, Memory: cfg.Memory, Store: cfg.Store, Session: cfg.Session}
}

// ToolNames returns the names of every tool this agent can invoke. Callers use
// it to classify a model-emitted tool name — an unrecognized name must collapse
// to a single "unknown" bucket so a hallucinated or adversarially-prompted tool
// name can never become an unbounded metric label or sink key.
func (a *Agent) ToolNames() []string {
	return a.loop.Registry.Names()
}

// Turn takes one user message to completion. It assembles the system prompt and the
// per-turn context bundle (situation + memory + active view), runs the loop (which
// bounds the model-facing conversation deterministically each round), persists the
// turn's new messages, and returns the assistant's final text. A non-nil error (e.g.
// the step cap) is returned alongside whatever text was produced.
func (a *Agent) Turn(ctx context.Context, userText, activeView string) (string, error) {
	full := a.Session.Messages

	memText, _ := a.Memory.Index()
	bundle, _ := AssembleContext(ctx, a.cfg.Situation, memText, activeView)

	system := BuildSystemPrompt(PromptInput{
		Env:           a.cfg.Env,
		IsProd:        a.cfg.IsProd,
		Surface:       a.cfg.Surface,
		DomainContext: a.cfg.Domain,
	})

	// The context bundle rides as a leading user-context block THIS turn only — the
	// model sees a fresh situation, but it is never persisted (see below).
	userMsg := Message{Role: RoleUser, Blocks: []Block{
		{Type: BlockText, Text: bundle},
		{Type: BlockText, Text: userText},
	}}

	// Run returns the NEW messages produced this turn; the loop bounds the model view
	// each round so the model never overflows the window even on a tool-heavy turn,
	// while the persisted session keeps the full transcript (design §5.11).
	produced, usage, err := a.loop.Run(ctx, system, full, userMsg)
	// Strip the ephemeral situation bundle from the stored user message: keeping it out
	// of history means the system+history prefix stays byte-stable turn-over-turn (so
	// the gateway's prompt cache keeps hitting) and the transcript isn't bloated by a
	// frozen snapshot repeated on every past turn. The live snapshot is re-injected
	// fresh each turn above.
	if len(produced) > 0 && produced[0].Role == RoleUser {
		clean := Message{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: userText}}}
		produced = append([]Message{clean}, produced[1:]...)
	}
	a.Session.Messages = append(full, produced...)
	// Persist even on a step-cap/loop error so the partial turn is resumable.
	_ = a.Store.Save(a.Session)

	// Report context usage for the indicator: exact used/cached from the gateway, plus
	// a calibrated per-component estimate over the BOUNDED model view (what the model
	// actually saw), and the trim budget.
	if a.cfg.OnContext != nil && usage != nil && usage.PromptTokens > 0 {
		modelView := a.Session.Messages
		if fitted, _ := a.cfg.Compactor.FitToWindow(modelView); fitted != nil {
			modelView = fitted
		}
		// Prepend the (ephemeral, unpersisted) bundle so the per-component split counts it.
		convForStats := append([]Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: bundle}}}}, modelView...)
		cs := contextStats(system, a.loop.exposedTools(), bundle, convForStats, usage)
		cs.Messages = len(a.Session.Messages)
		cs.CompactBudget = a.cfg.Compactor.trimBudget
		a.cfg.OnContext(cs)
	}
	// Auto-compaction: a clean turn whose ACTUAL prompt usage crossed the window
	// threshold durably compacts the session NOW (the same summarize-and-rewrite
	// the manual /compact runs, OnCompact notice included) instead of waiting for
	// a human — otherwise the deterministic trimmer re-elides the same overgrown
	// transcript every turn and the persisted session never shrinks. Best-effort:
	// a summary failure changes nothing (FitToWindow keeps bounding the view),
	// and an errored/cancelled turn skips it (its ctx may already be dead).
	if err == nil && usage != nil && a.cfg.Compactor.ShouldAutoCompact(usage.PromptTokens) {
		_, _, _ = a.Compact(ctx)
	}
	return finalText(produced), err
}

// Compact summarizes the live session's older transcript now, regardless of the
// auto-compaction budget — the manual /compact path. Unlike auto-compaction (which
// is in-window only, leaving the persisted transcript intact for resume), a manual
// compaction permanently rewrites the persisted session: the operator explicitly
// asked to free the context, so the summary replaces the older turns on disk too.
// It reports the CompactStat and whether it acted; a no-op (nothing safe to
// summarize) returns acted=false with no error.
func (a *Agent) Compact(ctx context.Context) (CompactStat, bool, error) {
	out, stat, err := a.cfg.Compactor.ForceCompact(ctx, a.Session.Messages)
	if err != nil {
		return CompactStat{}, false, err
	}
	if stat == nil {
		return CompactStat{}, false, nil
	}
	a.Session.Messages = out
	_ = a.Store.Save(a.Session)
	if a.cfg.OnCompact != nil {
		a.cfg.OnCompact(*stat)
	}
	return *stat, true, nil
}

// finalText returns the last assistant message's non-empty text, or "".
func finalText(conv []Message) string {
	for i := len(conv) - 1; i >= 0; i-- {
		if conv[i].Role == RoleAssistant {
			if t := strings.TrimSpace(conv[i].Text()); t != "" {
				return t
			}
		}
	}
	return ""
}
