package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// maxOverflowRetries bounds the reactive "prompt too long → recompact → retry" loop so a
// genuinely unfittable prompt surfaces the error instead of spinning.
const maxOverflowRetries = 3

// overflowTokenRe pulls the actual prompt-token count out of a context-overflow error
// message (e.g. "prompt is too long: 202695 tokens > 200000 maximum").
var overflowTokenRe = regexp.MustCompile(`(\d+)\s*tokens`)

// contextOverflowTokens reports whether err is a context-window-overflow error and, when
// the message carries the count, the actual prompt-token total that overflowed (0 if not
// parseable). Used to recalibrate from ground truth and retry.
func contextOverflowTokens(err error) (bool, int) {
	if err == nil {
		return false, 0
	}
	msg := strings.ToLower(err.Error())
	overflow := strings.Contains(msg, "prompt is too long") ||
		strings.Contains(msg, "context length") ||
		strings.Contains(msg, "maximum context") ||
		strings.Contains(msg, "context_length_exceeded") ||
		(strings.Contains(msg, "too long") && strings.Contains(msg, "token")) ||
		(strings.Contains(msg, "too many") && strings.Contains(msg, "token"))
	if !overflow {
		return false, 0
	}
	if m := overflowTokenRe.FindStringSubmatch(err.Error()); m != nil {
		n, _ := strconv.Atoi(m[1])
		return true, n
	}
	return true, 0
}

// ConfirmFunc asks the human to authorize a tool call. It returns true to allow.
// The TUI binds it to the Allow/Deny confirm gate (raised in every env); tests fake it.
// A nil ConfirmFunc is a deliberate fail-safe: every confirm-tier tool is
// declined. Wiring layers (Layer 2/3) MUST provide one, and should surface that
// it is set so a misconfiguration is not mistaken for the agent never proposing.
type ConfirmFunc func(ctx context.Context, tool Tool, input json.RawMessage, reason string) (bool, error)

// Loop is the agentic streaming tool-use loop (Claude Code's model). One Run is
// one user turn taken to completion: it calls the model, runs any tool_use
// blocks (auto in parallel, confirm-tier through the gate), feeds tool_result
// blocks back, and re-calls until a turn has no tool_use or the step cap trips.
type Loop struct {
	Model    Model
	Registry *Registry
	Gate     *Gate
	Skills   *SkillSet
	StepCap  int
	Confirm  ConfirmFunc

	// Compactor bounds the model-facing conversation each round (deterministic, no
	// model call) so a single tool-heavy turn can't overflow the window mid-turn.
	Compactor *Compactor
	// OnCompact reports that the model view was trimmed this turn (fired at most once
	// per turn), so the UI can surface a visible notice (optional).
	OnCompact func(CompactStat)

	// OnText streams assistant text to the UI (optional).
	OnText func(string)
	// OnReasoning streams the model's reasoning/thinking channel to the UI for
	// distinct display (optional). Reasoning is never persisted to the transcript.
	OnReasoning func(string)
	// OnToolStart announces a tool call for progress display, e.g. "observe_cost" (optional).
	OnToolStart func(name string, input json.RawMessage)
	// OnToolEnd reports a tool call's result for progress display — the raw output
	// (JSON for most tools, free text for use_skill) and whether it errored. Called
	// once per tool in block order on the loop goroutine (serial + ordered, like
	// OnToolStart), so a stateful UI sink never sees a concurrent callback. Optional.
	OnToolEnd func(name string, output json.RawMessage, isError bool)
	// Redactor, when non-nil, scrubs each tool result before it enters the
	// conversation (and the OnToolEnd peek). Host-supplied PII policy. Optional.
	Redactor Redactor
}

// ErrStepCap is returned when a turn exhausts StepCap tool rounds without a
// final answer; the caller surfaces a "continue?" prompt.
var ErrStepCap = fmt.Errorf("step cap reached")

