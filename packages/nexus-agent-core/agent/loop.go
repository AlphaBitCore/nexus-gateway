package agent

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"regexp"
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
	StepCap  int
	Confirm  ConfirmFunc

	// MaxToolCallsPerRound bounds how many auto-tier tools run CONCURRENTLY within a
	// single round. The model can emit an arbitrary number of tool_use blocks in one
	// response; without a cap each would spawn its own goroutine, so a single
	// adversarial or runaway response could fan out to hundreds of simultaneous
	// admin self-calls. A weighted semaphore caps in-flight execution; the rest queue
	// and run as slots free, so throughput is preserved while peak concurrency is
	// bounded. <= 0 falls back to DefaultMaxToolCallsPerRound.
	MaxToolCallsPerRound int
	// MaxToolCallsPerTurn bounds the CUMULATIVE number of tool calls executed across
	// all rounds of one turn — a runaway-cost ceiling orthogonal to StepCap (which
	// bounds rounds, not calls). Once the ceiling is reached the loop stops issuing
	// more tool calls and returns a structured "tool call limit exceeded" result for
	// the remaining blocks so the model adapts (rather than the turn silently
	// truncating). <= 0 falls back to DefaultMaxToolCallsPerTurn.
	MaxToolCallsPerTurn int

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
	// and whether it errored. Called
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

// DefaultMaxToolCallsPerRound bounds concurrent auto-tier tool execution within a
// single round (see Loop.MaxToolCallsPerRound). 8 keeps a normal multi-resource
// investigation fully parallel while capping the blast radius of a response that
// emits a pathological number of tool_use blocks.
const DefaultMaxToolCallsPerRound = 8

// DefaultMaxToolCallsPerTurn bounds the cumulative tool calls across all rounds of
// one turn (see Loop.MaxToolCallsPerTurn). 200 is far above any legitimate turn
// (a 40-round turn at ~5 calls/round) yet stops a runaway loop's cost.
const DefaultMaxToolCallsPerTurn = 200

// toolLimitExceededMsg is the structured tool_result returned for blocks dropped
// once the per-turn ceiling is hit, so the model sees a clear, adaptable signal.
const toolLimitExceededMsg = "tool call limit exceeded for this turn; stop calling tools and summarize what you have"

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

	maxSteps := l.StepCap
	if maxSteps <= 0 {
		maxSteps = DefaultStepCap
	}
	// turnToolCalls accumulates across rounds so the per-turn ceiling spans the whole
	// turn, not a single round. Passed by pointer into runToolUses.
	turnToolCalls := 0
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

		req := ModelRequest{System: system, Messages: sendableMessages(conv), Tools: l.exposedTools()}
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
		// A contentless reply (interrupt raced the stream, or the provider closed
		// without emitting blocks) must not enter the transcript: providers reject
		// empty content on replay, so one such message would poison every later
		// turn of the session with a 400.
		if len(resp.Message.Blocks) > 0 {
			produced = append(produced, resp.Message)
		}

		uses := resp.Message.ToolUses()
		if len(uses) == 0 {
			return produced, usage, nil // final answer
		}
		// Cancelled between the model's answer and running its tools → stop here rather
		// than firing the tool calls.
		if err := ctx.Err(); err != nil {
			return produced, usage, err
		}

		results := l.runToolUses(ctx, uses, &turnToolCalls)
		produced = append(produced, Message{Role: RoleUser, Blocks: results})
	}
	return produced, usage, ErrStepCap
}

// sendableMessages filters the model view down to what providers accept on
// replay: empty text blocks are dropped from each message, and a message left
// with no blocks at all is skipped. The persisted transcript is untouched —
// this guards the REQUEST, so a historic empty message (older session files)
// can never 400 the whole conversation again.
func sendableMessages(conv []Message) []Message {
	out := make([]Message, 0, len(conv))
	for _, m := range conv {
		blocks := make([]Block, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			if b.Type == BlockText && strings.TrimSpace(b.Text) == "" {
				continue
			}
			blocks = append(blocks, b)
		}
		if len(blocks) == 0 {
			continue
		}
		m.Blocks = blocks
		out = append(out, m)
	}
	return out
}

// exposedTools renders the tool schemas for the model.
func (l *Loop) exposedTools() []ToolSchema {
	return l.Registry.Schemas(nil)
}

// runToolUses executes the tool_use blocks: auto-tier reads run in parallel
// (bounded by MaxToolCallsPerRound), confirm-tier and declined calls are handled
// per-block. *turnToolCalls accumulates executed calls across rounds; once
// MaxToolCallsPerTurn is reached the remaining blocks return a structured "tool
// call limit exceeded" result instead of executing. Results are returned in the
// original block order.
func (l *Loop) runToolUses(ctx context.Context, uses []Block, turnToolCalls *int) []Block {
	results := make([]Block, len(uses))
	turnCap := l.MaxToolCallsPerTurn
	if turnCap <= 0 {
		turnCap = DefaultMaxToolCallsPerTurn
	}

	// limitHit reports whether the per-turn ceiling is already reached; bumpTurn
	// charges one executed call against the ceiling. Both run only on this (the loop)
	// goroutine, before any parallel fan-out, so no synchronization is needed.
	limitHit := func() bool { return *turnToolCalls >= turnCap }

	var parallel []int
	for i, u := range uses {
		if limitHit() {
			// Ceiling reached: drop the rest with a structured signal so the model stops
			// calling tools and summarizes (IsError so it is unmistakable).
			results[i] = ToolResult(u.ID, toolLimitExceededMsg, true)
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
			*turnToolCalls++
			l.announce(tool, u)
			results[i] = l.execute(ctx, tool, u)
			continue
		}
		// Auto-allowed → eligible for parallel execution. Charge it against the per-turn
		// ceiling now (on this goroutine) so the count is race-free.
		*turnToolCalls++
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
	// Bound peak concurrency with a counting semaphore: the model can emit far more
	// auto-tier blocks than we should run at once. Excess goroutines block on the
	// channel until a slot frees, so all run but never more than roundCap at a time.
	roundCap := l.MaxToolCallsPerRound
	if roundCap <= 0 {
		roundCap = DefaultMaxToolCallsPerRound
	}
	sem := make(chan struct{}, roundCap)
	var wg sync.WaitGroup
	for _, idx := range parallel {
		idx := idx
		u := uses[idx]
		tool, _ := l.Registry.Get(u.ToolName)
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}        // acquire a slot (blocks past roundCap in flight)
			defer func() { <-sem }() // release the slot
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
//
// A panic barrier wraps tool.Run: a tool that panics must NOT crash the shared
// process (the web assistant runs in-process inside the Control Plane, so a panic
// would take down the CP). Any panic is recovered and converted into an IsError
// tool_result carrying the panic message, so the model sees the failure and the
// turn survives. This covers BOTH execution paths — the synchronous confirm-tier
// call on the loop goroutine and the parallel auto-tier goroutines.
func (l *Loop) execute(ctx context.Context, tool Tool, u Block) (out Block) {
	defer func() {
		if r := recover(); r != nil {
			out = ToolResult(u.ID, fmt.Sprintf("tool panicked: %v", r), true)
		}
	}()
	res, err := tool.Run(ctx, u.Input)
	if err != nil {
		return ToolResult(u.ID, "tool error: "+err.Error(), true)
	}
	// Cap the body the model sees so one large resource read can't dominate the window.
	return ToolResult(u.ID, capToolResult(res.Content), res.IsError)
}
