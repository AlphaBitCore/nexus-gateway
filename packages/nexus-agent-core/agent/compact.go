package agent

import (
	"context"
	"strings"
)

// compactInstruction is the fixed prompt that turns the older transcript into a
// single durable summary preserving findings, decisions, and open threads. Used
// only by the explicit /compact path (ForceCompact).
const compactInstruction = `Summarize the conversation so far into a compact briefing that preserves: (1) findings and the data behind them, (2) decisions made, (3) open threads still to resolve. Write it as notes the assistant can rely on to continue. Do not invent anything not present above.`

// SummaryPrefix tags the synthesised summary message so it reads as prior
// context; hosts use it to render the condensed briefing as a system notice
// instead of a user bubble when a transcript is reloaded.
const SummaryPrefix = "[earlier conversation summary]\n"

// elidedPlaceholder replaces an old block body when the deterministic trimmer frees
// space. The block (and its tool_use/tool_result id) is kept so message structure and
// pairing stay valid — only the bulky body is dropped.
const elidedPlaceholder = "[earlier output elided to fit the context window]"

// Sizing. Auto context-bounding is DETERMINISTIC — no model call, so it is instant
// and can never hang (the 211k>200k overflow + the /compact "working… 115s" stall
// both came from a model-summary on the hot path). Two levers:
//   - maxToolResultChars caps each tool-result body the model sees, so one large
//     resource read can't dominate the window.
//   - trimBudget (in chars/4 ESTIMATE tokens) is the ceiling for the whole model-facing
//     conversation; when it is exceeded the trimmer elides the oldest block bodies
//     (tool output first) until the conversation fits.
//
// CALIBRATION. The trimmer measures size with the chars/4 estimate, but the gateway
// bills ACTUAL tokens — and JSON / IDs / dense tool output tokenize ~1.3–2x denser than
// chars/4 assumes. Bounding the estimate to a fixed fraction of the window therefore
// still overflowed (estimate ~130k → actual ~197k → `200032 > 200000`). So the budget is
// derived from the window in ACTUAL tokens (convActualFraction, leaving headroom for the
// system prompt + tool schemas + bundle + reply) divided by a live `calibration` ratio
// the loop learns from each response's exact prompt-token count (Compactor.observe). It
// starts conservative and relaxes once the real density is known.
const (
	maxToolResultChars = 24000 // ~6k chars/4 tokens
	convActualFraction = 0.78  // model-facing conv may use this much of the window in ACTUAL tokens
	trimHysteresis     = 0.55  // once trimming, free DOWN to this fraction of the budget so there's
	//                              headroom and we don't re-trim every turn (the "many trim lines" issue)
	defaultCalibration    = 2.0 // initial actual/estimate ratio (conservative until measured)
	minCalibration        = 1.0
	maxCalibration        = 4.0
	summaryKeepFraction   = 0.40 // ForceCompact: keep recent tail within this token budget
	defaultKeepRecent     = 6    // protected recent messages the trimmer never elides
	fallbackContextWindow = 128000
)

// Compactor bounds the model-facing conversation. FitToWindow is the deterministic
// auto path (cap + elide, no model call). ForceCompact is the explicit /compact path
// (one model summary call, interruptible).
type Compactor struct {
	model       Model
	keepRecent  int     // protected recent messages (FitToWindow) / verbatim tail floor (ForceCompact)
	window      int     // model context window in actual tokens
	calibration float64 // learned actual/estimate token ratio (see observe)
	trimBudget  int     // FitToWindow ceiling in ESTIMATE tokens; derived from window/calibration
	keepTarget  int     // ForceCompact: keep the recent tail within this estimated-token budget
}

// autoCompactFraction is the share of the model window at which a FINISHED turn
// triggers a durable session compaction automatically — the same summarize-and-
// rewrite the manual /compact runs, fired by the kernel instead of waiting for a
// human. Without it the deterministic trimmer re-elides the same overgrown
// transcript every single turn ("context trimmed" spam) and the persisted
// session never shrinks. 0.70 leaves enough headroom that the post-compact
// session (summary + the summaryKeepFraction tail) sits well below the trigger,
// so consecutive turns do not re-fire it.
const autoCompactFraction = 0.70

// ShouldAutoCompact reports whether a turn that ACTUALLY consumed promptTokens
// (the gateway-billed figure, not an estimate) has crossed the auto-compaction
// threshold. Nil-safe: an agent without a compactor never auto-compacts.
func (c *Compactor) ShouldAutoCompact(promptTokens int) bool {
	return c != nil && promptTokens > 0 && promptTokens >= int(float64(c.window)*autoCompactFraction)
}

