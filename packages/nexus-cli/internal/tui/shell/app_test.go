package shell

import (
	viewpkg "github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/views"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	capabilities "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/runtime"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

// newTestModel builds a root Model whose conversation drives a scripted agent
// (mirrors newTestConv at the root level so a full turn can be pumped through the
// root Update). The recordingRunner exposes what Turn saw for assertions.
func newTestModel(s Session, script convScript) (Model, *recordingRunner) {
	rr := &recordingRunner{script: script}
	build := func(canvas capabilities.Canvas, confirm agent.ConfirmFunc, onText, onReasoning func(string), onTool func(string, []byte), _ func(string, []byte, bool), _ func(agent.ContextStats, int), onCompact func(agent.CompactStat)) (AgentRunner, error) {
		rr.onText, rr.onReasoning, rr.onTool, rr.confirm = onText, onReasoning, onTool, confirm
		rr.onCompact = onCompact
		rr.canvasOK = canvas != nil
		return rr, nil
	}
	return NewModel(sampleGateway(), s, build), rr
}

// updateModel folds one message and re-asserts the concrete Model type.
func updateModel(m Model, msg tea.Msg) (Model, tea.Cmd) {
	tm, cmd := m.Update(msg)
	return tm.(Model), cmd
}

// pumpModel drives a non-blocking agent flow through the root Update until the
// turn completes (or a safety cap), returning the final model.
func pumpModel(m Model, cmd tea.Cmd) Model {
	for i := 0; cmd != nil && i < 200; i++ {
		msg := cmd()
		if msg == nil {
			return m
		}
		var tm tea.Model
		tm, cmd = m.Update(msg)
		m = tm.(Model)
		if _, done := msg.(agentDoneMsg); done {
			return m
		}
	}
	return m
}

func TestInputCursor_AtPromptWhenChatFocused(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m.width, m.height = 100, 40
	v := m.View()
	if !v.AltScreen {
		t.Fatal("the root view must request the alt-screen")
	}
	if v.Cursor == nil {
		t.Fatal("chat-focused view must position the real cursor at the input (the IME anchor)")
	}
	if v.Cursor.Y <= 0 {
		t.Fatalf("cursor should be on a content row near the bottom, got y=%d", v.Cursor.Y)
	}
	emptyX := v.Cursor.X
	// CJK advances the cursor by DISPLAY width (2 cells/char), not rune count — this
	// is what makes the IME candidate window anchor at the cursor, not the line start.
	m.conv.input.SetValue("你好") // 2 CJK runes = 4 display cells
	m.conv.input.SetCursor(2)
	x4 := m.View().Cursor.X
	if x4-emptyX != 4 {
		t.Fatalf("two CJK chars must move the cursor 4 display cells, got Δ=%d", x4-emptyX)
	}
	// ASCII advances one cell per rune.
	m.conv.input.SetValue("ab")
	m.conv.input.SetCursor(2)
	if x2 := m.View().Cursor.X; x2-emptyX != 2 {
		t.Fatalf("two ASCII chars must move the cursor 2 cells, got Δ=%d", x2-emptyX)
	}
	// Focusing the canvas blurs the prompt → no chat cursor (IME won't anchor there).
	cm, _ := m.focusCanvas()
	m = cm.(Model)
	m.width, m.height = 100, 40
	if m.View().Cursor != nil {
		t.Fatal("canvas-focused view must not show the chat cursor")
	}
}

// TestInputCursor_AnchorsAtPromptDuringReasoning reproduces the reported bug: while
// a long reasoning ("think") block streams, the hardware cursor must still sit on the
// convPromptPrefix prompt row, not drift up into the transcript. It pins the computed cursor
// row to the actual rendered prompt row, which catches the line-count drift that a
// mis-wrapped think block caused.
func TestInputCursor_AnchorsAtPromptDuringReasoning(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m.width, m.height = 120, 40
	// Simulate a turn mid-reasoning: a long, multi-line think block + a running turn.
	m.conv.running = true
	m.conv.appendLine("you", "investigate the alert")
	long := strings.Repeat("the model considers many options across several wrapped lines and paragraphs.\n", 12)
	m.conv.appendLine("think", long)
	m.conv.appendLine("tool", "▸ resource_describe")
	m.conv.input.SetValue("A")
	m.conv.input.SetCursor(1)

	v := m.View()
	if v.Cursor == nil {
		t.Fatal("the chat cursor must be positioned while chatting")
	}
	lines := strings.Split(v.Content, "\n")
	promptRow := -1
	for i, l := range lines {
		if strings.Contains(l, convPromptPrefix) {
			promptRow = i
		}
	}
	if promptRow < 0 {
		t.Fatalf("the prompt must render:\n%s", v.Content)
	}
	if v.Cursor.Y != promptRow {
		t.Fatalf("cursor Y=%d must equal the prompt row %d (it drifted into the transcript)", v.Cursor.Y, promptRow)
	}
}

func TestView_ChatAlwaysVisible(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m.width, m.height = 100, 40
	out := m.View().Content
	if !strings.Contains(out, convPromptPrefix) {
		t.Fatalf("the resident chat prompt must always render (no toggle):\n%s", out)
	}
}

func TestView_TopAndBottomBothRender(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m.width, m.height = 100, 40
	out := m.View().Content
	if !strings.Contains(out, "nexus") { // breadcrumb/canvas title line
		t.Fatalf("top canvas must render:\n%s", out)
	}
	if !strings.Contains(out, convPromptPrefix) {
		t.Fatalf("bottom chat must render in the same frame:\n%s", out)
	}
}

func TestTab_TogglesPaneFocus(t *testing.T) {
	m := NewModel(sampleGateway(), testSession()) // launches focusChat
	if m.focus != focusChat {
		t.Fatalf("dashboard must launch focused on chat, got %v", m.focus)
	}
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = upd.(Model)
	if m.focus != focusCanvas {
		t.Fatalf("tab from chat must focus the canvas, got %v", m.focus)
	}
	upd, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = upd.(Model)
	if m.focus != focusChat {
		t.Fatalf("tab from canvas must focus the chat, got %v", m.focus)
	}
}

func TestFocusToggle_EasesSplit(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	upd, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyTab}) // focus canvas, start the ease
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("a focus toggle should start the split ease (return a splitTick cmd)")
	}
	// Drive the ease to completion: each splitTick advances the frame and re-issues
	// until it settles at easeFrames (then returns nil).
	for i := 0; i < easeFrames+3 && cmd != nil; i++ {
		upd, cmd = m.Update(cmd())
		m = upd.(Model)
	}
	if m.easeFrame < easeFrames {
		t.Fatalf("the ease should settle at easeFrames, got %d", m.easeFrame)
	}
}

