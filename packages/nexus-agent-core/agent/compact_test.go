package agent

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// altHistory builds an n-message alternating transcript U,A,U,A,… starting with a
// user message — the shape a real turn loop produces.
func altHistory(n int) []Message {
	h := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			h = append(h, TextMessage(RoleUser, "u"+strconv.Itoa(i)))
		} else {
			h = append(h, TextMessage(RoleAssistant, "a"+strconv.Itoa(i)))
		}
	}
	return h
}

// toolRound builds an assistant tool_use message + its user tool_result reply, the
// pair shape the loop produces. body is the (large) tool output.
func toolRound(id, body string) []Message {
	return []Message{
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ID: id, ToolName: "read", Input: json.RawMessage(`{}`)}}},
		{Role: RoleUser, Blocks: []Block{ToolResult(id, body, false)}},
	}
}

// ---- capToolResult ----

func TestCapToolResultTruncatesOversizedBody(t *testing.T) {
	small := "ok"
	if capToolResult(small) != small {
		t.Fatal("a small body must pass through unchanged")
	}
	big := strings.Repeat("x", maxToolResultChars+5000)
	got := capToolResult(big)
	if len(got) >= len(big) {
		t.Fatalf("an oversized body must be truncated, got len %d", len(got))
	}
	if !strings.Contains(got, "truncated to fit the context window") {
		t.Fatal("truncation must leave a visible notice")
	}
}

// ---- FitToWindow (deterministic auto trim) ----

func TestFitToWindowUnderBudgetIsNoOp(t *testing.T) {
	c := NewCompactor(nil, 0) // nil model is fine — FitToWindow never calls the model
	hist := altHistory(20)
	out, stat := c.FitToWindow(hist)
	if stat != nil {
		t.Fatalf("under budget must be a no-op, stat=%v", stat)
	}
	if len(out) != 20 {
		t.Fatalf("no-op must not alter the conversation, got %d", len(out))
	}
}

func TestFitToWindowElidesOldestToolOutputToFit(t *testing.T) {
	c := NewCompactor(nil, 0)
	c.trimBudget = 200 // tokens — above the per-message placeholder floor, below the full size
	c.keepRecent = 2
	var hist []Message
	hist = append(hist, TextMessage(RoleUser, "investigate the jobs"))
	for i := 0; i < 6; i++ { // six big tool rounds
		hist = append(hist, toolRound("t"+strconv.Itoa(i), strings.Repeat("data ", 60))...) // ~75 tok each
	}
	before := estimateMessages(hist)
	out, stat := c.FitToWindow(hist)
	if stat == nil {
		t.Fatalf("over budget must trim, before=%d budget=%d", before, c.trimBudget)
	}
	if got := estimateMessages(out); got > c.trimBudget {
		t.Fatalf("trim must bring the conversation under budget, got %d > %d", got, c.trimBudget)
	}
	if stat.TokensAfter >= stat.TokensBefore || stat.Elided == 0 {
		t.Fatalf("trim must report freed tokens + elided count, got %+v", stat)
	}
	// Structure preserved: same message count and roles, every tool_use still paired
	// with a tool_result of the same id (no orphaned/dropped blocks).
	if len(out) != len(hist) {
		t.Fatalf("trim must elide bodies, never drop messages: %d != %d", len(out), len(hist))
	}
	for i := range out {
		if out[i].Role != hist[i].Role {
			t.Fatalf("trim must not change roles at %d", i)
		}
	}
	assertToolPairsIntact(t, out)
	// The most recent message (the last tool_result) is protected — not elided.
	if last := out[len(out)-1]; last.Blocks[0].Text == elidedPlaceholder {
		t.Fatal("the protected recent tail must not be elided")
	}
}

