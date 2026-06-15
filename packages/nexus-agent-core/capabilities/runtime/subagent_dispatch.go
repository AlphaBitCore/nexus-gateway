package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// subagent_dispatch lets the assistant fan a heavy or
// parallelizable mandate out to isolated, capped, tool-scoped child agents that
// report back summaries — keeping the main conversation clean and every LLM
// touchpoint behind a deterministic boundary (the harness thesis). Each child
// runs on the single kernel chassis (agent.RunSubagent); this file is the shared
// chat-surface wiring (web + CLI both register it): input parsing, registry
// curation, the parallel/single tier policy, and the summary contract.
//
// It lives in capabilities/runtime, not a service package, precisely so the web
// assistant (control-plane) and the CLI (nexus-cli) register the SAME tool over
// the SAME chassis. Surface-specific concerns ride as optional config hooks: the
// control-plane wires DecorateChild to stamp the audit sub-agent marker and
// the CLI leaves it nil.

// Subagent dispatch caps. They live here, with the
// shared tool, rather than in a single service's registry because both the web
// and the CLI surface construct this tool; a service-local constant could not be
// reached by the other module. They are fixed product policy (no admin knob);
// SubagentDispatchConfig lets tests pass smaller values.
const (
	// SubagentFanOutMax caps the number of tasks (and, in parallel mode, the
	// concurrent children) in one dispatch. A turn that needs more splits across
	// dispatches.
	SubagentFanOutMax = 4
	// SubagentMaxTurns bounds a chat-dispatched child's tool rounds.
	SubagentMaxTurns = 12
	// SubagentMaxTokens bounds a chat-dispatched child's cumulative token spend.
	SubagentMaxTokens = 120_000
	// SubagentDeadline bounds a chat-dispatched child's wall-clock lifetime.
	SubagentDeadline = 3 * time.Minute
)

// childSystemPrompt is the curated system prompt every chat-dispatched child
// starts from. It is deliberately self-contained: the child never sees the
// parent's session, so the prompt must state the whole contract — finish the one
// task, then emit a self-contained summary, because that summary is the ONLY
// thing the parent (and the user) ever see (child transcripts are not persisted).
const childSystemPrompt = "You are a focused sub-agent dispatched by the main Nexus assistant to complete ONE " +
	"specific task and report back. You have a limited tool set and a hard turn/token budget. " +
	"Do the task efficiently, then write a clear, self-contained final summary of what you found or did. " +
	"That summary is the ONLY thing returned to the main assistant — it cannot see your intermediate steps, " +
	"so include every fact, number, and caveat the main assistant needs. Do not ask the user questions; " +
	"if you cannot complete the task, say so plainly and summarize what you learned."

// SubagentDispatchConfig parameterizes the shared dispatch tool. The required
// fields (Parent/Model/Gate) wire it to the host agent's tool set + inference path
// (children reuse the SAME model → the host's billing identity, T6) and permission
// gate. The optional hooks adapt it per surface without forking the implementation.
type SubagentDispatchConfig struct {
	Parent  *agent.Registry   // the host agent's full registry; children get a pruned subset
	Model   agent.Model       // shared inference path → host billing identity
	Gate    *agent.Gate       // shared permission gate (confirm-tier classification)
	Confirm agent.ConfirmFunc // host confirm, for single-dispatch bubbling

	// DecorateChild, when set, derives the per-child run context from the parent's
	// (used by the control-plane to stamp the audit sub-agent marker). nil →
	// the child runs on the unmodified context.
	DecorateChild func(ctx context.Context, label string) context.Context

	// Caps (0 → the package default). Production leaves them zero; tests pass small
	// values to exercise the cap paths.
	FanOutMax int
	MaxTurns  int
	MaxTokens int
	Deadline  time.Duration
}

// SubagentDispatchTool is the registered chat tool wrapping the kernel chassis.
type SubagentDispatchTool struct{ cfg SubagentDispatchConfig }