func TestChatFocus_RoutesTypingToConversation(t *testing.T) {
	m := NewModel(sampleGateway(), testSession()) // focusChat by default
	upd, _ := m.Update(keyRunes("h"))
	m = upd.(Model)
	upd, _ = m.Update(keyRunes("i"))
	m = upd.(Model)
	if !strings.Contains(m.conv.input.Value(), "hi") {
		t.Fatalf("chat-focused typing must reach the conversation input, got %q", m.conv.input.Value())
	}
}

func TestCanvasFocus_NumberJumpsView(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab}) // focus canvas
	m = upd.(Model)
	upd, _ = m.Update(keyRunes("2"))
	m = upd.(Model)
	if m.active != 1 {
		t.Fatalf("canvas-focused '2' should jump to view index 1, got %d", m.active)
	}
}

func TestStatusBar_NonProdHasNoTopBar(t *testing.T) {
	m := NewModel(sampleGateway(), testSession()) // non-prod
	if got := m.statusBar(100); got != "" {
		t.Fatalf("non-prod must have no top status bar (env lives bottom-right), got %q", got)
	}
}

func TestStatusBar_ProdKeepsBanner(t *testing.T) {
	m := NewModel(sampleGateway(), testSessionProd())
	if !strings.Contains(m.statusBar(100), "PROD") {
		t.Fatal("prod must keep the red top banner for safety")
	}
}

func TestFooter_EnvBottomRight(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	foot := m.footerBar(100)
	if !strings.Contains(foot, "local") {
		t.Fatalf("env indicator must render bottom-right in the footer:\n%s", foot)
	}
}

func TestFooter_ShowsSpinnerWhenRunning(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m.conv.running = true
	m.conv.startedAt = time.Now()
	foot := m.footerBar(100)
	if !strings.Contains(foot, "working") {
		t.Fatalf("footer must show the working spinner on the left while a turn runs:\n%s", foot)
	}
	if !strings.Contains(foot, "local") {
		t.Fatalf("footer must still show the env indicator on the right:\n%s", foot)
	}
}

