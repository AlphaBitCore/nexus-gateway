package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// subagent.go is the kernel chassis: one isolated child-agent
// loop, capped and tool-scoped, that reports a summary back to its parent. There
// is exactly ONE subagent implementation in the system — the chat dispatch tool
// (internal/assistant) and the workflow run engine's llm node (S5 NodeExecutor)
// both run on this chassis, parameterized by SubagentSpec.
//
// It deliberately does NOT reuse the main Loop (loop.go): a subagent has distinct
// semantics (a single parent-curated mandate that resolves to a summary, not a
// conversational turn) and S5-critical hooks (a ToolInterceptor that commits
// sub-invocations, an OutputValidator retry loop, a ResumeTranscript prefix) that
// would overload the shared assistant/CLI turn loop. The chassis reuses the
// kernel's PRIMITIVES (Model, Registry, Gate, ConfirmFunc, Message/Block,
// capToolResult) and leaves Loop untouched.

// DispatchToolName is the chat dispatch tool's name. The chassis asserts a
// child's registry never contains it: a subagent can never dispatch a further
// subagent (depth-1 enforcement).
const DispatchToolName = "subagent_dispatch"

// maxValidatorRetries bounds the OutputValidator retry-with-error-history loop so
// a child that can never satisfy its contract halts (HaltValidatorRejected)
// rather than burning its whole turn budget on revisions.
const maxValidatorRetries = 2

// SubagentHalt types why a child stopped short of a clean final answer, so the
// summary carries a precise failure narrative — no silent degradation
// while the operator is away.
type SubagentHalt int

const (
	HaltNone              SubagentHalt = iota // produced a validated final answer
	HaltTurnCap                               // exhausted MaxTurns without a final answer
	HaltTokenCap                              // crossed MaxTokens
	HaltDeadline                              // hit the spec's wall-clock Deadline
	HaltCancelled                             // the parent context was cancelled
	HaltValidatorRejected                     // OutputValidator rejected every attempt
	HaltError                                 // a model/transport error ended the run
)

func (h SubagentHalt) String() string {
	switch h {
	case HaltNone:
		return "completed"
	case HaltTurnCap:
		return "halted: turn cap reached"
	case HaltTokenCap:
		return "halted: token cap reached"
	case HaltDeadline:
		return "halted: deadline exceeded"
	case HaltCancelled:
		return "halted: cancelled by parent"
	case HaltValidatorRejected:
		return "halted: output failed its contract after retries"
	case HaltError:
		return "halted: execution error"
	default:
		return "halted: unknown"
	}
}

// DenialKind distinguishes a human "no" from an unattended timeout on a bubbled
// confirm, so the summary names which one happened — the operator who walked
// away must be able to tell a deliberate decline from a missed prompt.
type DenialKind int

const (
	DenialUser    DenialKind = iota // a human explicitly declined
	DenialTimeout                   // the confirm timed out (operator away)
)

// Denial records one bubbled-confirm denial inside the child, surfaced on the
// result for the failure narrative.
type Denial struct {
	Tool string
	Kind DenialKind
}

// ToolInterceptor wraps every child tool dispatch. next runs the tool and returns
// its Result; an interceptor may record the (sub-)invocation around it — this is
// where a host records a sub-invocation event — or
// short-circuit it. A nil interceptor calls next directly.
type ToolInterceptor func(ctx context.Context, tool Tool, input json.RawMessage, next func() Result) Result