// NewCompactor sizes the budgets to the model's context window (the catalog's
// MaxContextTokens; 0 = unknown → a conservative fallback).
func NewCompactor(model Model, contextWindow int) *Compactor {
	if contextWindow <= 0 {
		contextWindow = fallbackContextWindow
	}
	c := &Compactor{
		model:       model,
		keepRecent:  defaultKeepRecent,
		window:      contextWindow,
		calibration: defaultCalibration,
		keepTarget:  int(float64(contextWindow) * summaryKeepFraction),
	}
	c.trimBudget = c.computeBudget()
	return c
}

// computeBudget converts the actual-token conv budget into an estimate-token ceiling
// using the current calibration: estimate × calibration ≈ actual, so
// estimate_budget = (window × convActualFraction) / calibration.
func (c *Compactor) computeBudget() int {
	cal := c.calibration
	if cal < minCalibration {
		cal = minCalibration
	}
	return int(float64(c.window) * convActualFraction / cal)
}

// onOverflow is the reactive fallback: the model rejected the prompt as too long, so the
// current budget is provably too high. Recalibrate from the error's exact actual-token
// count (ground truth) when available, then shrink the budget further with a safety
// margin so the immediate retry definitely fits. `sent` is the conversation that
// overflowed; actualPrompt is the count parsed from the error (0 if unparseable).
func (c *Compactor) onOverflow(actualPrompt int, sent []Message) {
	if actualPrompt > 0 {
		c.observe(actualPrompt, sent)
	}
	c.calibration *= 1.25 // extra margin beyond the measured ratio
	if c.calibration > maxCalibration {
		c.calibration = maxCalibration
	}
	c.trimBudget = c.computeBudget()
}

// observe refines the actual/estimate token ratio from a real gateway response so the
// trim budget tracks how densely THIS conversation tokenizes (chars/4 under-counts
// JSON/IDs, which caused the 197k→overflow). actualPrompt is the gateway's exact
// prompt-token count for the request whose model-facing messages were `sent`.
func (c *Compactor) observe(actualPrompt int, sent []Message) {
	est := estimateMessages(sent)
	if est <= 0 || actualPrompt <= 0 {
		return
	}
	ratio := float64(actualPrompt) / float64(est)
	if ratio < minCalibration {
		ratio = minCalibration
	}
	if ratio > maxCalibration {
		ratio = maxCalibration
	}
	c.calibration = ratio
	c.trimBudget = c.computeBudget()
}

// CompactStat reports what a compaction did, for the visible UI notice and the
// post-compaction context-stats refresh. Kind distinguishes the deterministic auto
// trim ("trim") from the explicit model summary ("summary").
type CompactStat struct {
	Kind           string // "trim" | "summary"
	TokensBefore   int    // estimated
	TokensAfter    int    // estimated
	Elided         int    // trim: number of messages whose bodies were elided
	MessagesBefore int    // summary: transcript length before
	MessagesAfter  int    // summary: transcript length after
}

// capToolResult truncates an oversized tool-result body so a single large read can't
// dominate the window. Kept deterministic (no tokenizer) — chars are a fine proxy.
func capToolResult(s string) string {
	if len(s) <= maxToolResultChars {
		return s
	}
	return CutText(s, maxToolResultChars) + "\n…[output truncated to fit the context window]"
}

// FitToWindow returns a model-facing copy of msgs bounded to the trim budget,
// deterministically and instantly (NO model call). When the estimate exceeds the
// budget it elides the oldest block bodies — tool-result output first, then other
// text/tool-args — replacing them with a placeholder while keeping every message and
// every tool_use/tool_result id intact (so structure and pairing stay valid). The
// most recent keepRecent messages are never elided. A nil *CompactStat means it did
// not need to act.
func (c *Compactor) FitToWindow(msgs []Message) ([]Message, *CompactStat) {
	before := estimateMessages(msgs)
	if c.trimBudget <= 0 || before <= c.trimBudget {
		return msgs, nil
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)

	elided := 0
	// Soft target: once trimming is triggered (over budget), free DOWN to a lower target
	// (hysteresis) so there is headroom and we don't re-trim every turn. Two oldest-first
	// passes that PROTECT the recent keepRecent tail: (0) tool-result bodies (the bulk),
	// then (1) text + tool-call arguments.
	target := int(float64(c.trimBudget) * trimHysteresis)
	protectFrom := len(out) - c.keepRecent
	if protectFrom < 0 {
		protectFrom = 0
	}
	for _, toolOnly := range []bool{true, false} {
		if estimateMessages(out) <= target {
			break
		}
		for i := 0; i < protectFrom && estimateMessages(out) > target; i++ {
			if nb, did := elideMessage(out[i], toolOnly); did {
				out[i] = nb
				elided++
			}
		}
	}
	// Hard guarantee: if protecting the tail still leaves us over the BUDGET (e.g. the
	// recent tail alone is huge), elide into the tail too — keep only the single most
	// recent message — so the model view can never exceed the window.
	if estimateMessages(out) > c.trimBudget {
		for i := 0; i < len(out)-1 && estimateMessages(out) > c.trimBudget; i++ {
			if nb, did := elideMessage(out[i], false); did {
				out[i] = nb
				elided++
			}
		}
	}
	after := estimateMessages(out)
	if after >= before {
		return msgs, nil // nothing elidable (all recent / already elided)
	}
	// Report ~actual tokens (estimate × calibration) so the notice matches the gauge,
	// which shows the gateway's exact count — not the chars/4 estimate.
	return out, &CompactStat{
		Kind:         "trim",
		TokensBefore: int(float64(before) * c.calibration),
		TokensAfter:  int(float64(after) * c.calibration),
		Elided:       elided,
	}
}