func TestSlash_ViewCommandFocusesCanvas(t *testing.T) {
	m := NewModel(sampleGateway(), testSession()) // launches focusChat
	upd, _ := m.handleSlash(slashSelectedMsg{cmd: slashCmd{name: "cost", kind: slashView}})
	m = upd.(Model)
	if m.focus != focusCanvas {
		t.Fatal("a / view command must move focus to the canvas so the result is navigable")
	}
	if m.active != m.indexOf("Cost") {
		t.Fatalf("/cost should open the Cost view, active=%d", m.active)
	}
}

func TestSlash_AgentCommandKeepsChatFocus(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	cm, _ := m.focusCanvas() // start on the canvas
	m = cm.(Model)
	upd, _ := m.handleSlash(slashSelectedMsg{cmd: slashCmd{name: "help", kind: slashAgent}})
	if upd.(Model).focus != focusChat {
		t.Fatal("an agent / command must focus the chat")
	}
}

func TestSlashModel_NoArgOpensPickMode(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	upd, _ := m.handleSlash(slashSelectedMsg{cmd: slashCmd{name: "model", kind: slashView}, arg: ""})
	m = upd.(Model)
	if m.entries[m.active].name != "Models" {
		t.Fatalf("/model with no arg must open the Models view, got %q", m.entries[m.active].name)
	}
	mv := m.views[m.active].(*viewpkg.ModelsView)
	if !mv.Picking() {
		t.Fatal("/model with no arg must put the Models view in pick mode")
	}
}

func TestSlashModel_WithArgSwitchesModel(t *testing.T) {
	applied := ""
	m := NewModel(sampleGateway(), testSession())
	m.applyModel = func(model, vkID, vkName string) error { applied = model; return nil }
	upd, _ := m.handleSlash(slashSelectedMsg{cmd: slashCmd{name: "model", kind: slashView}, arg: "gpt-4o-mini"})
	m = upd.(Model)
	if applied != "gpt-4o-mini" {
		t.Fatalf("/model <slug> must persist the new model via applyModel, got %q", applied)
	}
	if m.session.Model != "gpt-4o-mini" {
		t.Fatalf("/model <slug> must update the session model, got %q", m.session.Model)
	}
	if m.conv.session.Model != "gpt-4o-mini" {
		t.Fatalf("/model <slug> must propagate to the conversation, got %q", m.conv.session.Model)
	}
}

func TestSetModelMsg_SwitchesModel(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	upd, _ := m.Update(kit.SetModelMsg{Code: "claude-haiku"})
	m = upd.(Model)
	if m.session.Model != "claude-haiku" {
		t.Fatalf("setModelMsg must switch the chat model, got %q", m.session.Model)
	}
}

func TestSetChatModel_BroadcastsToSessionViews(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m = m.setChatModel("brand-new-model")
	// The root session + the resident conversation follow the switch; the broadcast
	// to the session-bearing dashboard views is covered by views.TestChat_SetSession*.
	if m.session.Model != "brand-new-model" {
		t.Fatalf("the model switch must update the root session, got %q", m.session.Model)
	}
	if m.conv.session.Model != "brand-new-model" {
		t.Fatalf("the model switch must propagate to the conversation, got %q", m.conv.session.Model)
	}
}

func TestModelBreadcrumbReplacesTabRow(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if !strings.Contains(m.View().Content, "nexus › Overview") {
		t.Fatalf("the breadcrumb trail should head the dashboard:\n%s", m.View().Content)
	}
	// The breadcrumb shows only the active trail — a lateral view's name (Radar)
	// must not appear, proving the old all-views tab row is gone.
	if cb := m.crumbBar(120); strings.Contains(cb, "Radar") {
		t.Fatalf("crumb bar should show only the active trail, got %q", cb)
	}
}

func TestModelIMEGuardSwallowsCJKInNormalMode(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	// NORMAL mode requires canvas focus (chat is the launch default = INPUT).
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyTab})
	if m.currentMode() != modeNormal {
		t.Fatal("a canvas-focused model with no slash/capture should be in NORMAL mode")
	}
	before := m.active
	m, cmd := updateModel(m, keyRunes("中"))
	if m.active != before {
		t.Fatalf("a CJK rune must not change the active view (IME guard), active=%d", m.active)
	}
	if cmd != nil {
		t.Fatal("a swallowed NORMAL-mode key should produce no command")
	}
}

