package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// askFake drives the ask bar's two LLM calls deterministically: it returns the
// route JSON when the request carries the router system prompt, otherwise the
// answer prose. Read methods come from the embedded sampleGateway.
type askFake struct {
	*fakeGateway
	route     string
	answer    string
	streamErr error // fails the router ChatStream call (distinct from read-method fakeGateway.err)
	answerErr error // fails only the second (summary) ChatStream call
}

func newAskFake(route, answer string) askFake {
	return askFake{fakeGateway: sampleGateway(), route: route, answer: answer}
}

func (f askFake) ChatStream(_ context.Context, _ string, req core.ChatRequest, onDelta func(string)) (*core.ChatUsage, error) {
	isRouter := false
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "Convert the user's question into ONE JSON object") {
			isRouter = true
			break
		}
	}
	if isRouter {
		if f.streamErr != nil {
			return nil, f.streamErr
		}
		if onDelta != nil && f.route != "" {
			onDelta(f.route)
		}
		return nil, nil
	}
	if f.answerErr != nil {
		return nil, f.answerErr
	}
	if onDelta != nil && f.answer != "" {
		onDelta(f.answer)
	}
	return nil, nil
}

func entriesFixture() []viewEntry {
	return []viewEntry{{name: "Radar"}, {name: "Cost"}, {name: "SLO"}}
}

// pump runs the ask bar's state machine to completion: it folds the bar's own
// internal messages (route/data/answer deltas + dones) and stops at the first
// externally-routed message (navigateMsg / openEventMsg / askCloseMsg) or when no
// command remains. Returns the final bar and the last message produced.
func pump(a askBar, cmd tea.Cmd) (askBar, tea.Msg) {
	var last tea.Msg
	for cmd != nil {
		msg := cmd()
		last = msg
		switch msg.(type) {
		case askRouteDeltaMsg, askRouteDoneMsg, askDataMsg, askAnswerDeltaMsg, askAnswerDoneMsg:
			a, cmd = a.Update(msg)
		default:
			return a, msg
		}
	}
	return a, last
}

