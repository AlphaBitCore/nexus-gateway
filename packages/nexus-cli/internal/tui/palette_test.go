package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestRegistry_FuzzyMatch(t *testing.T) {
	entries := []viewEntry{
		{name: "Overview", aliases: []string{"ov", "health"}},
		{name: "Radar", aliases: []string{"traffic"}},
		{name: "Alerts", aliases: []string{"al", "firing"}},
	}
	// empty query matches everything
	if got := matchEntries(entries, ""); len(got) != 3 {
		t.Fatalf("empty query should match all, got %v", got)
	}
	// alias match: "traffic" → Radar (index 1)
	if got := matchEntries(entries, "traffic"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("'traffic' should match Radar, got %v", got)
	}
	// name substring, case-insensitive
	if got := matchEntries(entries, "ALER"); len(got) != 1 || got[0] != 2 {
		t.Fatalf("'ALER' should match Alerts, got %v", got)
	}
	// no match
	if got := matchEntries(entries, "zzz"); len(got) != 0 {
		t.Fatalf("'zzz' should match nothing, got %v", got)
	}
}

func TestPalette_FilterNavSelect(t *testing.T) {
	entries := []viewEntry{
		{name: "Overview"}, {name: "Radar", aliases: []string{"traffic"}}, {name: "Cost"},
	}
	p := newPalette(entries)
	if len(p.matches) != 3 {
		t.Fatalf("fresh palette matches all: %v", p.matches)
	}
	// type "traffic" → only Radar (index 1)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("traffic")})
	if len(p.matches) != 1 || p.matches[0] != 1 {
		t.Fatalf("after typing 'traffic', matches=%v", p.matches)
	}
	if !strings.Contains(p.View(), "Radar") {
		t.Fatalf("palette view should list Radar:\n%s", p.View())
	}
	// enter → jumpMsg{1}
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || cmd() != (jumpMsg{index: 1}) {
		t.Fatal("enter should emit jumpMsg for the highlighted match")
	}
	// esc → paletteCloseMsg
	_, cmd = p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil || cmd() != (paletteCloseMsg{}) {
		t.Fatal("esc should emit paletteCloseMsg")
	}
}

func TestPalette_NavClampsAndEmpty(t *testing.T) {
	p := newPalette([]viewEntry{{name: "A"}, {name: "B"}})
	// up at top clamps
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyUp})
	if p.cursor != 0 {
		t.Fatal("up at top should clamp to 0")
	}
	// down moves, then clamps at end
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyDown})
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyDown})
	if p.cursor != 1 {
		t.Fatalf("down should clamp at last match, cursor=%d", p.cursor)
	}
	// filter to nothing → enter is a no-op, view shows placeholder
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("zzz")})
	if !strings.Contains(p.View(), "no matching view") {
		t.Fatalf("empty matches should show placeholder:\n%s", p.View())
	}
	if _, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("enter with no matches should be a no-op")
	}
}

func TestModel_PaletteOpenJumpClose(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = m2.(Model)
	// ":" opens the palette
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")})
	m = m2.(Model)
	if !m.paletteOpen {
		t.Fatal("':' should open the palette")
	}
	if !strings.Contains(m.View(), "Command palette") {
		t.Fatalf("open palette should render in the footer:\n%s", m.View())
	}
	// type to filter, then a jumpMsg switches the view + closes
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("cost")})
	m = m2.(Model)
	m2, _ = m.Update(jumpMsg{index: 4}) // Cost
	m = m2.(Model)
	if m.paletteOpen || m.active != 4 {
		t.Fatalf("jumpMsg should switch to Cost and close palette (open=%v active=%d)", m.paletteOpen, m.active)
	}
	// reopen + esc closes without switching
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")})
	m = m2.(Model)
	m2, _ = m.Update(paletteCloseMsg{})
	m = m2.(Model)
	if m.paletteOpen || m.active != 4 {
		t.Fatal("paletteCloseMsg should close without changing the active view")
	}
}

func TestModel_PaletteSuspendsGlobalKeys(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")})
	m = m2.(Model)
	// 'q' while palette open must NOT quit — it types into the query
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m = m2.(Model)
	if m.quitting {
		t.Fatal("'q' while palette open should not quit")
	}
	// ctrl+c still quits
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should still quit with palette open")
	}
}