// NewSubagentDispatchTool returns the dispatch tool, filling unset caps from the
// package defaults.
func NewSubagentDispatchTool(cfg SubagentDispatchConfig) SubagentDispatchTool {
	if cfg.FanOutMax <= 0 {
		cfg.FanOutMax = SubagentFanOutMax
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = SubagentMaxTurns
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = SubagentMaxTokens
	}
	if cfg.Deadline <= 0 {
		cfg.Deadline = SubagentDeadline
	}
	return SubagentDispatchTool{cfg: cfg}
}

type dispatchInput struct {
	Tasks    []dispatchTask `json:"tasks"`
	Parallel bool           `json:"parallel"`
}

type dispatchTask struct {
	Task  string   `json:"task"`
	Tools []string `json:"tools,omitempty"`
}

// dispatchResult is one child's report in the tool's JSON output. Status is the
// typed halt narrative (a parent reading only summaries must still be
// able to tell a clean completion from a capped/denied/timed-out one).
type dispatchResult struct {
	Index   int              `json:"index"`
	Task    string           `json:"task"`
	Status  string           `json:"status"`
	Turns   int              `json:"turns"`
	Usage   dispatchUsage    `json:"usage"`
	Summary string           `json:"summary"`
	Denials []dispatchDenial `json:"denials,omitempty"`
}

type dispatchUsage struct {
	TotalTokens int `json:"totalTokens"`
}

type dispatchDenial struct {
	Tool string `json:"tool"`
	Kind string `json:"kind"` // "user" | "timeout"
}

type dispatchOutput struct {
	Parallel   bool             `json:"parallel"`
	Dispatched int              `json:"dispatched"`
	TotalUsage dispatchUsage    `json:"totalUsage"`
	Results    []dispatchResult `json:"results"`
}

func (SubagentDispatchTool) Name() string { return agent.DispatchToolName }

func (SubagentDispatchTool) Description() string {
	return "Dispatch one or more focused sub-agents to handle heavy or parallel sub-tasks in isolation. " +
		"Each runs with its own tool subset and a hard turn/token budget, and reports back ONLY a summary " +
		"(you cannot see its steps). Use for: parallel investigations across resources, or a heavy multi-step " +
		"sub-task you want kept out of the main thread. Args: tasks (each with a task description and optional " +
		"tools subset), parallel. In parallel mode every child is restricted to read-only (auto-tier) tools."
}

