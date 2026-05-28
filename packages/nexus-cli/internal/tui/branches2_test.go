package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestRun_WiresShellWithAltScreen(t *testing.T) {
	orig := run
	defer func() { run = orig }()
	var sawShell, sawAltOpt bool
	run = func(m tea.Model, opts ...tea.ProgramOption) error {
		_, sawShell = m.(*Shell)
		sawAltOpt = len(opts) == 1
		return nil
	}
	deps := Deps{Gateway: sampleGateway(), HasSession: func() bool { return true },
		Session: Session{EnvName: "local", Model: "m", VKSecret: "s"}}
	if err := Run(deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawShell || !sawAltOpt {
		t.Fatalf("Run should launch a Shell with the alt-screen option (shell=%v alt=%v)", sawShell, sawAltOpt)
	}
}

func TestWizard_VKNavAndSecretTyping(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	r.hasSession = true
	w := newWizard(r.deps)
	w, _ = runStep(t, w, w.Init()) // → stageModel with one model
	// pick the model, load VKs
	w, cmd := w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	w, _ = runStep(t, w, cmd)
	// give it two VKs to exercise nav clamping both ways
	w.vks = append(w.vks, w.vks[0])
	w.vks[1].Name = "second"
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyDown})
	if w.vkCursor != 1 {
		t.Fatalf("down should move VK cursor to 1, got %d", w.vkCursor)
	}
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyUp})
	if w.vkCursor != 0 {
		t.Fatalf("up should move VK cursor to 0, got %d", w.vkCursor)
	}
	// select VK → secret stage, then type into the (masked) secret field
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if w.stage != stageSecret {
		t.Fatalf("should be at secret stage, got %d", w.stage)
	}
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("nvk_typed")})
	if w.secret.Value() != "nvk_typed" {
		t.Fatalf("typed secret not captured: %q", w.secret.Value())
	}
}

func TestKill_ConfirmFieldTyping(t *testing.T) {
	k := newKill(sampleGateway(), Session{EnvName: "prod", IsProd: true})
	k.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}) // → confirming
	// typing flows into the confirm field
	v, _ := k.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("prod")})
	k = v.(*killView)
	if k.input.Value() != "prod" {
		t.Fatalf("confirm field should capture typing, got %q", k.input.Value())
	}
}

func TestLab_EditorTyping(t *testing.T) {
	l := newLab(sampleGateway(), testSession())
	l.editor.SetValue("")
	v, _ := l.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}) // edit mode
	l = v.(*labView)
	v, _ = l.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("{}")})
	l = v.(*labView)
	if !strings.Contains(l.editor.Value(), "{}") {
		t.Fatalf("editor should capture typing, got %q", l.editor.Value())
	}
}

func TestChat_TranscriptTrimsToBudget(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	for i := 0; i < 20; i++ {
		c.turns = append(c.turns, chatTurn{role: "user", text: "line"})
	}
	out := c.transcript(4) // budget far smaller than the transcript
	if lines := strings.Count(out, "\n") + 1; lines > 4 {
		t.Fatalf("transcript should trim to the budget, got %d lines", lines)
	}
}

func TestModel_EventIndexFallback(t *testing.T) {
	// A model whose tabs lack "Event" falls back to index 0 (defensive branch).
	m := Model{tabs: []string{"A", "B"}}
	if m.eventIndex() != 0 {
		t.Fatal("eventIndex should fall back to 0 when Event is absent")
	}
}
