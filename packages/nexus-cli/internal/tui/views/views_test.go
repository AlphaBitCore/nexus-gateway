package views

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
	"strings"
	"testing"
)

func TestSLO_FetchView(t *testing.T) {
	s := newSLO(sampleGateway())
	if !strings.Contains(s.View(100, 20), "loading") {
		t.Fatal("initial SLO shows loading")
	}
	v, cmd := s.Update(s.Init()())
	if cmd == nil {
		t.Fatal("SLO Update should schedule a poll tick")
	}
	out := v.View(120, 30)
	for _, want := range []string{"Availability", "Per-provider latency", "OpenAI", "Routing fallbacks", "Passthrough"} {
		if !strings.Contains(out, want) {
			t.Errorf("SLO view missing %q:\n%s", want, out)
		}
	}
	// tick refetches.
	if _, c := v.Update(sloTick{}); c == nil {
		t.Fatal("sloTick should refetch")
	}
}

func TestSLO_Error(t *testing.T) {
	s := newSLO(&fakeGateway{err: errors.New("slo-down")})
	v, _ := s.Update(s.Init()())
	if !strings.Contains(v.View(80, 20), "slo-down") {
		t.Fatal("SLO should surface fetch error")
	}
}

func TestSLO_Helpers(t *testing.T) {
	if sloLatencyColor(40000) != styles.Red || sloLatencyColor(9000) != styles.Amber || sloLatencyColor(100) != styles.Green {
		t.Fatal("sloLatencyColor RAG thresholds wrong")
	}
}

func TestCost_FetchView(t *testing.T) {
	c := newCost(sampleGateway())
	if !strings.Contains(c.View(100, 20), "loading") {
		t.Fatal("initial cost shows loading")
	}
	v, cmd := c.Update(c.Init()())
	if cmd == nil {
		t.Fatal("cost Update should schedule a poll tick")
	}
	out := v.View(120, 30)
	for _, want := range []string{"Cache saved", "$2.1100", "Burn rate", "Top providers", "OpenAI", "total $1.3373"} {
		if !strings.Contains(out, want) {
			t.Errorf("cost view missing %q:\n%s", want, out)
		}
	}
	if _, cc := v.Update(costTick{}); cc == nil {
		t.Fatal("costTick should refetch")
	}
}

func TestCost_Error(t *testing.T) {
	c := newCost(&fakeGateway{err: errors.New("cost-down")})
	v, _ := c.Update(c.Init()())
	if !strings.Contains(v.View(80, 20), "cost-down") {
		t.Fatal("cost should surface fetch error")
	}
}

func TestChat_SendStreamsAndFinishes(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	if !c.ready() || !c.Capturing() {
		t.Fatal("chat with a model+VK should be ready and capturing")
	}
	// type a prompt
	v, _ := c.Update(keyRunes("hi"))
	c = v.(*chat)
	// enter sends → returns the waitDelta cmd
	v, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if !c.streaming || cmd == nil {
		t.Fatal("enter should start streaming")
	}
	// drain: first the delta, then the done frame
	msg := cmd() // chatDeltaMsg{"Hello world"}
	if dm, ok := msg.(chatDeltaMsg); !ok || dm.text != "Hello world" {
		t.Fatalf("first msg should be the content delta, got %#v", msg)
	}
	v, cmd2 := c.Update(msg)
	c = v.(*chat)
	done := cmd2() // chatDoneMsg
	if _, ok := done.(chatDoneMsg); !ok {
		t.Fatalf("second msg should be done, got %#v", done)
	}
	v, _ = c.Update(done)
	c = v.(*chat)
	if c.streaming {
		t.Fatal("chat should stop streaming after done")
	}
	out := c.View(120, 30)
	for _, want := range []string{"Hello world", "tokens=19", "asst"} {
		if !strings.Contains(out, want) {
			t.Errorf("chat transcript missing %q:\n%s", want, out)
		}
	}
}