func TestFitToWindowElidesTextWhenToolOutputNotEnough(t *testing.T) {
	// No tool results at all — pass 1 must elide plain text to still fit.
	c := NewCompactor(nil, 0)
	c.trimBudget = 200
	c.keepRecent = 2
	var hist []Message
	for i := 0; i < 10; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		hist = append(hist, TextMessage(role, strings.Repeat("word ", 40)))
	}
	out, stat := c.FitToWindow(hist)
	if stat == nil || estimateMessages(out) > c.trimBudget {
		t.Fatalf("must elide text to fit, stat=%v size=%d budget=%d", stat, estimateMessages(out), c.trimBudget)
	}
}

func TestFitToWindowProtectsRecentTail(t *testing.T) {
	c := NewCompactor(nil, 0)
	c.trimBudget = 260 // above floor + protected tail, so the recent tail is preserved
	c.keepRecent = 3
	var hist []Message
	for i := 0; i < 8; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		hist = append(hist, TextMessage(role, strings.Repeat("z ", 100)))
	}
	out, _ := c.FitToWindow(hist)
	// Last keepRecent messages keep their content; older ones are elided.
	for i := len(out) - c.keepRecent; i < len(out); i++ {
		if out[i].Text() == elidedPlaceholder {
			t.Fatalf("protected message %d must not be elided", i)
		}
	}
	if out[0].Text() != elidedPlaceholder {
		t.Fatal("the oldest message should be elided to fit")
	}
}

func TestFitToWindowHysteresisFreesHeadroom(t *testing.T) {
	// Once triggered, the trimmer frees DOWN to a soft target well below the budget so it
	// does not re-trim every turn (the "many trim lines" report). With plenty of old tool
	// output to elide, the result lands near the soft target, not just barely under budget.
	c := NewCompactor(nil, 0)
	c.trimBudget = 1000
	c.keepRecent = 2
	hist := []Message{TextMessage(RoleUser, "start")}
	for i := 0; i < 20; i++ {
		hist = append(hist, toolRound("t"+strconv.Itoa(i), strings.Repeat("data ", 60))...)
	}
	out, stat := c.FitToWindow(hist)
	if stat == nil {
		t.Fatal("must trim when over budget")
	}
	got := estimateMessages(out)
	if got > c.trimBudget*9/10 {
		t.Fatalf("hysteresis must free well below the budget for headroom, got %d (budget %d)", got, c.trimBudget)
	}
	// The recent tail is still protected (not elided) on the soft path.
	if out[len(out)-1].Blocks[0].Text == elidedPlaceholder {
		t.Fatal("the most recent message must remain intact")
	}
}

// ---- Loop integration: the user's failure mode ----

func TestLoopBoundsModelViewAcrossManyLargeToolReads(t *testing.T) {
	// The operator workload that overflowed in prod: ONE turn issues many resource
	// reads, each returning a large body. The model-facing request must stay under the
	// trim budget every round — deterministically, with NO summary model call (so it
	// can never hang). This is the regression test for "211k > 200k mid-turn".
	reg := NewRegistry()
	big := strings.Repeat("payload ", 20000) // ~160k chars per raw result
	reg.Register(&stubTool{name: "read", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		return Result{Content: big}, nil
	}})
	var resps []*ModelResponse
	for i := 0; i < 15; i++ {
		resps = append(resps, asstToolUse("u"+strconv.Itoa(i), "read", `{}`))
	}
	resps = append(resps, asstText("done"))
	fm := newFakeModel(resps...)

	comp := NewCompactor(fm, 0)
	comp.trimBudget = 40000 // tokens
	comp.keepRecent = 4
	loop := &Loop{Model: fm, Registry: reg, Gate: NewGate(nil, nil, false), StepCap: 40, Compactor: comp}

	produced, _, err := loop.Run(context.Background(), "sys", nil, TextMessage(RoleUser, "investigate every job"))
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	// Every model request stayed under budget — the mid-turn bound held.
	for i, req := range fm.gotReqs {
		if est := estimateMessages(req.Messages); est > comp.trimBudget {
			t.Fatalf("round %d: model request was %d tok > trim budget %d — mid-turn overflow NOT prevented", i, est, comp.trimBudget)
		}
		// The deterministic path must never make a summary model call.
		if req.System == compactInstruction {
			t.Fatal("auto bounding made a model summary call — it must be deterministic (no model call, never hangs)")
		}
	}
	// The full turn still ran to completion and the persisted (produced) messages keep
	// the full, un-elided tool outputs (capped, but not placeholder-elided).
	var sawFullBody bool
	for _, m := range produced {
		for _, b := range m.Blocks {
			if b.Type == BlockToolResult && strings.Contains(b.Text, "payload") {
				sawFullBody = true
			}
		}
	}
	if !sawFullBody {
		t.Fatal("produced messages (for persistence) must keep the real tool output, not the elided placeholder")
	}
}