// submit types q and presses enter, returning the bar + the first command.
func submit(a askBar, q string) (askBar, tea.Cmd) {
	a.input.SetValue(q)
	return a.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

func TestAskBarNotReadyGate(t *testing.T) {
	a := newAskBar(newAskFake("", ""), Session{}, entriesFixture()) // no model/VK
	a, cmd := submit(a, "anything")
	if a.phase != askErrorPhase || !strings.Contains(a.notice, "Virtual Key") {
		t.Fatalf("expected not-ready gate, got phase=%d notice=%q", a.phase, a.notice)
	}
	if cmd != nil {
		t.Fatal("not-ready submit should not start a stream")
	}
}

func TestAskBarEmptyQuestionIgnored(t *testing.T) {
	a := newAskBar(newAskFake("", ""), testSession(), entriesFixture())
	a, cmd := submit(a, "   ")
	if cmd != nil || a.phase != askIdle {
		t.Fatalf("blank question should be a no-op, got phase=%d cmd=%v", a.phase, cmd)
	}
}

func TestAskBarNavigateEmitsNavigateMsg(t *testing.T) {
	a := newAskBar(newAskFake(`{"action":"navigate","view":"Cost"}`, ""), testSession(), entriesFixture())
	a, cmd := submit(a, "show me cost")
	a, msg := pump(a, cmd)
	nav, ok := msg.(navigateMsg)
	if !ok || nav.view != "Cost" {
		t.Fatalf("expected navigateMsg{Cost}, got %#v", msg)
	}
}

func TestAskBarNavigateCarriesFilterToRoot(t *testing.T) {
	route := `{"action":"navigate","view":"Radar","filter":{"provider":"openai","status":"5xx"}}`
	a := newAskBar(newAskFake(route, ""), testSession(), entriesFixture())
	a, cmd := submit(a, "5xx from openai")
	_, msg := pump(a, cmd)
	nav, ok := msg.(navigateMsg)
	if !ok || nav.view != "Radar" || nav.filter == nil || nav.filter.Provider != "openai" {
		t.Fatalf("expected navigateMsg with openai filter, got %#v", msg)
	}
}

func TestAskBarExplainEmitsOpenEvent(t *testing.T) {
	a := newAskBar(newAskFake(`{"action":"explain","event_id":"evt-9"}`, ""), testSession(), entriesFixture())
	a, cmd := submit(a, "why did evt-9 fail")
	_, msg := pump(a, cmd)
	oe, ok := msg.(openEventMsg)
	if !ok || oe.id != "evt-9" || !oe.explain {
		t.Fatalf("expected openEventMsg{evt-9, explain}, got %#v", msg)
	}
}

func TestAskBarAnswerStreams(t *testing.T) {
	a := newAskBar(newAskFake(`{"action":"answer","source":"cost"}`, "OpenAI leads at $42."), testSession(), entriesFixture())
	a, cmd := submit(a, "most expensive provider?")
	a, _ = pump(a, cmd)
	if !strings.Contains(a.answer, "OpenAI leads") {
		t.Fatalf("answer not streamed: %q", a.answer)
	}
	if a.phase != askIdle {
		t.Fatalf("expected idle after answer, got %d", a.phase)
	}
	if !strings.Contains(a.View(), "OpenAI leads") {
		t.Fatalf("answer should render in the view:\n%s", a.View())
	}
}

func TestAskBarAnswerEachSource(t *testing.T) {
	for _, src := range []string{"cost", "errors", "slo", "fleet"} {
		route := `{"action":"answer","source":"` + src + `"}`
		a := newAskBar(newAskFake(route, "summary for "+src), testSession(), entriesFixture())
		a, cmd := submit(a, "tell me about "+src)
		a, _ = pump(a, cmd)
		if a.phase == askErrorPhase {
			t.Fatalf("source %q errored: %s", src, a.notice)
		}
		if !strings.Contains(a.answer, "summary for "+src) {
			t.Fatalf("source %q did not stream its answer: %q", src, a.answer)
		}
	}
}

func TestAskBarAnswerFetchError(t *testing.T) {
	f := newAskFake(`{"action":"answer","source":"cost"}`, "x")
	f.fakeGateway.err = errChat // ByProvider/Cost now fail
	a := newAskBar(f, testSession(), entriesFixture())
	a, cmd := submit(a, "spend?")
	a, _ = pump(a, cmd)
	if a.phase != askErrorPhase || !strings.Contains(a.notice, "fetch failed") {
		t.Fatalf("expected fetch-failed error, got phase=%d notice=%q", a.phase, a.notice)
	}
}

func TestAskBarAnswerStreamError(t *testing.T) {
	f := newAskFake(`{"action":"answer","source":"cost"}`, "x")
	f.answerErr = errChat // route succeeds, the summary call fails
	a := newAskBar(f, testSession(), entriesFixture())
	a, cmd := submit(a, "spend?")
	a, _ = pump(a, cmd)
	if a.phase != askErrorPhase || !strings.Contains(a.notice, errChat.Error()) {
		t.Fatalf("expected answer-stream error surfaced, got phase=%d notice=%q", a.phase, a.notice)
	}
}

func TestFetchAnswerData(t *testing.T) {
	ctx := context.Background()
	for _, src := range []answerSource{sourceCost, sourceErrors, sourceSLO, sourceFleet} {
		data, err := fetchAnswerData(ctx, sampleGateway(), src)
		if err != nil {
			t.Fatalf("source %q: unexpected error %v", src, err)
		}
		if !json.Valid(data) {
			t.Fatalf("source %q: produced invalid JSON: %s", src, data)
		}
	}
	// Each source surfaces a read error.
	for _, src := range []answerSource{sourceCost, sourceErrors, sourceSLO, sourceFleet} {
		if _, err := fetchAnswerData(ctx, &fakeGateway{err: errChat}, src); err == nil {
			t.Fatalf("source %q: expected read error to propagate", src)
		}
	}
	// An unknown source is a programming error, not a silent empty answer.
	if _, err := fetchAnswerData(ctx, sampleGateway(), answerSource("bogus")); err == nil {
		t.Fatal("unknown source should error")
	}
}

func TestAskBarUnknownShowsHint(t *testing.T) {
	a := newAskBar(newAskFake(`garbage`, ""), testSession(), entriesFixture())
	a, cmd := submit(a, "???")
	a, _ = pump(a, cmd)
	if a.phase != askErrorPhase || !strings.Contains(a.notice, "palette") {
		t.Fatalf("expected unknown hint, got phase=%d notice=%q", a.phase, a.notice)
	}
}

func TestAskBarRouteError(t *testing.T) {
	f := newAskFake(`{"action":"navigate","view":"Cost"}`, "")
	f.streamErr = errChat // ChatStream itself fails
	a := newAskBar(f, testSession(), entriesFixture())
	a, cmd := submit(a, "cost")
	a, _ = pump(a, cmd)
	if a.phase != askErrorPhase || !strings.Contains(a.notice, "routing failed") {
		t.Fatalf("expected routing-failed error, got phase=%d notice=%q", a.phase, a.notice)
	}
}

func TestAskBarFetchCancelOnDismiss(t *testing.T) {
	a := newAskBar(newAskFake(`{"action":"answer","source":"cost"}`, "ans"), testSession(), entriesFixture())
	a, cmd := submit(a, "spend?")
	// Drive only the route messages to reach the answer-fetch phase.
	a, _ = a.Update(cmd())   // askRouteDeltaMsg
	a, cmd = a.Update(askRouteDoneMsg{}) // finishRoute -> answer arm sets fetchCancel
	if a.fetchCancel == nil || a.phase != askAnswering {
		t.Fatalf("answer arm should set a cancelable fetch, phase=%d cancel=%v", a.phase, a.fetchCancel)
	}
	// esc must tear the in-flight fetch down.
	a, _ = a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.fetchCancel != nil {
		t.Fatal("esc should cancel and clear the in-flight fetch")
	}
}

func TestAskBarEscCloses(t *testing.T) {
	a := newAskBar(newAskFake("", ""), testSession(), entriesFixture())
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit a close command")
	}
	if _, ok := cmd().(askCloseMsg); !ok {
		t.Fatalf("esc should emit askCloseMsg, got %#v", cmd())
	}
}

