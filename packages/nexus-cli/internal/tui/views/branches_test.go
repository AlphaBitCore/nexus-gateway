package views

import (
	"errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"strings"
	"testing"
)

// chatTabIndex / labTabIndex / killTabIndex must track NewModel's tab order.
const (
	chatTabIndex = 5
	labTabIndex  = 6
	killTabIndex = 7
)

func TestViews_InitAndHelp(t *testing.T) {
	gw, s := sampleGateway(), testSession()
	if newChat(gw, s).Init() == nil {
		t.Fatal("chat Init returns the blink cmd")
	}
	if newLab(gw, s).Init() != nil {
		t.Fatal("lab Init has no startup command")
	}
	if newKill(gw, s).Init() == nil {
		t.Fatal("kill Init should fetch the current kill-switch + passthrough state")
	}
	if !strings.Contains(newChat(gw, s).Help(), "enter send") {
		t.Fatal("ready chat help")
	}
	if !strings.Contains(newChat(gw, kit.Session{}).Help(), "no model/VK") {
		t.Fatal("not-ready chat help")
	}
	if !strings.Contains(newLab(gw, s).Help(), "generator burst") {
		t.Fatal("lab idle help")
	}
	le := newLab(gw, s)
	le.editing = true
	if !strings.Contains(le.Help(), "send lab request") {
		t.Fatal("lab edit help")
	}
}

func TestSLOCost_LastGoodOnError(t *testing.T) {
	// SLO: a later error keeps the prior data with a stale note.
	s := newSLO(sampleGateway())
	v, _ := s.Update(s.Init()())
	sv := v.(*slo)
	sv.err = errors.New("blip")
	if !strings.Contains(sv.View(120, 30), "last-good") {
		t.Fatal("SLO should show last-good banner on transient error")
	}
	// SLO availability with no metrics.
	empty := newSLO(&fakeGateway{phases: sampleGateway().phases})
	v, _ = empty.Update(empty.Init()())
	if !strings.Contains(v.View(120, 30), "no metrics") {
		t.Fatal("SLO availability with no sparkline → 'no metrics'")
	}
	// Cost: last-good on error.
	c := newCost(sampleGateway())
	cv, _ := c.Update(c.Init()())
	cvv := cv.(*cost)
	cvv.err = errors.New("blip")
	if !strings.Contains(cvv.View(120, 30), "last-good") {
		t.Fatal("cost should show last-good banner on transient error")
	}
}

func TestSLOCost_EmptyPanels(t *testing.T) {
	s := newSLO(&fakeGateway{sp: sampleGateway().sp, phases: nil, fallbacks: nil})
	v, _ := s.Update(s.Init()())
	out := v.View(120, 30)
	if !strings.Contains(out, "(no data)") || !strings.Contains(out, "(none)") {
		t.Fatalf("SLO empty panels should render placeholders:\n%s", out)
	}
	c := newCost(&fakeGateway{roi: nil, cost: nil})
	cv, _ := c.Update(c.Init()())
	if !strings.Contains(cv.View(120, 30), "no spend") {
		t.Fatal("cost empty report → '(no spend)'")
	}
}

func TestLab_GenStatus(t *testing.T) {
	// genStatus running / failed badge.
	l := newLab(sampleGateway(), testSession())
	l.genTotal, l.genOK, l.genFail, l.genRunning = 10, 3, 0, true
	if !strings.Contains(l.genStatus(), "running") {
		t.Fatal("running generator badge")
	}
	l.genRunning, l.genFail = false, 2
	if !strings.Contains(l.genStatus(), "5/10") || !strings.Contains(l.genStatus(), "failed 2") {
		t.Fatalf("finished generator counts wrong: %s", l.genStatus())
	}
}

func TestKill_ConfirmViewProd(t *testing.T) {
	k := newKill(sampleGateway(), kit.Session{EnvName: "prod", IsProd: true})
	k.Update(keyRunes("e")) // → raises the shared confirm gate (prod framing)
	if !k.Capturing() {
		t.Fatal("prod kill toggle should raise the confirm gate, not fire")
	}
	out := k.View(100, 12)
	if !strings.Contains(out, "PRODUCTION · prod") || !strings.Contains(out, "engage the kill switch") {
		t.Fatalf("prod confirm view should carry the prod banner + the action:\n%s", out)
	}
	if !strings.Contains(out, "Apply to prod") {
		t.Fatalf("prod confirm view should offer the env-named Apply button:\n%s", out)
	}
}