// ---- calibration (actual vs chars/4 estimate) ----

func TestObserveCalibratesTrimBudgetToActualDensity(t *testing.T) {
	c := NewCompactor(nil, 200000)
	base := c.trimBudget // 200000 * 0.78 / 2.0 (default calibration)
	if base < 70000 || base > 86000 {
		t.Fatalf("default budget must be conservative for a 200k window, got %d", base)
	}
	sent := []Message{TextMessage(RoleUser, strings.Repeat("x", 4000))} // ~1000 est tokens
	// Dense content: actual is 3x the estimate → calibration up → budget shrinks.
	c.observe(3000, sent)
	if c.calibration < 2.9 || c.trimBudget >= base {
		t.Fatalf("dense content must raise calibration + shrink the budget, cal=%v budget=%d (base %d)", c.calibration, c.trimBudget, base)
	}
	dense := c.trimBudget
	// Light content: actual ≈ estimate → calibration falls back toward 1.0 → budget grows.
	c.observe(1000, sent)
	if c.calibration != 1.0 || c.trimBudget <= dense {
		t.Fatalf("light content must lower calibration + grow the budget, cal=%v budget=%d", c.calibration, c.trimBudget)
	}
}

func TestLoopStaysUnderActualWindowWithDenseContent(t *testing.T) {
	// End-to-end: dense tool output (actual = 1.8x the chars/4 estimate) must NOT
	// overflow the window. The bug was FitToWindow bounding the estimate while the
	// gateway billed ~1.5x more actual tokens (197k → 200032). Calibration fixes it.
	reg := NewRegistry()
	big := strings.Repeat("payload ", 20000)
	reg.Register(&stubTool{name: "read", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		return Result{Content: big}, nil
	}})
	fm := &densityModel{density: 1.8, toolRounds: 25}
	comp := NewCompactor(fm, 200000)
	loop := &Loop{Model: fm, Registry: reg, Gate: NewGate(nil, nil, false), StepCap: 40, Compactor: comp}

	if _, _, err := loop.Run(context.Background(), "sys", nil, TextMessage(RoleUser, "investigate every job")); err != nil {
		t.Fatalf("loop: %v", err)
	}
	// Every request's ACTUAL prompt (estimate × density) stayed under the window.
	var maxActual int
	for i, req := range fm.gotReqs {
		actual := int(float64(estimateMessages(req.Messages)) * fm.density)
		if actual > comp.window {
			t.Fatalf("round %d: actual prompt %d exceeded the window %d — calibration did not prevent overflow", i, actual, comp.window)
		}
		if actual > maxActual {
			maxActual = actual
		}
	}
	// The trimmer actually engaged (otherwise the test proves nothing) and calibration
	// converged near the real density.
	if maxActual < comp.window/2 {
		t.Fatalf("test did not exercise a near-full window (max actual %d) — increase toolRounds", maxActual)
	}
	if comp.calibration < 1.6 || comp.calibration > 2.0 {
		t.Fatalf("calibration should converge near the real density 1.8, got %v", comp.calibration)
	}
}

// ---- reactive overflow recovery ----

