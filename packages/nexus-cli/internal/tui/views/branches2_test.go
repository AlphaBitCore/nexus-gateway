package views

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"strings"
	"testing"
)

func TestLab_EditorTyping(t *testing.T) {
	l := newLab(sampleGateway(), testSession())
	l.editor.SetValue("")
	v, _ := l.Update(keyRunes("e")) // edit mode
	l = v.(*labView)
	v, _ = l.Update(keyRunes("{}"))
	l = v.(*labView)
	if !strings.Contains(l.editor.Value(), "{}") {
		t.Fatalf("editor should capture typing, got %q", l.editor.Value())
	}
}

func TestChat_EscJumpsToOverview(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc in chat should emit a jump")
	}
	if jm, ok := cmd().(kit.JumpMsg); !ok || jm.Index != 0 {
		t.Fatalf("esc should jump to Overview (index 0), got %#v", cmd())
	}
}

func TestChat_TranscriptWrapsLongText(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	c.turns = append(c.turns, chatTurn{role: "assistant", text: strings.Repeat("word ", 60)})
	out := c.transcript(40, 100)
	for _, ln := range strings.Split(out, "\n") {
		if lipgloss.Width(ln) > 40 {
			t.Fatalf("transcript line exceeds width 40: %q", ln)
		}
	}
}

func TestChat_TranscriptTrimsToBudget(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	for i := 0; i < 20; i++ {
		c.turns = append(c.turns, chatTurn{role: "user", text: "line"})
	}
	out := c.transcript(80, 4) // budget far smaller than the transcript
	if lines := strings.Count(out, "\n") + 1; lines > 4 {
		t.Fatalf("transcript should trim to the budget, got %d lines", lines)
	}
}