// SubagentSpec is the parent-supplied contract for one child run. Caps are passed
// in (the chassis has no defaults registry of its own — chat consumers fill them
// from workflow/defaults.go, S5 from frozen meta), so the kernel stays free of
// product policy.
type SubagentSpec struct {
	// System is the child's system prompt; Task is its mandate (becomes the user
	// message). The child starts from these alone — never the parent's session —
	// so its context stays isolated.
	System string
	Task   string

	// Registry is the child's tool registry: a SUBSET of the parent's, curated by
	// the caller. It MUST NOT contain DispatchToolName (depth-1); the chassis
	// rejects a registry that does.
	Registry *Registry
	// Model is the model seam — the same core.Client path as the parent, so child
	// usage rides the parent's billing identity.
	Model Model
	// Gate decides confirm-tier escalation; nil → a default gate where confirm-tier
	// tools ask. Confirm is the PARENT's ConfirmFunc: a confirm-tier tool inside the
	// child bubbles its confirmation to the parent surface. A nil Confirm
	// auto-denies confirm-tier tools (fail-safe).
	Gate    *Gate
	Confirm ConfirmFunc
	// Compactor bounds the model-facing view each round (optional).
	Compactor *Compactor

	// Caps. MaxTurns bounds tool rounds (≤0 → DefaultStepCap); MaxTokens halts when
	// cumulative usage crosses it (0 → unlimited); Deadline bounds wall-clock
	// (0 → none). All hard — a child cannot run unbounded.
	MaxTurns  int
	MaxTokens int
	Deadline  time.Duration

	// Hooks — all optional with chat-grade defaults (nil). AC-S12-5: this set is
	// sufficient for S5's NodeExecutor without touching the engine package.
	//
	// ContextBuilder seeds curated history before the mandate. ResumeTranscript
	// supplies a recorded message prefix so a resumed run replays prior turns
	// (a host can rebuild it from its own turn records). ToolInterceptor wraps
	// every dispatch (S5 sub-invocation commits). OutputValidator gates the final
	// answer, driving a bounded retry-with-error-history loop on rejection.
	ContextBuilder   func(ctx context.Context) ([]Message, error)
	ResumeTranscript func() ([]Message, error)
	ToolInterceptor  ToolInterceptor
	OutputValidator  func(output string) error
	// Credential, when set, resolves a per-run credential (header name + value)
	// the child's tool/model transport reads from the run context — S5 supplies
	// the run token here so the child's self-calls authenticate as the run owner.
	// Resolved once at start and threaded onto the run context via
	// WithSubagentCredential; a nil hook leaves the context unstamped (chat
	// children already carry the caller bearer on their pre-built client).
	Credential func(ctx context.Context) (name, value string, err error)

	// Label names the child (subagent id / task line) for nested announcement and
	// audit correlation. Observers are optional progress callbacks.
	Label       string
	OnText      func(string)
	OnToolStart func(name string, input json.RawMessage)
	OnToolEnd   func(name string, output json.RawMessage, isError bool)
	// OnUnknownTool fires when the model requests a tool NOT in the child's
	// curated registry — the attempt is rejected (the child sees a non-fatal
	// "no such tool" result and adapts), and this hook lets the caller record
	// the rejected attempt out of band. The web host wires it to record the runtime
	// leg ("an llm node attempting an undeclared tool is rejected by injection
	// AND recorded on the blackboard"). nil → only the rejection happens.
	OnUnknownTool func(name string, input json.RawMessage)
}

// SubagentResult is the child's report to the parent. Summary is the final answer
// (or the best partial on a halt); Halt + Denials carry the typed failure
// narrative; Usage is the child's cumulative token cost.
type SubagentResult struct {
	Summary string
	Usage   Usage
	Halt    SubagentHalt
	Denials []Denial
	Turns   int
}

// ErrDispatchInChildRegistry is returned when a SubagentSpec's registry still
// contains the dispatch tool — a depth-1 violation that would let a child spawn
// grandchildren.
var ErrDispatchInChildRegistry = errors.New("subagent: child registry contains the dispatch tool (depth-1 violated)")

// SubagentCredential is a per-run credential (header name + value) threaded onto
// the run context for the child's transport to read (S5's run token).
type SubagentCredential struct {
	Name  string
	Value string
}

type subagentCredKey struct{}

// WithSubagentCredential returns a context carrying the child's per-run
// credential. Set by RunSubagent from the spec's Credential hook.
func WithSubagentCredential(ctx context.Context, c SubagentCredential) context.Context {
	return context.WithValue(ctx, subagentCredKey{}, c)
}