func TestContextOverflowTokens(t *testing.T) {
	over, n := contextOverflowTokens(errors.New("transport error (400 invalid_request): prompt is too long: 202695 tokens > 200000 maximum"))
	if !over || n != 202695 {
		t.Fatalf("must detect overflow + parse the actual count, over=%v n=%d", over, n)
	}
	if over, _ := contextOverflowTokens(errors.New("context_length_exceeded")); !over {
		t.Fatal("must detect the OpenAI-style overflow code")
	}
	if over, n := contextOverflowTokens(errors.New("some other 500 error")); over || n != 0 {
		t.Fatalf("a non-overflow error must not be classified as overflow, over=%v", over)
	}
	if over, _ := contextOverflowTokens(nil); over {
		t.Fatal("nil error is not an overflow")
	}
}

func TestOnOverflowRecalibratesAndShrinksBudget(t *testing.T) {
	c := NewCompactor(nil, 200000)
	before := c.trimBudget
	// The error reported 250000 actual tokens for a conv we estimated at ~1000 → the real
	// ratio is huge, so calibration jumps and the budget shrinks hard for the retry.
	sent := []Message{TextMessage(RoleUser, strings.Repeat("x", 4000))} // ~1000 est
	c.onOverflow(250000, sent)
	if c.calibration <= defaultCalibration || c.trimBudget >= before {
		t.Fatalf("onOverflow must raise calibration + shrink the budget, cal=%v budget=%d (was %d)", c.calibration, c.trimBudget, before)
	}
	// Even without a parseable count, it must still shrink (margin) so the retry differs.
	c2 := NewCompactor(nil, 200000)
	b2 := c2.trimBudget
	c2.onOverflow(0, nil)
	if c2.trimBudget >= b2 {
		t.Fatalf("onOverflow with no count must still shrink the budget, got %d (was %d)", c2.trimBudget, b2)
	}
}

func TestLoopRecoversFromContextOverflow(t *testing.T) {
	// The model rejects the first (too-big) prompt, then succeeds — the loop must
	// recalibrate from the error, trim harder, and retry the same step so the turn
	// completes instead of failing the turn.
	reg := NewRegistry()
	fm := &overflowModel{overflowTimes: 1, err: errors.New("400 invalid_request: prompt is too long: 250000 tokens > 200000 maximum")}
	comp := NewCompactor(fm, 200000)
	var hist []Message
	for i := 0; i < 8; i++ {
		hist = append(hist, toolRound("t"+strconv.Itoa(i), strings.Repeat("data ", 200))...)
	}
	loop := &Loop{Model: fm, Registry: reg, Gate: NewGate(nil, nil, false), StepCap: 40, Compactor: comp}

	produced, _, err := loop.Run(context.Background(), "sys", hist, TextMessage(RoleUser, "go"))
	if err != nil {
		t.Fatalf("an overflow must self-heal and the turn complete, got %v", err)
	}
	if fm.calls < 2 {
		t.Fatalf("expected a retry after the overflow, calls=%d", fm.calls)
	}
	if finalText(produced) != "recovered" {
		t.Fatalf("the turn must finish with the recovered answer, got %q", finalText(produced))
	}
}

func TestLoopGivesUpAfterRepeatedOverflow(t *testing.T) {
	// A genuinely unfittable prompt must surface the error after bounded retries, not loop.
	reg := NewRegistry()
	fm := &overflowModel{overflowTimes: 100, err: errors.New("prompt is too long: 999999 tokens > 200000 maximum")}
	comp := NewCompactor(fm, 200000)
	loop := &Loop{Model: fm, Registry: reg, Gate: NewGate(nil, nil, false), StepCap: 40, Compactor: comp}

	_, _, err := loop.Run(context.Background(), "sys", altHistory(8), TextMessage(RoleUser, "go"))
	if err == nil {
		t.Fatal("repeated overflow must eventually surface the error, not loop forever")
	}
	if fm.calls > maxOverflowRetries+2 {
		t.Fatalf("overflow retries must be bounded, calls=%d (cap %d)", fm.calls, maxOverflowRetries)
	}
}