func TestChat_NotReady(t *testing.T) {
	c := newChat(sampleGateway(), kit.Session{EnvName: "local"}) // no model/VK
	if c.ready() {
		t.Fatal("chat without model/VK is not ready")
	}
	if !strings.Contains(c.View(100, 20), "Select a model") {
		t.Fatal("not-ready chat should prompt to select a model/VK")
	}
	// enter must not start streaming when not ready
	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil || c.streaming {
		t.Fatal("enter when not ready should not stream")
	}
}

func TestChat_IgnoresInputWhileStreaming(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	c.streaming = true
	_, cmd := c.Update(keyRunes("x"))
	if cmd != nil {
		t.Fatal("keystrokes ignored while streaming")
	}
	if !strings.Contains(c.Help(), "streaming") {
		t.Fatal("help should reflect streaming state")
	}
}

func TestChat_StreamError(t *testing.T) {
	c := newChat(&fakeGateway{err: errors.New("vk invalid")}, testSession())
	c.Update(keyRunes("hi"))
	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msg := cmd() // deltaCh closed immediately (err) → done
	v, _ := c.Update(msg)
	c = v.(*chat)
	if !strings.Contains(c.View(120, 30), "vk invalid") {
		t.Fatalf("stream error should appear in transcript:\n%s", c.View(120, 30))
	}
}

func TestLab_GeneratorBurst(t *testing.T) {
	l := newLab(sampleGateway(), testSession())
	out := l.View(120, 30)
	if !strings.Contains(out, "Synthetic generator") || !strings.Contains(out, "press g") {
		t.Fatalf("lab idle view wrong:\n%s", out)
	}
	v, cmd := l.Update(keyRunes("g"))
	l = v.(*labView)
	if !l.genRunning || cmd == nil {
		t.Fatal("g should start the generator burst")
	}
	// drain all burst results
	for l.genOK+l.genFail < l.genTotal {
		msg := cmd()
		v, cmd = l.Update(msg)
		l = v.(*labView)
	}
	if l.genRunning || l.genOK != generatorBurstSize {
		t.Fatalf("after draining, generator should be done with all OK: ok=%d fail=%d", l.genOK, l.genFail)
	}
	if !strings.Contains(l.View(120, 30), "DONE") {
		t.Fatal("finished generator should show DONE")
	}
}

func TestLab_RequestLab(t *testing.T) {
	l := newLab(sampleGateway(), testSession())
	v, cmd := l.Update(keyRunes("r"))
	l = v.(*labView)
	if !l.labBusy || cmd == nil {
		t.Fatal("r should send the lab request")
	}
	v, _ = l.Update(cmd())
	l = v.(*labView)
	out := l.View(120, 40)
	if !strings.Contains(out, "response:") || !strings.Contains(out, "total_tokens") {
		t.Errorf("lab should show the forwarded response:\n%s", out)
	}
}