// SubagentCredentialFromContext returns the per-run credential threaded by
// RunSubagent, or false when none was set (chat children, where the transport
// already carries the caller bearer).
func SubagentCredentialFromContext(ctx context.Context) (SubagentCredential, bool) {
	c, ok := ctx.Value(subagentCredKey{}).(SubagentCredential)
	return c, ok
}

// RunSubagent runs one isolated child agent to completion or a capped halt. It
// returns a SubagentResult in all non-programmer-error cases (caps and denials
// are in-band on the result, not Go errors); it returns a non-nil error only for
// a misconfigured spec or a model/transport failure (paired with HaltError).
func RunSubagent(ctx context.Context, spec SubagentSpec) (SubagentResult, error) {
	if spec.Registry == nil {
		return SubagentResult{Halt: HaltError}, errors.New("subagent: nil registry")
	}
	if spec.Model == nil {
		return SubagentResult{Halt: HaltError}, errors.New("subagent: nil model")
	}
	// Depth-1: a child can never reach the dispatch tool.
	if _, ok := spec.Registry.Get(DispatchToolName); ok {
		return SubagentResult{Halt: HaltError}, ErrDispatchInChildRegistry
	}

	gate := spec.Gate
	if gate == nil {
		gate = NewGate(nil, nil, false) // no classifier; confirm-tier tools ask
	}
	maxTurns := spec.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultStepCap
	}

	// A wall-clock Deadline becomes a derived context so an unattended child cannot
	// run forever; haltForCtx tells deadline from a parent cancellation afterwards.
	runCtx := ctx
	if spec.Deadline > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Deadline)
		defer cancel()
	}

	// Resolve the per-run credential once and thread it onto the run context so the
	// child's transport (tool self-calls, model call) can read it — S5's run token.
	if spec.Credential != nil {
		name, value, cerr := spec.Credential(runCtx)
		if cerr != nil {
			return SubagentResult{Halt: HaltError}, fmt.Errorf("subagent credential: %w", cerr)
		}
		runCtx = WithSubagentCredential(runCtx, SubagentCredential{Name: name, Value: value})
	}

	history, err := seedHistory(runCtx, spec)
	if err != nil {
		return SubagentResult{Halt: HaltError}, err
	}

	produced := []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: spec.Task}}}}
	var total Usage
	var denials []Denial
	validatorAttempts := 0

	for round := 0; round < maxTurns; round++ {
		if halt, stopped := haltForCtx(ctx, runCtx); stopped {
			return SubagentResult{Summary: lastText(produced), Usage: total, Halt: halt, Denials: denials, Turns: round}, nil
		}
		if spec.MaxTokens > 0 && total.TotalTokens >= spec.MaxTokens {
			summary := wrapUpCapped(runCtx, spec, history, produced, &total,
				"Your token budget is exhausted.")
			return SubagentResult{Summary: summary, Usage: total, Halt: HaltTokenCap, Denials: denials, Turns: round}, nil
		}

		conv := append(append([]Message(nil), history...), produced...)
		if spec.Compactor != nil {
			if fitted, stat := spec.Compactor.FitToWindow(conv); stat != nil {
				conv = fitted
			}
		}

		req := ModelRequest{System: spec.System, Messages: conv, Tools: spec.Registry.Schemas(nil)}
		resp, gerr := spec.Model.Generate(runCtx, req, spec.OnText, nil)
		if gerr != nil {
			// A cancelled/expired context surfaces as a model error; classify it as the
			// matching halt rather than a raw error so the narrative stays precise.
			if halt, stopped := haltForCtx(ctx, runCtx); stopped {
				return SubagentResult{Summary: lastText(produced), Usage: total, Halt: halt, Denials: denials, Turns: round}, nil
			}
			return SubagentResult{Summary: lastText(produced), Usage: total, Halt: HaltError, Denials: denials, Turns: round}, gerr
		}
		addUsage(&total, resp.Usage)
		produced = append(produced, resp.Message)

		uses := resp.Message.ToolUses()
		if len(uses) == 0 {
			out := resp.Message.Text()
			if spec.OutputValidator != nil {
				if verr := spec.OutputValidator(out); verr != nil {
					if validatorAttempts >= maxValidatorRetries {
						return SubagentResult{Summary: out, Usage: total, Halt: HaltValidatorRejected, Denials: denials, Turns: round + 1}, nil
					}
					validatorAttempts++
					// Retry with the rejection as error history so the child can self-correct.
					produced = append(produced, Message{Role: RoleUser, Blocks: []Block{{Type: BlockText,
						Text: "Your final answer did not satisfy its output contract: " + verr.Error() +
							". Revise and produce a corrected final answer."}}})
					continue
				}
			}
			return SubagentResult{Summary: out, Usage: total, Halt: HaltNone, Denials: denials, Turns: round + 1}, nil
		}

		results, roundDenials := runChildTools(runCtx, spec, gate, uses)
		denials = append(denials, roundDenials...)
		produced = append(produced, Message{Role: RoleUser, Blocks: results})
	}
	summary := wrapUpCapped(runCtx, spec, history, produced, &total,
		"Your turn budget is exhausted.")
	return SubagentResult{Summary: summary, Usage: total, Halt: HaltTurnCap, Denials: denials, Turns: maxTurns}, nil
}

