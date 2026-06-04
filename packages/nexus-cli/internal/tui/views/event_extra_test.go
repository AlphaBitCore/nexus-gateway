package views

import (
	"errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
	"strings"
	"testing"
)

// errChat is a canned stream failure for the explain-error test.
var errChat = errors.New("vk invalid")

// blockedEventGateway returns an event with hook decisions + bodies.
func blockedEventGateway() *fakeGateway {
	g := sampleGateway()
	g.ev = &core.TrafficEvent{
		ID: "evx", StatusCode: 403, ModelName: "gpt-4o-mini", ProviderName: "openai",
		RequestHookDecision: "block", RequestHookReason: "pii detected",
		ResponseHookDecision: "allow",
		RequestBody:          []byte(`{"model":"gpt-4o-mini"}`),
		ResponseBody:         []byte(`{"error":"blocked"}`),
	}
	return g
}

func TestEvent_HookDecisionsAndBodies(t *testing.T) {
	e := newEvent(blockedEventGateway(), testSession())
	e.SetID("evx")
	v, _ := e.Update(e.Init()())
	e = v.(*EventView)
	out := e.View(120, 40)
	if !strings.Contains(out, "Hooks") || !strings.Contains(out, "BLOCK") || !strings.Contains(out, "pii detected") {
		t.Fatalf("event should show hook decision + reason:\n%s", out)
	}
	// b cycles none → request → response
	if e.bodyPanel() != "" {
		t.Fatal("bodyMode 0 shows no body")
	}
	v, _ = e.Update(keyRunes("b"))
	e = v.(*EventView)
	if !strings.Contains(e.View(120, 40), "Request body") {
		t.Fatal("first b shows request body")
	}
	v, _ = e.Update(keyRunes("b"))
	e = v.(*EventView)
	if !strings.Contains(e.View(120, 40), "Response body") {
		t.Fatal("second b shows response body")
	}
	v, _ = e.Update(keyRunes("b"))
	e = v.(*EventView)
	if e.bodyPanel() != "" {
		t.Fatal("third b cycles back to no body")
	}
}

func TestEvent_ExplainStreams(t *testing.T) {
	g := blockedEventGateway()
	g.chatText = "Blocked because the request contained PII."
	e := newEvent(g, testSession())
	e.SetID("evx")
	v, _ := e.Update(e.Init()())
	e = v.(*EventView)
	// x starts the explanation stream
	v, cmd := e.Update(keyRunes("x"))
	e = v.(*EventView)
	if !e.explaining || cmd == nil {
		t.Fatal("x should start explaining")
	}
	// drain delta then done
	msg := cmd()
	if dm, ok := msg.(explainDeltaMsg); !ok || !strings.Contains(dm.text, "PII") {
		t.Fatalf("first explain msg should be the delta, got %#v", msg)
	}
	v, cmd2 := e.Update(msg)
	e = v.(*EventView)
	v, _ = e.Update(cmd2()) // explainDoneMsg
	e = v.(*EventView)
	if e.explaining {
		t.Fatal("explain should finish")
	}
	if !strings.Contains(e.View(120, 40), "Blocked because") {
		t.Fatalf("explanation should render:\n%s", e.View(120, 40))
	}
}

func TestEvent_AutoExplainTriggersAfterLoad(t *testing.T) {
	g := blockedEventGateway()
	g.chatText = "Blocked because the request contained PII."
	e := newEvent(g, testSession())
	e.SetIDExplain("evx")
	if !e.autoExplain {
		t.Fatal("SetIDExplain should arm autoExplain")
	}
	// Deliver the loaded event; the armed explain should fire.
	v, cmd := e.Update(e.Init()())
	e = v.(*EventView)
	if e.autoExplain {
		t.Fatal("autoExplain should be cleared after firing")
	}
	if !e.explaining || cmd == nil {
		t.Fatal("auto-explain should start the explanation stream once the event loaded")
	}
	// A plain (non-explain) open must NOT auto-explain.
	plain := newEvent(blockedEventGateway(), testSession())
	plain.SetID("evx")
	pv, _ := plain.Update(plain.Init()())
	if pv.(*EventView).explaining {
		t.Fatal("a plain Radar open must not auto-explain")
	}
}

func TestEvent_ExplainRequiresVK(t *testing.T) {
	e := newEvent(blockedEventGateway(), kit.Session{EnvName: "local"}) // no model/VK
	e.SetID("evx")
	e.Update(e.Init()())
	_, cmd := e.Update(keyRunes("x"))
	if cmd != nil || e.explainErr == nil || !strings.Contains(e.explainErr.Error(), "Virtual Key") {
		t.Fatalf("explain without a VK should error, got %v", e.explainErr)
	}
}

func TestEvent_ExplainStreamError(t *testing.T) {
	g := blockedEventGateway()
	g.ev = &core.TrafficEvent{ID: "evx", StatusCode: 500}
	e := newEvent(g, testSession())
	e.SetID("evx")
	e.Update(e.Init()())
	// make ChatStream fail by clearing chatText + setting err only on stream:
	g.err = errChat
	v, cmd := e.Update(keyRunes("x"))
	e = v.(*EventView)
	// drain: deltaCh closes immediately (err) → done with error
	v, _ = e.Update(cmd())
	e = v.(*EventView)
	if !strings.Contains(e.View(120, 40), errChat.Error()) {
		t.Fatalf("explain stream error should surface:\n%s", e.View(120, 40))
	}
}

func TestEvent_Replay(t *testing.T) {
	g := blockedEventGateway()
	g.ev.Path = "/v1/chat/completions"
	g.simResp = []byte(`{"choices":[{"message":{"content":"replayed"}}]}`)
	e := newEvent(g, testSession())
	e.SetID("evx")
	v, _ := e.Update(e.Init()())
	e = v.(*EventView)
	// r re-fires the captured request via the simulator
	v, cmd := e.Update(keyRunes("r"))
	e = v.(*EventView)
	if !e.replayBusy || cmd == nil {
		t.Fatal("r should start a replay")
	}
	v, _ = e.Update(cmd())
	e = v.(*EventView)
	if !strings.Contains(e.View(120, 40), "replayed") {
		t.Fatalf("replay result should render:\n%s", e.View(120, 40))
	}
	// no VK → replay blocked with a prompt
	e2 := newEvent(g, kit.Session{EnvName: "local"})
	e2.SetID("evx")
	e2.Update(e2.Init()())
	if _, c := e2.Update(keyRunes("r")); c != nil || e2.replayErr == nil {
		t.Fatal("replay without a VK should be blocked")
	}
	// no request body → blocked
	g3 := blockedEventGateway()
	g3.ev = &core.TrafficEvent{ID: "evx", Path: "/v1/chat/completions"} // no RequestBody
	e3 := newEvent(g3, testSession())
	e3.SetID("evx")
	e3.Update(e3.Init()())
	if _, c := e3.Update(keyRunes("r")); c != nil || e3.replayErr == nil {
		t.Fatal("replay without a captured body should be blocked")
	}
}

func TestEvent_HelpAndHookColor(t *testing.T) {
	e := newEvent(sampleGateway(), testSession())
	if !strings.Contains(e.Help(), "inspect") {
		t.Fatal("no-event help mentions inspect")
	}
	e.SetID("ev1")
	e.Update(e.Init()())
	if !strings.Contains(e.Help(), "explain") {
		t.Fatal("loaded help mentions explain")
	}
	if hookDecisionColor("block") != styles.Red || hookDecisionColor("redact") != styles.Amber || hookDecisionColor("allow") != styles.Green {
		t.Fatal("hook decision RAG colors wrong")
	}
}