// ---- ForceCompact (/compact model summary) ----

func TestForceCompactSummarizesAndKeepsAssistantTail(t *testing.T) {
	fm := newFakeModel(asstText("BRIEFING: cost spike on anthropic"))
	c := NewCompactor(fm, 0)
	c.keepRecent = 2
	c.keepTarget = 2
	hist := altHistory(10)
	out, stat, err := c.ForceCompact(context.Background(), hist)
	if err != nil || stat == nil {
		t.Fatalf("force must summarize, stat=%v err=%v", stat, err)
	}
	if stat.Kind != "summary" {
		t.Fatalf("force is the summary path, got kind %q", stat.Kind)
	}
	if fm.calls != 1 {
		t.Fatalf("exactly one summary model call, got %d", fm.calls)
	}
	if out[0].Role != RoleUser || !strings.Contains(out[0].Text(), "BRIEFING") {
		t.Fatalf("first message must be the user-role summary, got %s %q", out[0].Role, out[0].Text())
	}
	if out[1].Role != RoleAssistant {
		t.Fatalf("kept tail must begin with assistant (no consecutive user msgs), got %s", out[1].Role)
	}
	if stat.MessagesAfter >= stat.MessagesBefore {
		t.Fatalf("summary must shrink the transcript, %d→%d", stat.MessagesBefore, stat.MessagesAfter)
	}
}

func TestForceCompactBoundsItsOwnInput(t *testing.T) {
	// At high context the summary call must NOT ingest a near-window prompt (the cause
	// of the "/compact working… 115s" stall): ForceCompact first trims its input.
	fm := newFakeModel(asstText("S"))
	c := NewCompactor(fm, 0)
	c.trimBudget = 250
	c.keepRecent = 2
	c.keepTarget = 20
	var hist []Message
	hist = append(hist, TextMessage(RoleUser, "start"))
	for i := 0; i < 8; i++ {
		hist = append(hist, toolRound("t"+strconv.Itoa(i), strings.Repeat("data ", 80))...)
	}
	if _, _, err := c.ForceCompact(context.Background(), hist); err != nil {
		t.Fatalf("force: %v", err)
	}
	older := fm.gotReqs[0].Messages
	if est := estimateMessages(older); est > c.trimBudget {
		t.Fatalf("the summary call must see a bounded input (%d) under the trim budget %d", est, c.trimBudget)
	}
}

func TestForceCompactModelErrorReturnsHistory(t *testing.T) {
	fm := &fakeModel{errs: []error{errors.New("model down")}}
	c := NewCompactor(fm, 0)
	c.keepRecent = 2
	hist := altHistory(8)
	out, stat, err := c.ForceCompact(context.Background(), hist)
	if err == nil || stat != nil {
		t.Fatal("a summary model error must surface and not claim a compaction")
	}
	if len(out) != 8 {
		t.Fatalf("on error the original history is returned unchanged, got %d", len(out))
	}
}

func TestForceCompactIsInterruptible(t *testing.T) {
	// A cancelled context must tear the summary call down promptly (ESC during /compact).
	fm := &blockingModel{released: make(chan struct{})}
	c := NewCompactor(fm, 0)
	c.keepRecent = 2
	hist := altHistory(8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := c.ForceCompact(ctx, hist)
		done <- err
	}()
	cancel() // interrupt
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an interrupted /compact must return a (cancellation) error, not succeed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ForceCompact did not return promptly after ctx cancel — not interruptible")
	}
}