func TestModelSlashOpenViewDispatchAndClose(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m, _ = updateModel(m, keyRunes("/"))
	if !m.slashOpen {
		t.Fatal("'/' should open the slash palette")
	}
	// Selecting a view command switches to that view and closes the palette.
	m, _ = updateModel(m, slashSelectedMsg{cmd: slashCmd{name: "cost", kind: slashView}})
	if m.slashOpen {
		t.Fatal("selecting a command should close the palette")
	}
	if m.active != m.indexOf("Cost") {
		t.Fatalf("/cost should switch to the Cost view, active=%d", m.active)
	}
	// Re-open then dismiss with slashCloseMsg.
	m, _ = updateModel(m, keyRunes("/"))
	m, _ = updateModel(m, slashCloseMsg{})
	if m.slashOpen {
		t.Fatal("slashCloseMsg should close the palette")
	}
}

func TestModelSlashEventArgDrills(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m, _ = updateModel(m, slashSelectedMsg{cmd: slashCmd{name: "event", kind: slashView}, arg: "ev-9a3f"})
	if m.active != m.indexOf("Event") {
		t.Fatalf("/event <id> should drill into the Event view, active=%d", m.active)
	}
	if cb := m.crumbBar(120); !strings.Contains(cb, "ev-9a3f") {
		t.Fatalf("the drilled event id should appear in the breadcrumb, got %q", cb)
	}
}

func TestCrumbBar_GlobalHintResponsive(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	// Wide: the unused top-right shows the global-shortcut strip.
	if wide := m.crumbBar(120); !strings.Contains(wide, "tab pane") {
		t.Fatalf("a wide row should show the global hint top-right, got %q", wide)
	}
	// Narrow: the hint is dropped so it never collides with the breadcrumb.
	if narrow := m.crumbBar(20); strings.Contains(narrow, "tab pane") {
		t.Fatalf("a narrow row must drop the global hint, got %q", narrow)
	}
}

func TestModelSlashAgentOpensConversation(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m, _ = updateModel(m, slashSelectedMsg{cmd: slashCmd{name: "help", kind: slashAgent}})
	if m.focus != focusChat {
		t.Fatal("an agent slash command should focus the resident chat")
	}
	// /help renders the full reference into the transcript as a "help" block.
	m.conv.flushReveal() // /help types out; snap it for the assertion
	help := ""
	for _, ln := range m.conv.lines {
		if ln.tag == "help" {
			help = ln.text
		}
	}
	if !strings.Contains(help, "keys & commands") {
		t.Fatalf("/help should render the reference block into the chat, got lines=%+v", m.conv.lines)
	}
}

func TestModelAgentNavDrivesRadarFilter(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m, _ = updateModel(m, agentNavMsg{view: "radar", filter: core.TrafficFilter{StatusRange: "error"}})
	if m.active != m.indexOf("Radar") {
		t.Fatalf("an agent navigate drive should open the named view, active=%d", m.active)
	}
	r, ok := m.views[m.active].(*viewpkg.Radar)
	if !ok || !r.ErrorsOnly() {
		t.Fatal("the radar filter from the nav drive should be applied (errors-only)")
	}
}

func TestModelAgentShowDrivesEventView(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m, _ = updateModel(m, agentShowMsg{id: "ev-777"})
	if m.active != m.indexOf("Event") {
		t.Fatalf("a show_event drive should drill into the Event view, active=%d", m.active)
	}
	if cb := m.crumbBar(120); !strings.Contains(cb, "ev-777") {
		t.Fatalf("the shown event id should head the breadcrumb, got %q", cb)
	}
}

func TestModelAgentHighlightIsSafeNoop(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	before := m.active
	// No view implements highlighter, so a highlight drive is a best-effort no-op
	// (design §6: the agent never hangs on it) and must not change the view.
	m, _ = updateModel(m, agentHighlightMsg{ref: "row-3"})
	if m.active != before {
		t.Fatal("a highlight drive should not change the active view")
	}
}