func TestAskBarIgnoresTypingWhileRouting(t *testing.T) {
	a := newAskBar(newAskFake(`{"action":"navigate","view":"Cost"}`, ""), testSession(), entriesFixture())
	a.phase = askRouting
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd != nil {
		t.Fatal("typing while routing must be ignored")
	}
	// enter while routing is also ignored.
	_, cmd = a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("enter while routing must be ignored")
	}
}

func TestAskBarTypingEditsInput(t *testing.T) {
	a := newAskBar(newAskFake("", ""), testSession(), entriesFixture())
	a, _ = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")})
	if a.input.Value() != "hi" {
		t.Fatalf("typing while idle should edit the input, got %q", a.input.Value())
	}
}

// noWriteGateway fails the test if any mutating Gateway method is called. It
// embeds the read/stream askFake so only the write methods are overridden. This
// is the structural proof that the ask bar can never autonomously mutate state.
type noWriteGateway struct {
	askFake
	t *testing.T
}

func (g noWriteGateway) SetKillSwitch(context.Context, bool) (*core.KillSwitchResult, error) {
	g.t.Error("ask bar must never call SetKillSwitch")
	return nil, nil
}
func (g noWriteGateway) SetProviderEnabled(context.Context, string, bool) error {
	g.t.Error("ask bar must never call SetProviderEnabled")
	return nil
}
func (g noWriteGateway) CacheFlush(context.Context) error {
	g.t.Error("ask bar must never call CacheFlush")
	return nil
}
func (g noWriteGateway) RevokeVK(context.Context, string) error {
	g.t.Error("ask bar must never call RevokeVK")
	return nil
}
func (g noWriteGateway) RegenerateVK(context.Context, string) (*core.RegeneratedVK, error) {
	g.t.Error("ask bar must never call RegenerateVK")
	return nil, nil
}
func (g noWriteGateway) SetRoutingRuleEnabled(context.Context, string, bool) error {
	g.t.Error("ask bar must never call SetRoutingRuleEnabled")
	return nil
}
func (g noWriteGateway) SetPassthroughGlobal(context.Context, core.PassthroughGlobalRequest) error {
	g.t.Error("ask bar must never call SetPassthroughGlobal")
	return nil
}

func TestAskBarNeverWrites(t *testing.T) {
	routes := []string{
		`{"action":"navigate","view":"Cost"}`,
		`{"action":"navigate","view":"Radar","filter":{"provider":"openai","status":"5xx"}}`,
		`{"action":"answer","source":"cost"}`,
		`{"action":"answer","source":"errors"}`,
		`{"action":"answer","source":"slo"}`,
		`{"action":"answer","source":"fleet"}`,
		`{"action":"explain","event_id":"e1"}`,
		`{"action":"disable_provider","view":"x"}`, // hallucinated write -> unknown
		`{"action":"revoke","event_id":"vk1"}`,     // hallucinated write -> unknown
		`garbage`,
	}
	for _, r := range routes {
		gw := noWriteGateway{askFake: newAskFake(r, "summary"), t: t}
		a := newAskBar(gw, testSession(), entriesFixture())
		a, cmd := submit(a, "do something")
		// Pump every produced command + folded message to completion, exercising the
		// answer path's read fetch + summary stream as well.
		_, _ = pump(a, cmd)
	}
}

func TestAskBarViewPhases(t *testing.T) {
	a := newAskBar(newAskFake("", ""), testSession(), entriesFixture())
	if !strings.Contains(a.View(), "Ask Nexus") || !strings.Contains(a.View(), "enter to ask") {
		t.Fatalf("idle view missing chrome:\n%s", a.View())
	}
	a.phase = askRouting
	if !strings.Contains(a.View(), "routing") {
		t.Fatalf("routing view missing status:\n%s", a.View())
	}
	a.phase = askAnswering
	if !strings.Contains(a.View(), "thinking") {
		t.Fatalf("answering view missing placeholder:\n%s", a.View())
	}
	a.phase = askErrorPhase
	a.notice = "boom"
	if !strings.Contains(a.View(), "boom") {
		t.Fatalf("error view missing notice:\n%s", a.View())
	}
}