func TestLab_EditAndInvalidJSON(t *testing.T) {
	l := newLab(sampleGateway(), testSession())
	// enter edit mode
	v, _ := l.Update(keyRunes("e"))
	l = v.(*labView)
	if !l.Capturing() {
		t.Fatal("e should enter edit mode (capturing)")
	}
	// esc leaves edit mode
	v, _ = l.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	l = v.(*labView)
	if l.Capturing() {
		t.Fatal("esc should exit edit mode")
	}
	// break the body, then ctrl+s → invalid JSON error
	l.editor.SetValue("{not json")
	v, _ = l.Update(keyRunes("e"))
	l = v.(*labView)
	_, cmd := l.updateEditing(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	v, _ = l.Update(cmd())
	l = v.(*labView)
	if l.labErr == nil || !strings.Contains(l.labErr.Error(), "valid JSON") {
		t.Fatalf("invalid JSON should surface an error, got %v", l.labErr)
	}
}

func TestLab_RouteDryRun(t *testing.T) {
	g := sampleGateway()
	g.route = &core.RoutingSimulateResult{
		Substituted: true, RuleName: "prefer-anthropic",
		Targets:  []core.RoutingTarget{{ProviderName: "Anthropic", ModelCode: "claude-sonnet-4-6"}},
		Warnings: []string{"no stage-1 rule matched"},
	}
	l := newLab(g, testSession())
	// idle hint
	if !strings.Contains(l.View(120, 40), "press t to resolve") {
		t.Fatalf("route panel idle hint missing:\n%s", l.View(120, 40))
	}
	v, cmd := l.Update(keyRunes("t"))
	l = v.(*labView)
	if !l.routeBusy || cmd == nil {
		t.Fatal("t should start the route dry-run")
	}
	v, _ = l.Update(cmd())
	l = v.(*labView)
	out := l.View(120, 40)
	if !strings.Contains(out, "prefer-anthropic") || !strings.Contains(out, "Anthropic → claude-sonnet-4-6") || !strings.Contains(out, "no stage-1 rule matched") {
		t.Fatalf("route panel should render the resolved route + warning:\n%s", out)
	}
	// no model → t is inert + panel prompts to select
	l2 := newLab(sampleGateway(), kit.Session{EnvName: "local"})
	if _, c := l2.Update(keyRunes("t")); c != nil {
		t.Fatal("t without a model should be inert")
	}
	if !strings.Contains(l2.View(120, 40), "select a model to dry-run") {
		t.Fatal("route panel should prompt to select a model")
	}
	// route error surfaces
	le := newLab(&fakeGateway{err: errors.New("route-down")}, testSession())
	v, cmd = le.Update(keyRunes("t"))
	le = v.(*labView)
	v, _ = le.Update(cmd())
	if !strings.Contains(v.View(120, 40), "route-down") {
		t.Fatal("route error should surface")
	}
}

func TestLab_NotReady(t *testing.T) {
	l := newLab(sampleGateway(), kit.Session{EnvName: "local"})
	// g and r are no-ops when not ready
	_, c1 := l.Update(keyRunes("g"))
	_, c2 := l.Update(keyRunes("r"))
	if c1 != nil || c2 != nil {
		t.Fatal("generator/lab should be inert without a model/VK")
	}
	if !strings.Contains(l.View(100, 20), "Select a model") {
		t.Fatal("not-ready lab should prompt for a model/VK")
	}
}

// loadKill builds a Kill view and runs its initial state fetch.
func loadKill(gw *fakeGateway, s kit.Session) *killView {
	k := newKill(gw, s)
	v, _ := k.Update(k.Init()())
	return v.(*killView)
}

func TestKill_ShowsStateAndSnapshot(t *testing.T) {
	gw := sampleGateway()
	gw.ksState = &core.KillSwitchState{Engaged: true, Known: true, Version: 9, By: "admin"}
	gw.passSnap = &core.PassthroughSnapshot{
		Global:   core.PassthroughTier{Enabled: true, BypassHooks: true},
		Adapters: map[string]core.PassthroughTier{"anthropic": {Enabled: true, BypassCache: true}},
		Providers: map[string]core.PassthroughTier{
			"p1": {Enabled: true, BypassHooks: true}, "p2": {Enabled: false, BypassHooks: true},
		},
	}
	out := loadKill(gw, testSession()).View(120, 14)
	if !strings.Contains(out, "Kill switch") || !strings.Contains(out, "Emergency passthrough") {
		t.Fatalf("kill view should show both panels:\n%s", out)
	}
	if !strings.Contains(out, "ENGAGED") || !strings.Contains(out, "by admin") {
		t.Fatalf("kill switch should show engaged state + actor:\n%s", out)
	}
	if !strings.Contains(out, "bypassing: hooks") {
		t.Fatalf("passthrough should show what the global tier bypasses:\n%s", out)
	}
	// anthropic adapter active (1); p1 active, p2 inactive (1 provider).
	if !strings.Contains(out, "1 adapter(s), 1 provider(s)") {
		t.Fatalf("passthrough should count active overrides:\n%s", out)
	}
}

func TestKill_NonProdToggle(t *testing.T) {
	gw := sampleGateway()
	k := loadKill(gw, testSession()) // non-prod
	if !strings.Contains(k.View(100, 12), "off") {
		t.Fatalf("initial kill switch should render off:\n%s", k.View(100, 12))
	}
	// e raises the confirm gate even in non-prod — no silent write.
	v, cmd := k.Update(keyRunes("e"))
	k = v.(*killView)
	if !k.Capturing() || cmd != nil || k.busy {
		t.Fatal("e must raise the gate, not fire, in non-prod")
	}
	// allowing the gate fires the toggle.
	v, cmd = k.Update(keyRunes("y"))
	k = v.(*killView)
	if !k.busy || cmd == nil {
		t.Fatal("allowing the gate should fire the toggle")
	}
	v, cmd2 := k.Update(cmd()) // killResultMsg → re-fetch state
	k = v.(*killView)
	if k.busy || cmd2 == nil {
		t.Fatal("a completed toggle should clear busy and trigger a state re-read")
	}
	if !strings.Contains(k.View(100, 12), "last toggle") {
		t.Fatalf("kill-switch toggle should record fan-out counts:\n%s", k.View(100, 12))
	}
}

func TestKill_PassthroughToggle(t *testing.T) {
	gw := sampleGateway()
	k := loadKill(gw, testSession())
	// p raises the gate; allowing engages the global emergency passthrough (bypass hooks).
	v, _ := k.Update(keyRunes("p"))
	k = v.(*killView)
	if !k.Capturing() {
		t.Fatal("p should raise the confirm gate")
	}
	v, cmd := k.Update(keyRunes("y"))
	k = v.(*killView)
	if !k.busy || cmd == nil {
		t.Fatal("allowing should fire the passthrough toggle")
	}
	v, _ = k.Update(cmd())
	k = v.(*killView)
	if gw.lastPassthrough == nil || !gw.lastPassthrough.Enabled || !gw.lastPassthrough.BypassHooks {
		t.Fatalf("p should set global passthrough enabled+bypassHooks: %+v", gw.lastPassthrough)
	}
	// o + allow disengages.
	v, _ = k.Update(keyRunes("o"))
	k = v.(*killView)
	v, cmd = k.Update(keyRunes("y"))
	k = v.(*killView)
	k.Update(cmd())
	if gw.lastPassthrough.Enabled {
		t.Fatalf("o should disable global passthrough: %+v", gw.lastPassthrough)
	}
}

func TestKill_ProdGateDenyThenAllow(t *testing.T) {
	k := loadKill(sampleGateway(), kit.Session{EnvName: "prod", IsProd: true})
	v, _ := k.Update(keyRunes("e"))
	k = v.(*killView)
	if !k.Capturing() {
		t.Fatal("prod engage must raise the confirm gate")
	}
	// enter on the default Deny → no toggle.
	v, cmd := k.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	k = v.(*killView)
	if cmd != nil || k.busy || k.Capturing() {
		t.Fatalf("enter on the default Deny must abort without firing: cmd=%v busy=%v", cmd, k.busy)
	}
	// re-open, allow → fires.
	v, _ = k.Update(keyRunes("e"))
	k = v.(*killView)
	v, cmd = k.Update(keyRunes("y"))
	k = v.(*killView)
	if cmd == nil || !k.busy {
		t.Fatal("allowing the gate should fire the toggle")
	}
}

func TestKill_ProdPassthroughConfirmMentionsPassthrough(t *testing.T) {
	k := loadKill(sampleGateway(), kit.Session{EnvName: "prod", IsProd: true})
	v, _ := k.Update(keyRunes("p"))
	k = v.(*killView)
	if !k.Capturing() {
		t.Fatal("prod passthrough engage must raise the confirm gate")
	}
	if !strings.Contains(k.View(100, 12), "global emergency passthrough") {
		t.Fatalf("prod passthrough confirm should name the passthrough target:\n%s", k.View(100, 12))
	}
}

func TestKill_ConfirmCancelAndError(t *testing.T) {
	k := loadKill(sampleGateway(), kit.Session{EnvName: "prod", IsProd: true})
	k.Update(keyRunes("d"))
	// esc cancels the gate
	v, _ := k.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if v.(*killView).Capturing() {
		t.Fatal("esc should cancel the confirmation")
	}
	// error path: gateway toggle fails. (Load succeeds via a clean gateway, then
	// the toggle gateway fails — so the error is the toggle's, not the fetch's.)
	ke := loadKill(sampleGateway(), testSession())
	ke.gw = &fakeGateway{err: errors.New("hub offline")}
	v, _ = ke.Update(keyRunes("e")) // gate up
	ke = v.(*killView)
	v, cmd := ke.Update(keyRunes("y")) // allow → fire cmd
	ke = v.(*killView)
	v, _ = ke.Update(cmd()) // killResultMsg with the toggle error
	if !strings.Contains(v.View(100, 12), "hub offline") {
		t.Fatalf("toggle error should be surfaced:\n%s", v.View(100, 12))
	}
}

func TestKill_NeverToggledAndAllBypass(t *testing.T) {
	gw := sampleGateway()
	gw.ksState = &core.KillSwitchState{Known: false} // never toggled
	gw.passSnap = &core.PassthroughSnapshot{Global: core.PassthroughTier{
		Enabled: true, BypassHooks: true, BypassCache: true, BypassNormalize: true,
	}}
	out := loadKill(gw, testSession()).View(120, 12)
	if !strings.Contains(out, "never toggled") {
		t.Fatalf("an un-toggled kill switch should say so:\n%s", out)
	}
	if !strings.Contains(out, "hooks, cache, normalize") {
		t.Fatalf("global passthrough should list all bypass flags:\n%s", out)
	}
}

func TestKill_IgnoresKeysWhileBusy(t *testing.T) {
	k := loadKill(sampleGateway(), testSession())
	k.busy = true
	_, cmd := k.Update(keyRunes("e"))
	if cmd != nil {
		t.Fatal("a key while a toggle is in flight must be ignored")
	}
}

// snapErrGateway fails only the passthrough-snapshot read, leaving the
// kill-switch read healthy — to prove the two panels surface errors independently.
type snapErrGateway struct{ *fakeGateway }

func (g snapErrGateway) PassthroughSnapshot(context.Context) (*core.PassthroughSnapshot, error) {
	return nil, errors.New("snapshot down")
}

func TestKill_PartialReadError(t *testing.T) {
	k := newKill(snapErrGateway{sampleGateway()}, testSession())
	v, _ := k.Update(k.Init()())
	out := v.View(120, 12)
	if !strings.Contains(out, "off") { // kill-switch read succeeded → state still shows
		t.Fatalf("kill-switch panel should render its state when only the snapshot read failed:\n%s", out)
	}
	if !strings.Contains(out, "snapshot down") {
		t.Fatalf("passthrough panel should surface its own read error:\n%s", out)
	}
}

func TestKill_LeaveClearsConfirm(t *testing.T) {
	k := loadKill(sampleGateway(), kit.Session{EnvName: "prod", IsProd: true})
	v, _ := k.Update(keyRunes("e"))
	k = v.(*killView)
	if !k.Capturing() {
		t.Fatal("prod engage should raise the gate")
	}
	k.Leave()
	if k.Capturing() {
		t.Fatal("leave should clear the in-flight confirmation")
	}
}

func TestKill_LoadError(t *testing.T) {
	k := newKill(&fakeGateway{err: errors.New("hub down")}, testSession())
	v, _ := k.Update(k.Init()())
	if !strings.Contains(v.View(100, 12), "hub down") {
		t.Fatalf("a failed state read should surface in the kill switch panel:\n%s", v.View(100, 12))
	}
}