// Run executes one user turn. history is the prior transcript (not mutated). It
// returns the NEW messages produced this turn (the user message + all assistant/tool
// turns) — NOT history — so the caller appends them to the persisted session. The
// model-facing conversation (history + produced) is rebuilt and bounded by the
// Compactor each round, so persistence keeps the full transcript while the model only
// ever sees a windowed view. The returned *Usage is the latest model call's token
// accounting, or nil if no call reported usage.
func (l *Loop) Run(ctx context.Context, system string, history []Message, user Message) ([]Message, *Usage, error) {
	produced := []Message{user}
	var usage *Usage
	var announcedTrim bool
	overflowRetries := 0

	// activeAllowed narrows the exposed tools when a skill is active (nil = all).
	var activeAllowed []string
	useSkill := newUseSkillTool(l.Skills, func(_ string, allow []string) {
		activeAllowed = withBuiltins(allow)
	})

	maxSteps := l.StepCap
	if maxSteps <= 0 {
		maxSteps = DefaultStepCap
	}
	for round := 0; round < maxSteps; round++ {
		// Bail promptly when the turn is cancelled (the TUI's esc-interrupt cancels
		// the turn context). Without this the loop would start another round — a fresh
		// Generate + a full tool round — before a ctx-bound call happened to error,
		// which is what made an interrupt feel like it hadn't taken.
		if err := ctx.Err(); err != nil {
			return produced, usage, err
		}

		// Build the model-facing conversation fresh each round, then bound it
		// deterministically (instant — no model call, so it can never hang). Only the
		// model view is trimmed; `produced` (returned for persistence) keeps the full,
		// un-elided messages so a resume has the complete transcript.
		conv := append(append([]Message(nil), history...), produced...)
		if l.Compactor != nil {
			if fitted, stat := l.Compactor.FitToWindow(conv); stat != nil {
				conv = fitted
				if !announcedTrim && l.OnCompact != nil {
					l.OnCompact(*stat)
				}
				announcedTrim = true
			}
		}

		req := ModelRequest{System: system, Messages: conv, Tools: l.exposedTools(activeAllowed)}
		resp, err := l.Model.Generate(ctx, req, l.OnText, l.OnReasoning)
		if err != nil {
			// Reactive fallback: the model rejected the prompt as too long (calibration
			// lagged, or a sudden density spike). Recalibrate from the error's exact token
			// count, trim harder, and retry THIS step — so the overflow self-heals and the
			// turn continues instead of failing. Bounded so an unfittable prompt still surfaces.
			if l.Compactor != nil && overflowRetries < maxOverflowRetries {
				if over, actual := contextOverflowTokens(err); over {
					l.Compactor.onOverflow(actual, conv)
					overflowRetries++
					round--
					continue
				}
			}
			return produced, usage, err
		}
		overflowRetries = 0
		if resp.Usage != nil {
			usage = resp.Usage
			// Learn the actual/estimate token ratio from the gateway's exact count so the
			// trim budget tracks this conversation's real density (chars/4 under-counts
			// dense JSON tool output). conv is exactly what was sent this call.
			if l.Compactor != nil && resp.Usage.PromptTokens > 0 {
				l.Compactor.observe(resp.Usage.PromptTokens, conv)
			}
		}
		produced = append(produced, resp.Message)

		uses := resp.Message.ToolUses()
		if len(uses) == 0 {
			return produced, usage, nil // final answer
		}
		// Cancelled between the model's answer and running its tools → stop here rather
		// than firing the tool calls.
		if err := ctx.Err(); err != nil {
			return produced, usage, err
		}

		results := l.runToolUses(ctx, uses, useSkill)
		produced = append(produced, Message{Role: RoleUser, Blocks: results})
	}
	return produced, usage, ErrStepCap
}

// exposedTools renders the tool schemas for the model, honoring skill narrowing.
// The use_skill builtin is always advertised so the model can load skills — but
// only once, even if a registry also happens to expose it.
func (l *Loop) exposedTools(allowed []string) []ToolSchema {
	schemas := l.Registry.Schemas(allowed)
	for _, s := range schemas {
		if s.Name == "use_skill" {
			return schemas
		}
	}
	return append(schemas, ToolSchema{
		Name:        "use_skill",
		Description: "Load a skill playbook by name to follow a proven procedure.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
	})
}

