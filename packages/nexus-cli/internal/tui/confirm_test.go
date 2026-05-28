package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestConfirm_NonProdRunsImmediately(t *testing.T) {
	c := newConfirm(Session{EnvName: "local"})
	ran := false
	c.begin("do a thing", func() tea.Cmd { ran = true; return nil })
	if c.capturing() {
		t.Fatal("non-prod should not require a typed confirmation")
	}
	if !ran {
		t.Fatal("non-prod should run the action immediately")
	}
	if c.view() != "" {
		t.Fatal("an inactive confirm renders nothing")
	}
	// an inactive confirm does not consume keys.
	if handled, _ := c.update(tea.KeyMsg{Type: tea.KeyEnter}); handled {
		t.Fatal("inactive confirm should not handle keys")
	}
}

func TestConfirm_ProdTypedConfirm(t *testing.T) {
	c := newConfirm(Session{EnvName: "prod", IsProd: true})
	ran := false
	c.begin("disable provider X", func() tea.Cmd { ran = true; return nil })
	if !c.capturing() || ran {
		t.Fatal("prod should require confirmation before running")
	}
	if out := c.view(); !strings.Contains(out, "disable provider X") || !strings.Contains(out, "prod") {
		t.Fatalf("confirm view should show the prompt + env: %s", out)
	}
	if !strings.Contains(c.helpHint(), "prod") {
		t.Fatalf("help hint should mention the env: %s", c.helpHint())
	}

	// a wrong name aborts: not run, error set, no longer active.
	c.input.SetValue("nope")
	if handled, _ := c.update(tea.KeyMsg{Type: tea.KeyEnter}); !handled {
		t.Fatal("enter should be handled while confirming")
	}
	if ran || c.err == nil || c.capturing() {
		t.Fatalf("a mismatched confirmation must abort: ran=%v err=%v", ran, c.err)
	}

	// the matching name runs the action.
	c.begin("disable provider X", func() tea.Cmd { ran = true; return nil })
	c.input.SetValue("prod")
	if handled, _ := c.update(tea.KeyMsg{Type: tea.KeyEnter}); !handled || !ran || c.capturing() {
		t.Fatal("a matching confirmation should run the action")
	}
}

func TestConfirm_CancelAndType(t *testing.T) {
	c := newConfirm(Session{EnvName: "prod", IsProd: true})
	ran := false
	c.begin("x", func() tea.Cmd { ran = true; return nil })
	if handled, _ := c.update(tea.KeyMsg{Type: tea.KeyEsc}); !handled || c.capturing() || ran {
		t.Fatal("esc should cancel without running")
	}
	// typing (non enter/esc) is consumed while active.
	c.begin("x", func() tea.Cmd { return nil })
	if handled, _ := c.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pr")}); !handled {
		t.Fatal("typing should be handled while confirming")
	}
}