func TestModelLeftArrowIsBackAlias(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m.focus = focusCanvas // back/popNav is a canvas-focus action
	m, _ = updateModel(m, slashSelectedMsg{cmd: slashCmd{name: "event", kind: slashView}, arg: "ev-1"})
	if m.active != m.indexOf("Event") || m.nav.depth() != 1 {
		t.Fatalf("a drill should push the nav stack: active=%d depth=%d", m.active, m.nav.depth())
	}
	// left arrow pops back exactly like esc.
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.active != 0 || m.nav.depth() != 0 {
		t.Fatalf("← should pop back to the cockpit like esc: active=%d depth=%d", m.active, m.nav.depth())
	}
}

func TestModelEscPopsNavStackToCockpit(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m.focus = focusCanvas // esc-back/popNav is a canvas-focus action
	// Drill Overview → Event (pushes the nav stack).
	m, _ = updateModel(m, slashSelectedMsg{cmd: slashCmd{name: "event", kind: slashView}, arg: "ev-1"})
	if m.active != m.indexOf("Event") || m.nav.depth() != 1 {
		t.Fatalf("a drill should push the nav stack: active=%d depth=%d", m.active, m.nav.depth())
	}
	// esc pops back to the cockpit.
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.active != 0 || m.nav.depth() != 0 {
		t.Fatalf("esc should pop back to the cockpit: active=%d depth=%d", m.active, m.nav.depth())
	}
	// esc at the root is a no-op (already on the cockpit).
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.active != 0 || cmd != nil {
		t.Fatal("esc at the cockpit root should be a no-op")
	}
}

func TestModelConversationTurnThroughRoot(t *testing.T) {
	m, rr := newTestModel(testSessionLocal(), func(onText func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		onText("spend is up 12%")
		return "spend is up 12%", nil
	})
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	// Chat is resident + focused by default — just type and send.
	if m.focus != focusChat {
		t.Fatal("the chat is focused by default; no toggle needed")
	}
	m.conv.input.SetValue("why is spend up?")
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = pumpModel(m, cmd)
	if rr.gotView != "Overview" {
		t.Fatalf("the active view should be threaded to the agent as turn context, got %q", rr.gotView)
	}
	if rr.gotText != "why is spend up?" {
		t.Fatalf("the typed question should reach the agent, got %q", rr.gotText)
	}
	if !strings.Contains(stripSGR(m.conv.View(60, 20)), "spend is up 12%") {
		t.Fatalf("the agent reply should land in the chat transcript:\n%s", m.conv.View(60, 20))
	}
	// Idle esc moves focus up to the canvas (the chat stays resident).
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.focus != focusCanvas {
		t.Fatal("idle esc should move focus from the chat up to the canvas")
	}
}

func TestModelReasoningRoutedThroughRoot(t *testing.T) {
	// Reasoning deltas must reach the conversation through the root's message
	// routing (regression: agentReasoningMsg was missing from the root's agent-
	// message case, so it fell through to the active canvas view and was dropped).
	m, _ := newTestModel(testSessionLocal(), func(onText, onReasoning func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		onReasoning("checking spend trend")
		onText("up 12%")
		return "up 12%", nil
	})
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m.conv.input.SetValue("why is spend up?")
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = pumpModel(m, cmd)
	out := stripSGR(m.conv.View(80, 24))
	if !strings.Contains(out, "checking spend trend") {
		t.Fatalf("reasoning must reach the chat transcript through the root:\n%s", out)
	}
	if !strings.Contains(out, "up 12%") {
		t.Fatalf("the answer must still render:\n%s", out)
	}
}

func TestRootPaste_InsertsIntoFocusedChatInput(t *testing.T) {
	// A bracketed paste is a tea.PasteMsg, not a key — the root must route it to the
	// focused chat input (regression: PasteMsg fell through to the canvas view, so
	// pasting did nothing in the prompt).
	m := NewModel(sampleGateway(), testSession())
	if m.focus != focusChat {
		t.Fatal("chat is focused by default")
	}
	m, _ = updateModel(m, tea.PasteMsg{Content: "pasted text"})
	if got := m.conv.input.Value(); got != "pasted text" {
		t.Fatalf("paste should insert into the chat prompt, got %q", got)
	}
	// With the canvas focused and no text-capturing view, a paste is dropped (no panic).
	tm, _ := m.focusCanvas()
	m = tm.(Model)
	m, _ = updateModel(m, tea.PasteMsg{Content: "ignored"})
	if strings.Contains(m.conv.input.Value(), "ignored") {
		t.Fatal("a canvas-focused paste must not leak into the chat prompt")
	}
}