// elideMessage returns a copy of m with bulky block bodies replaced by the
// placeholder. toolResultOnly=true elides only tool-result output; false also elides
// text blocks and tool-call argument blobs. Reports whether anything changed.
func elideMessage(m Message, toolResultOnly bool) (Message, bool) {
	changed := false
	blocks := make([]Block, len(m.Blocks))
	copy(blocks, m.Blocks)
	for i, b := range blocks {
		switch b.Type {
		case BlockToolResult:
			if b.Text != elidedPlaceholder && strings.TrimSpace(b.Text) != "" {
				blocks[i].Text = elidedPlaceholder
				changed = true
			}
		case BlockText:
			if !toolResultOnly && b.Text != elidedPlaceholder && strings.TrimSpace(b.Text) != "" {
				blocks[i].Text = elidedPlaceholder
				changed = true
			}
		case BlockToolUse:
			if !toolResultOnly && len(b.Input) > 0 {
				blocks[i].Input = nil
				changed = true
			}
		}
	}
	return Message{Role: m.Role, Blocks: blocks}, changed
}

// ForceCompact summarizes the older transcript in one model call and replaces it with
// a single summary message — the explicit /compact path. It first bounds its own input
// with FitToWindow so the summary call never ingests a near-window prompt (which is
// what made /compact stall). It honors ctx so an interrupt tears it down promptly. A
// nil *CompactStat means there was nothing safe to summarize.
func (c *Compactor) ForceCompact(ctx context.Context, history []Message) ([]Message, *CompactStat, error) {
	bounded := history
	if fitted, _ := c.FitToWindow(history); fitted != nil {
		bounded = fitted
	}
	cut := c.chooseCut(bounded)
	if cut <= 0 {
		return history, nil, nil
	}
	older := bounded[:cut]
	recent := bounded[cut:]

	req := ModelRequest{System: compactInstruction, Messages: older}
	resp, err := c.model.Generate(ctx, req, nil, nil)
	if err != nil {
		return history, nil, err
	}
	summary := strings.TrimSpace(resp.Message.Text())
	summaryMsg := TextMessage(RoleUser, SummaryPrefix+summary)

	out := make([]Message, 0, 1+len(recent))
	out = append(out, summaryMsg)
	out = append(out, recent...)

	return out, &CompactStat{
		Kind:           "summary",
		MessagesBefore: len(history),
		MessagesAfter:  len(out),
		TokensBefore:   estimateMessages(history),
		TokensAfter:    estimateMessages(out),
	}, nil
}

// chooseCut returns the index at which the kept recent tail begins for ForceCompact.
// The tail is selected newest-first, capped by keepRecent (count) and keepTarget
// (tokens), then snapped down to an assistant-role boundary so the kept tail begins
// with an assistant turn — which keeps each tool_result attached to its tool_use and
// keeps the user-role summary from being followed by another user message (both are
// provider 400s). A cut of 0 means there is no safe older portion to summarize.
func (c *Compactor) chooseCut(history []Message) int {
	keep := c.keepRecent
	if keep < 1 {
		keep = 1
	}
	n, toks := 0, 0
	for i := len(history) - 1; i >= 0; i-- {
		t := messageTokens(history[i])
		if n >= 1 && (n >= keep || (c.keepTarget > 0 && toks+t > c.keepTarget)) {
			break
		}
		toks += t
		n++
	}
	cut := len(history) - n
	for cut > 0 && history[cut].Role != RoleAssistant {
		cut--
	}
	return cut
}