func TestForceCompactNoOpOnTinyTranscript(t *testing.T) {
	fm := newFakeModel(asstText("S"))
	c := NewCompactor(fm, 0)
	hist := altHistory(2)
	out, stat, err := c.ForceCompact(context.Background(), hist)
	if err != nil || stat != nil {
		t.Fatalf("nothing to summarize must be a no-op, stat=%v", stat)
	}
	if len(out) != 2 || fm.calls != 0 {
		t.Fatalf("no-op leaves history and makes no model call, len=%d calls=%d", len(out), fm.calls)
	}
}

func TestForceCompactAllUserHistoryDeclines(t *testing.T) {
	fm := newFakeModel(asstText("S"))
	c := NewCompactor(fm, 0)
	c.keepRecent = 1
	c.keepTarget = 1
	hist := []Message{TextMessage(RoleUser, "a"), TextMessage(RoleUser, "b"), TextMessage(RoleUser, "c")}
	out, stat, err := c.ForceCompact(context.Background(), hist)
	if err != nil || stat != nil {
		t.Fatalf("all-user history has no safe cut → no-op, stat=%v err=%v", stat, err)
	}
	if len(out) != 3 || fm.calls != 0 {
		t.Fatalf("no-op leaves history, no model call, len=%d calls=%d", len(out), fm.calls)
	}
}

func TestElideMessageElidesEveryBodyKind(t *testing.T) {
	m := Message{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockText, Text: "some analysis"},
		{Type: BlockToolUse, ID: "t1", ToolName: "read", Input: json.RawMessage(`{"big":"args"}`)},
		{Type: BlockToolResult, ID: "t0", Text: "old result body"},
	}}
	// toolResultOnly=true → only the tool_result body is elided.
	only, did := elideMessage(m, true)
	if !did || only.Blocks[2].Text != elidedPlaceholder {
		t.Fatal("toolResultOnly must elide the tool_result body")
	}
	if only.Blocks[0].Text != "some analysis" || only.Blocks[1].Input == nil {
		t.Fatal("toolResultOnly must leave text + tool-call args untouched")
	}
	// toolResultOnly=false → text + tool-call args are elided too.
	all, did := elideMessage(m, false)
	if !did || all.Blocks[0].Text != elidedPlaceholder || all.Blocks[1].Input != nil {
		t.Fatalf("full elide must drop text + tool-call args, got %+v", all.Blocks)
	}
}

func TestFitToWindowNoOpWhenSingleMessageCannotShrink(t *testing.T) {
	// A single message is always the protected tail (pass 2 keeps the last message), so
	// there is nothing elidable even though it exceeds the budget — a clean no-op.
	c := NewCompactor(nil, 0)
	c.trimBudget = 5
	hist := []Message{TextMessage(RoleUser, strings.Repeat("z", 400))}
	out, stat := c.FitToWindow(hist)
	if stat != nil {
		t.Fatalf("a lone message has nothing to elide → no-op, got %v", stat)
	}
	if len(out) != 1 {
		t.Fatal("the message must be returned unchanged")
	}
}

func TestForceCompactKeepRecentClampedToOne(t *testing.T) {
	// keepRecent < 1 is clamped so a summary always keeps at least one recent message.
	fm := newFakeModel(asstText("S"))
	c := NewCompactor(fm, 0)
	c.keepRecent = 0
	c.keepTarget = 1
	out, stat, err := c.ForceCompact(context.Background(), altHistory(8))
	if err != nil || stat == nil {
		t.Fatalf("must summarize, stat=%v err=%v", stat, err)
	}
	if len(out) < 2 {
		t.Fatalf("clamp must keep summary + at least one recent message, got %d", len(out))
	}
}

// assertToolPairsIntact verifies every tool_result's id has a matching tool_use
// earlier in the conversation (no orphaned tool_result after trimming).
func assertToolPairsIntact(t *testing.T, msgs []Message) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == BlockToolUse {
				seen[b.ID] = true
			}
			if b.Type == BlockToolResult && !seen[b.ID] {
				t.Fatalf("orphaned tool_result %q has no preceding tool_use", b.ID)
			}
		}
	}
}