// wrapUpCapped gives a capped child ONE tool-less wrap-up completion so its
// work survives the cap: mid-tool-loop, lastText is typically empty or a plan
// fragment, and a child that just saved a draft would otherwise die without
// reporting the versionId — the only handle that makes the work reachable.
// Cost is bounded (a single model call, no tools to loop on); any failure
// falls back to the old lastText behavior rather than masking the halt.
func wrapUpCapped(ctx context.Context, spec SubagentSpec, history, produced []Message, total *Usage, why string) string {
	conv := append(append([]Message(nil), history...), produced...)
	conv = append(conv, Message{Role: RoleUser, Blocks: []Block{{Type: BlockText,
		Text: why + " STOP working now and write your final self-contained summary: what you completed, " +
			"what remains, and every identifier the main assistant needs to continue (especially any " +
			"versionId you created or updated). Do not call tools."}}})
	if spec.Compactor != nil {
		if fitted, stat := spec.Compactor.FitToWindow(conv); stat != nil {
			conv = fitted
		}
	}
	// No tools offered: the wrap-up cannot start another round.
	resp, err := spec.Model.Generate(ctx, ModelRequest{System: spec.System, Messages: conv}, spec.OnText, nil)
	if err != nil {
		return lastText(produced)
	}
	addUsage(total, resp.Usage)
	if out := resp.Message.Text(); strings.TrimSpace(out) != "" {
		return out
	}
	return lastText(produced)
}

