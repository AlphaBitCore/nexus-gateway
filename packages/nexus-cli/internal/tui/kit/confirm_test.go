package kit

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func keyRunes(s string) tea.KeyPressMsg {
	r := []rune(s)
	k := tea.KeyPressMsg{Text: s}
	if len(r) == 1 {
		k.Code = r[0]
	}
	return k
}

func TestConfirm_NonProdAlsoGates(t *testing.T) {
	c := NewConfirm(Session{EnvName: "local"})
	ran := false
	c.Begin("do a thing", func() tea.Cmd { ran = true; return nil })
	if !c.Capturing() {
		t.Fatal("non-prod must ALSO raise the gate — every mutation is confirmed in every env")
	}
	if ran {
		t.Fatal("the action must not run before the operator allows it")
	}
	// the off-prod gate renders a neutral (non-PROD) confirmation.
	out := c.View()
	if !strings.Contains(out, "do a thing") {
		t.Fatalf("the gate view should show the prompt: %s", out)
	}
	if strings.Contains(out, "PROD") {
		t.Fatalf("an off-prod gate must not carry the PROD banner: %s", out)
	}
	if c.allow {
		t.Fatal("the choice must default to Deny (safe default) in every env")
	}
	// y allows and runs.
	if handled, _ := c.Update(keyRunes("y")); !handled || !ran || c.Capturing() {
		t.Fatal("y should allow and run the action")
	}
	// a resolved confirm renders nothing and no longer consumes keys.
	if c.View() != "" {
		t.Fatal("a resolved confirm renders nothing")
	}
	if handled, _ := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); handled {
		t.Fatal("an inactive confirm should not handle keys")
	}
}

func TestConfirm_ProdSelectAllowDeny(t *testing.T) {
	c := NewConfirm(Session{EnvName: "prod", IsProd: true})
	ran := false
	c.Begin("disable provider X", func() tea.Cmd { ran = true; return nil })
	if !c.Capturing() || ran {
		t.Fatal("prod should require a choice before running")
	}
	if out := c.View(); !strings.Contains(out, "disable provider X") || !strings.Contains(out, "PROD") {
		t.Fatalf("confirm view should show the prompt + prod marker: %s", out)
	}
	if c.allow {
		t.Fatal("the prod choice must default to Deny (safe default)")
	}
	// enter on the default (Deny) must NOT run the action.
	if handled, _ := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); !handled {
		t.Fatal("enter should be handled while confirming")
	}
	if ran || c.Capturing() {
		t.Fatalf("enter on the default Deny must abort without running: ran=%v", ran)
	}

	// Move the selection to Allow, then enter runs the action.
	c.Begin("disable provider X", func() tea.Cmd { ran = true; return nil })
	c.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // → Allow
	if !c.allow {
		t.Fatal("→ should move the selection to Allow")
	}
	if handled, _ := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); !handled || !ran || c.Capturing() {
		t.Fatal("enter on Allow should run the action")
	}
}

func TestConfirm_ProdQuickKeys(t *testing.T) {
	// `y` is a quick Allow; `n` / esc deny.
	c := NewConfirm(Session{EnvName: "prod", IsProd: true})
	ran := false
	c.Begin("flush", func() tea.Cmd { ran = true; return nil })
	if handled, _ := c.Update(keyRunes("y")); !handled || !ran || c.Capturing() {
		t.Fatal("y should allow immediately")
	}
	c.Begin("flush", func() tea.Cmd { ran = true; return nil })
	ran = false
	if handled, _ := c.Update(keyRunes("n")); !handled || ran || c.Capturing() {
		t.Fatal("n should deny without running")
	}
	if !strings.Contains(c.HelpHint(), "allow") {
		t.Fatalf("help hint should describe the choice: %s", c.HelpHint())
	}
}

func TestConfirm_Cancel(t *testing.T) {
	c := NewConfirm(Session{EnvName: "prod", IsProd: true})
	ran := false
	c.Begin("x", func() tea.Cmd { ran = true; return nil })
	c.Cancel()
	if c.Capturing() {
		t.Fatal("Cancel must drop the gate")
	}
	// A stale resolve after cancel is a no-op (run was cleared, gate inactive).
	if handled, _ := c.Update(keyRunes("y")); handled || ran {
		t.Fatal("a cancelled gate must run nothing and handle no keys")
	}
}

func TestConfirm_RendersAllowSelectedAndSwallows(t *testing.T) {
	// prod, Allow selected → renders the env-named Apply button.
	c := NewConfirm(Session{EnvName: "prod", IsProd: true})
	c.Begin("act", func() tea.Cmd { return nil })
	c.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // → Allow
	if !strings.Contains(c.View(), "Apply") {
		t.Fatalf("prod Allow-selected view must show the Apply button: %s", c.View())
	}
	// A stray non-choice key is swallowed (handled, no command) while the gate is up.
	if handled, cmd := c.Update(keyRunes("z")); !handled || cmd != nil {
		t.Fatal("a stray key must be swallowed, never run the action")
	}
	// off-prod (neutral), Allow selected.
	n := NewConfirm(Session{EnvName: "local"})
	n.Begin("act", func() tea.Cmd { return nil })
	n.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if !strings.Contains(n.View(), "Apply") {
		t.Fatalf("neutral Allow-selected view must show Apply: %s", n.View())
	}
}

func TestConfirm_CancelEsc(t *testing.T) {
	c := NewConfirm(Session{EnvName: "prod", IsProd: true})
	ran := false
	c.Begin("x", func() tea.Cmd { ran = true; return nil })
	if handled, _ := c.Update(tea.KeyPressMsg{Code: tea.KeyEsc}); !handled || c.Capturing() || ran {
		t.Fatal("esc should cancel without running")
	}
	// an arrow toggle is consumed and keeps the gate up.
	c.Begin("x", func() tea.Cmd { return nil })
	if handled, _ := c.Update(tea.KeyPressMsg{Code: tea.KeyLeft}); !handled || !c.Capturing() {
		t.Fatal("an arrow should be handled and keep the gate active")
	}
}