func TestRootEsc_InterruptsRunningTurnKeepsChatFocus(t *testing.T) {
	m, _ := newTestModel(testSessionLocal(), func(onText func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		onText("partial")
		return "partial", nil
	})
	m.conv.input.SetValue("hello")
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter}) // start a turn (running)
	if !m.conv.running {
		t.Fatal("enter should start a running turn")
	}
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc}) // chat-focused esc while running
	if m.focus != focusChat {
		t.Fatal("esc-interrupt must keep focus on the chat, not move to the canvas")
	}
	if !m.conv.interrupted {
		t.Fatal("esc should mark the running turn interrupted")
	}
}

func TestSplitBody_TopAndBottomStack(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	// Both panes render: the conversation prompt below the active view.
	body := m.splitBody(120, 30)
	if !strings.Contains(body, convPromptPrefix) {
		t.Fatalf("the resident chat must render in the split body:\n%s", body)
	}
}

func TestModelCurrentModeTransitions(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	// Chat is the launch default → INPUT mode.
	if m.currentMode() != modeInput {
		t.Fatal("default focus is chat → INPUT mode")
	}
	// Focus the canvas → NORMAL mode.
	cm, _ := m.focusCanvas()
	m = cm.(Model)
	if m.currentMode() != modeNormal {
		t.Fatal("canvas focus with no capture/slash is NORMAL mode")
	}
	m.slashOpen = true
	if m.currentMode() != modeInput {
		t.Fatal("an open slash palette is INPUT mode")
	}
	m.slashOpen = false
	// A text-capturing view (Chat) is INPUT mode even when canvas-focused.
	jm, _ := m.jumpTop(m.indexOf("Chat"))
	m = jm.(Model)
	if m.currentMode() != modeInput {
		t.Fatal("a text-capturing view (Chat) is INPUT mode")
	}
}

func TestModelHelpTextPrecedence(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	// The chat owns the keybar while focused (the launch default).
	if !strings.Contains(m.helpText(), "send") {
		t.Fatalf("a focused chat should own the keybar, got %q", m.helpText())
	}
	// Focus the canvas: Radar supplies no Help() → the default keybar.
	cm, _ := m.focusCanvas()
	m = cm.(Model)
	rm, _ := m.jumpTop(m.indexOf("Radar"))
	m = rm.(Model)
	if !strings.Contains(m.helpText(), "/ commands") {
		t.Fatalf("a canvas view without its own keybar should fall back to the default, got %q", m.helpText())
	}
	// Kill supplies its own keybar.
	km, _ := m.jumpTop(m.indexOf("Kill"))
	m = km.(Model)
	if !strings.Contains(m.helpText(), "kill-switch on/off") {
		t.Fatalf("Kill should supply its own keybar, got %q", m.helpText())
	}
}

// TestModel_AgentConfirmForcesChatFocus guards the confirm-visibility fix: a
// mitigation confirm must force focus to the resident chat so the typed-confirm
// gate is on a typeable surface even if the operator was navigating the canvas.
func TestModel_AgentConfirmForcesChatFocus(t *testing.T) {
	m := NewModel(sampleGateway(), testSessionProd())
	cm, _ := m.focusCanvas() // operator was on the canvas
	m = cm.(Model)
	m, _ = updateModel(m, agentConfirmMsg{tool: "mitigate_cache_flush", reason: "flush the cache"})
	if m.focus != focusChat {
		t.Fatal("a mitigation confirm must force focus to the chat")
	}
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if !strings.Contains(m.View().Content, "mitigate_cache_flush") {
		t.Fatalf("the allow/deny gate should render in the focused chat:\n%s", m.View().Content)
	}
}

func TestModelJumpMsgRoutes(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	// Drill away from the cockpit, then a jumpMsg (e.g. Chat's esc) returns home.
	m, _ = updateModel(m, slashSelectedMsg{cmd: slashCmd{name: "cost", kind: slashView}})
	m, _ = updateModel(m, kit.JumpMsg{Index: 0})
	if m.active != 0 {
		t.Fatalf("kit.JumpMsg{0} should return to the cockpit, active=%d", m.active)
	}
}