// runChildTools dispatches one round of the child's tool calls in block order.
// Confirm-tier tools bubble to the parent's ConfirmFunc; a denial (or
// timeout) becomes a non-fatal tool signal the child sees and adapts to
// (AC-S12-3) while the typed denial is recorded for the summary. Every dispatch
// passes through the ToolInterceptor (S5 sub-invocation commits).
func runChildTools(ctx context.Context, spec SubagentSpec, gate *Gate, uses []Block) ([]Block, []Denial) {
	results := make([]Block, len(uses))
	var denials []Denial
	for i, u := range uses {
		tool, ok := spec.Registry.Get(u.ToolName)
		if !ok {
			// The model asked for a tool outside its curated registry — reject it
			// (non-fatal so the child adapts) and surface the rejected attempt so
			// the caller can record it.
			if spec.OnUnknownTool != nil {
				spec.OnUnknownTool(u.ToolName, u.Input)
			}
			results[i] = ToolResult(u.ID, fmt.Sprintf("no such tool %q", u.ToolName), true)
			continue
		}
		if decision, reason := gate.Decide(tool, u.Input); decision == Ask {
			approved := false
			var cerr error
			if spec.Confirm != nil {
				approved, cerr = spec.Confirm(ctx, tool, u.Input, reason)
			}
			if !approved || cerr != nil {
				kind := DenialUser
				msg := "this action was declined"
				if isTimeout(ctx, cerr) {
					kind = DenialTimeout
					msg = "confirmation timed out (operator away); action not taken"
				}
				denials = append(denials, Denial{Tool: tool.Name(), Kind: kind})
				// Declined is a non-fatal signal so the child adapts rather than aborting.
				results[i] = ToolResult(u.ID, msg, false)
				continue
			}
		}
		if spec.OnToolStart != nil {
			spec.OnToolStart(tool.Name(), u.Input)
		}
		res := dispatchChildTool(ctx, tool, u.Input, spec.ToolInterceptor)
		results[i] = ToolResult(u.ID, capToolResult(res.Content), res.IsError)
		if spec.OnToolEnd != nil {
			spec.OnToolEnd(tool.Name(), json.RawMessage(results[i].Text), results[i].IsError)
		}
	}
	return results, denials
}

// dispatchChildTool runs one tool through the interceptor (when set). next maps a
// tool error to an error Result so a failing tool is a signal to the child, never
// a crash of the chassis.
func dispatchChildTool(ctx context.Context, tool Tool, input json.RawMessage, interceptor ToolInterceptor) Result {
	next := func() Result {
		res, err := tool.Run(ctx, input)
		if err != nil {
			return Result{Content: "tool error: " + err.Error(), IsError: true}
		}
		return res
	}
	if interceptor != nil {
		return interceptor(ctx, tool, input, next)
	}
	return next()
}

// seedHistory composes the child's starting transcript: curated context first,
// then a recorded resume prefix (S5). Both are optional.
func seedHistory(ctx context.Context, spec SubagentSpec) ([]Message, error) {
	var history []Message
	if spec.ContextBuilder != nil {
		h, err := spec.ContextBuilder(ctx)
		if err != nil {
			return nil, fmt.Errorf("subagent context builder: %w", err)
		}
		history = append(history, h...)
	}
	if spec.ResumeTranscript != nil {
		h, err := spec.ResumeTranscript()
		if err != nil {
			return nil, fmt.Errorf("subagent resume transcript: %w", err)
		}
		history = append(history, h...)
	}
	return history, nil
}

// haltForCtx reports whether the run context is done and which halt it maps to:
// a parent-context cancellation is HaltCancelled, while the chassis's own derived
// deadline firing (parent still live) is HaltDeadline.
func haltForCtx(parent, runCtx context.Context) (SubagentHalt, bool) {
	if parent.Err() != nil {
		return HaltCancelled, true
	}
	if runCtx.Err() != nil {
		return HaltDeadline, true
	}
	return HaltNone, false
}

// isTimeout reports whether a bubbled-confirm error is a timeout (operator away)
// rather than a deliberate decline — detected via a deadline-exceeded context
// error, the idiom a timing-out confirm returns.
func isTimeout(ctx context.Context, cerr error) bool {
	if errors.Is(cerr, context.DeadlineExceeded) {
		return true
	}
	return ctx.Err() == context.DeadlineExceeded
}

// addUsage accumulates one model call's usage into the child's running total.
func addUsage(total *Usage, u *Usage) {
	if u == nil {
		return
	}
	total.PromptTokens += u.PromptTokens
	total.CompletionTokens += u.CompletionTokens
	total.TotalTokens += u.TotalTokens
	total.CachedTokens += u.CachedTokens
}

// lastText returns the text of the most recent assistant message, the best
// partial summary to surface when a child halts before a clean final answer.
func lastText(produced []Message) string {
	for i := len(produced) - 1; i >= 0; i-- {
		if produced[i].Role == RoleAssistant {
			if t := produced[i].Text(); t != "" {
				return t
			}
		}
	}
	return ""
}