// runToolUses executes the tool_use blocks: auto-tier reads run in parallel,
// confirm-tier and declined calls are handled per-block; use_skill is handled
// inline. Results are returned in the original block order.
func (l *Loop) runToolUses(ctx context.Context, uses []Block, useSkill *useSkillTool) []Block {
	results := make([]Block, len(uses))

	var parallel []int
	for i, u := range uses {
		// use_skill is loop-handled (it narrows tools via its activate seam). Announce
		// it like any other tool so the operator SEES the skill load ("▸ use_skill")
		// in the transcript — a silent load looked like nothing happened. This runs on
		// the loop goroutine, so the OnToolStart contract stays serial + ordered.
		if u.ToolName == "use_skill" {
			if l.OnToolStart != nil {
				l.OnToolStart(u.ToolName, u.Input)
			}
			res, _ := useSkill.Run(ctx, u.Input)
			results[i] = ToolResult(u.ID, capToolResult(res.Content), res.IsError)
			continue
		}
		tool, ok := l.Registry.Get(u.ToolName)
		if !ok {
			results[i] = ToolResult(u.ID, fmt.Sprintf("no such tool %q", u.ToolName), true)
			continue
		}
		decision, reason := l.Gate.Decide(tool, u.Input)
		if decision == Ask {
			allowed := false
			if l.Confirm != nil {
				ok, cerr := l.Confirm(ctx, tool, u.Input, reason)
				allowed = ok && cerr == nil
			}
			if !allowed {
				// Declined is a normal signal (not an error) so the model adapts.
				results[i] = ToolResult(u.ID, "user declined this action", false)
				continue
			}
			// Approved confirm-tier tools run sequentially (each gated by a human).
			l.announce(tool, u)
			results[i] = l.execute(ctx, tool, u)
			continue
		}
		// Auto-allowed → eligible for parallel execution.
		parallel = append(parallel, i)
	}

	// Announce the auto-allowed reads in block order on THIS goroutine, then run them
	// concurrently. Splitting the announcement from execution keeps the OnToolStart
	// callback strictly serial and ordered even though the tool runs overlap — a
	// stateful UI sink (the TUI appends a transcript line) must never see a concurrent
	// call (see TestLoopToolStartCallbackIsSerial).
	for _, idx := range parallel {
		u := uses[idx]
		tool, _ := l.Registry.Get(u.ToolName)
		l.announce(tool, u)
	}
	var wg sync.WaitGroup
	for _, idx := range parallel {
		idx := idx
		u := uses[idx]
		tool, _ := l.Registry.Get(u.ToolName)
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[idx] = l.execute(ctx, tool, u)
		}()
	}
	wg.Wait()

	// Redact PII from tool output BEFORE it enters the conversation the model sees
	// (and the OnToolEnd peek). Runs serially on the loop goroutine after the
	// parallel batch joins, so a Redactor need not be concurrency-safe. Host-
	// injected: the web assistant scrubs raw traffic bodies before prompt entry;
	// the CLI leaves Redactor nil (passthrough — the operator sees real data).
	if l.Redactor != nil {
		for i, u := range uses {
			if results[i].Text == "" {
				continue
			}
			results[i].Text = l.Redactor.RedactToolOutput(u.ToolName, results[i].Text)
		}
	}

	// Report each tool's result in block order (serial, on this goroutine) so a
	// stateful UI sink shows the output peek without ever seeing a concurrent
	// OnToolEnd from the parallel batch — the same ordering contract as OnToolStart.
	if l.OnToolEnd != nil {
		for i, u := range uses {
			l.OnToolEnd(u.ToolName, json.RawMessage(results[i].Text), results[i].IsError)
		}
	}
	return results
}

// announce fires the OnToolStart progress callback (when wired). It is always called
// on the loop goroutine in block order — never from a parallel worker — so a stateful
// sink observes serial, ordered announcements.
func (l *Loop) announce(tool Tool, u Block) {
	if l.OnToolStart != nil {
		l.OnToolStart(tool.Name(), u.Input)
	}
}

// execute runs one tool and maps its outcome to a tool_result block. It does not
// announce (OnToolStart) — the caller announces in deterministic order off the
// parallel goroutines so the callback contract stays serial.
func (l *Loop) execute(ctx context.Context, tool Tool, u Block) Block {
	res, err := tool.Run(ctx, u.Input)
	if err != nil {
		return ToolResult(u.ID, "tool error: "+err.Error(), true)
	}
	// Cap the body the model sees so one large resource read can't dominate the window.
	return ToolResult(u.ID, capToolResult(res.Content), res.IsError)
}

// withBuiltins ensures the kernel builtins remain reachable even when a skill
// narrows the toolset (so the agent can still load another skill / remember).
func withBuiltins(allow []string) []string {
	out := append([]string(nil), allow...)
	for _, b := range []string{"use_skill", "remember", "forget"} {
		if !contains(out, b) {
			out = append(out, b)
		}
	}
	sort.Strings(out)
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