func (SubagentDispatchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"tasks":{"type":"array","minItems":1,"items":{"type":"object","properties":{` +
		`"task":{"type":"string","description":"the self-contained mandate for this sub-agent"},` +
		`"tools":{"type":"array","items":{"type":"string"},"description":"optional subset of your own tools to grant; omit to inherit all your eligible tools"}` +
		`},"required":["task"]}},` +
		`"parallel":{"type":"boolean","description":"run the tasks concurrently (read-only tools only) instead of one after another"}` +
		`},"required":["tasks"]}`)
}

// Tier is auto: dispatching is itself a read-like control action. Any production
// side effect happens inside a child, where that child's own confirm-tier tool
// bubbles to the host ConfirmFunc (single dispatch only) — never silently.
func (SubagentDispatchTool) Tier() agent.Tier { return agent.TierAuto }

func (t SubagentDispatchTool) Run(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	var in dispatchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return dispatchErr("invalid input: " + err.Error()), nil
	}
	if len(in.Tasks) == 0 {
		return dispatchErr("dispatch requires at least one task"), nil
	}
	if len(in.Tasks) > t.cfg.FanOutMax {
		return dispatchErr(fmt.Sprintf("too many tasks (%d); a single dispatch may fan out to at most %d sub-agents — split into multiple dispatches",
			len(in.Tasks), t.cfg.FanOutMax)), nil
	}

	// Build each child's curated registry up front so a tier/unknown-tool violation
	// fails the WHOLE dispatch before any child starts (no half-run fan-out).
	regs := make([]*agent.Registry, len(in.Tasks))
	for i, task := range in.Tasks {
		if strings.TrimSpace(task.Task) == "" {
			return dispatchErr(fmt.Sprintf("task %d has an empty task description", i+1)), nil
		}
		reg, err := t.childRegistry(task, in.Parallel)
		if err != nil {
			return dispatchErr(err.Error()), nil
		}
		regs[i] = reg
	}

	results := make([]dispatchResult, len(in.Tasks))
	if in.Parallel {
		t.runParallel(ctx, in.Tasks, regs, results)
	} else {
		t.runSequential(ctx, in.Tasks, regs, results)
	}

	out := dispatchOutput{Parallel: in.Parallel, Dispatched: len(in.Tasks), Results: results}
	for _, r := range results {
		out.TotalUsage.TotalTokens += r.Usage.TotalTokens
	}

	body, err := json.Marshal(out)
	if err != nil {
		return dispatchErr("failed to encode dispatch result: " + err.Error()), nil
	}
	return agent.Result{Content: string(body)}, nil
}

// childRegistry curates one child's tool registry from the host's, applying the
// depth-1 rule (never the dispatch tool) and the parallel-mode tier policy.
//
//   - An explicit task.tools list is validated: every name must be a real host tool,
//     and naming the dispatch tool or (in parallel mode) a confirm-tier tool fails
//     fast with the offending tool + tier — the model asked for something the mode
//     forbids, so we reject loudly rather than silently dropping it.
//   - With no explicit list the child inherits every eligible host tool: all but the
//     dispatch tool, and in parallel mode all but confirm-tier tools (parallel
//     children are read-only because a confirm cannot bubble across a fan-out).
func (t SubagentDispatchTool) childRegistry(task dispatchTask, parallel bool) (*agent.Registry, error) {
	child := agent.NewRegistry()
	if len(task.Tools) > 0 {
		for _, name := range task.Tools {
			if name == agent.DispatchToolName {
				return nil, fmt.Errorf("a sub-agent cannot be granted %q (sub-agents cannot dispatch further sub-agents)", agent.DispatchToolName)
			}
			tool, ok := t.cfg.Parent.Get(name)
			if !ok {
				return nil, fmt.Errorf("unknown tool %q (not in your tool set)", name)
			}
			if parallel && tool.Tier() == agent.TierConfirm {
				return nil, fmt.Errorf("tool %q is confirm-tier and cannot run in a parallel dispatch (confirmations cannot bubble across a fan-out); run it in a non-parallel dispatch", name)
			}
			child.Register(tool)
		}
		return child, nil
	}
	for _, name := range t.cfg.Parent.Names() {
		if name == agent.DispatchToolName {
			continue
		}
		tool, _ := t.cfg.Parent.Get(name)
		if parallel && tool.Tier() == agent.TierConfirm {
			continue
		}
		child.Register(tool)
	}
	return child, nil
}

// runSequential runs the children one after another. A single (non-parallel)
// dispatch may include confirm-tier tools, so the host ConfirmFunc is wrapped to
// name the originating sub-agent before bubbling.
func (t SubagentDispatchTool) runSequential(ctx context.Context, tasks []dispatchTask, regs []*agent.Registry, out []dispatchResult) {
	for i, task := range tasks {
		spec := t.specFor(i, task, regs[i])
		spec.Confirm = t.bubbleConfirm(i, task)
		res, err := agent.RunSubagent(t.childContext(ctx, i), spec)
		out[i] = renderResult(i, task, res, err)
	}
}

// runParallel runs the children concurrently. The task count is already capped at
// FanOutMax in Run, so the fan-out width — and thus the number of concurrent
// children — is bounded without a separate semaphore. Children are read-only
// (childRegistry stripped confirm-tier), so no confirm bubbles here; Confirm is
// left nil (fail-safe auto-deny on any escalation).
func (t SubagentDispatchTool) runParallel(ctx context.Context, tasks []dispatchTask, regs []*agent.Registry, out []dispatchResult) {
	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := agent.RunSubagent(t.childContext(ctx, i), t.specFor(i, tasks[i], regs[i]))
			out[i] = renderResult(i, tasks[i], res, err)
		}(i)
	}
	wg.Wait()
}

// childContext applies the optional DecorateChild hook so a surface can stamp
// per-child context (the control-plane stamps the audit sub-agent marker, T4).
func (t SubagentDispatchTool) childContext(ctx context.Context, i int) context.Context {
	if t.cfg.DecorateChild == nil {
		return ctx
	}
	return t.cfg.DecorateChild(ctx, subagentLabel(i))
}

// specFor builds the chassis spec for one child: curated registry, shared model +
// gate, configured caps, and a label naming the child for nested announcement.
func (t SubagentDispatchTool) specFor(i int, task dispatchTask, reg *agent.Registry) agent.SubagentSpec {
	return agent.SubagentSpec{
		System:    childSystemPrompt,
		Task:      task.Task,
		Registry:  reg,
		Model:     t.cfg.Model,
		Gate:      t.cfg.Gate,
		MaxTurns:  t.cfg.MaxTurns,
		MaxTokens: t.cfg.MaxTokens,
		Deadline:  t.cfg.Deadline,
		Label:     subagentLabel(i),
	}
}

// bubbleConfirm wraps the host ConfirmFunc so a confirm-tier tool inside child i
// surfaces on the host surface with the sub-agent named. A nil host confirm
// stays nil (the chassis then auto-denies confirm-tier tools, fail-safe).
func (t SubagentDispatchTool) bubbleConfirm(i int, task dispatchTask) agent.ConfirmFunc {
	if t.cfg.Confirm == nil {
		return nil
	}
	prefix := fmt.Sprintf("[%s — %s] ", subagentLabel(i), truncateTask(task.Task))
	return func(ctx context.Context, tool agent.Tool, input json.RawMessage, reason string) (bool, error) {
		return t.cfg.Confirm(ctx, tool, input, prefix+reason)
	}
}

// renderResult converts a chassis SubagentResult (or a spec/transport error) into
// the JSON-facing per-task result. A Go error from RunSubagent is a model/spec
// failure, not an in-band halt, so it surfaces as an error status the parent sees.
func renderResult(i int, task dispatchTask, res agent.SubagentResult, err error) dispatchResult {
	out := dispatchResult{
		Index:   i + 1,
		Task:    task.Task,
		Turns:   res.Turns,
		Usage:   dispatchUsage{TotalTokens: res.Usage.TotalTokens},
		Summary: res.Summary,
		Status:  res.Halt.String(),
	}
	if err != nil {
		out.Status = "error: " + err.Error()
		if out.Summary == "" {
			out.Summary = "the sub-agent could not run: " + err.Error()
		}
	}
	for _, d := range res.Denials {
		out.Denials = append(out.Denials, dispatchDenial{Tool: d.Tool, Kind: denialKindString(d.Kind)})
	}
	return out
}

func denialKindString(k agent.DenialKind) string {
	if k == agent.DenialTimeout {
		return "timeout"
	}
	return "user"
}

// subagentLabel is the stable per-dispatch child name used in confirm prompts and
// audit correlation. 1-based to match the user-facing result index.
func subagentLabel(i int) string { return fmt.Sprintf("subagent %d", i+1) }

func truncateTask(s string) string {
	s = strings.TrimSpace(s)
	const max = 60
	// Slice on a rune boundary so a multibyte character near the limit is never
	// cut mid-rune (the result is shown in the human-facing confirm prompt).
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func dispatchErr(msg string) agent.Result { return agent.Result{Content: msg, IsError: true} }
