package shell

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestSlashCmdMatches(t *testing.T) {
	c := slashCmd{name: "cost", desc: "spend, burn-rate, top talkers", aliases: []string{"spend", "$"}}
	cases := []struct {
		q    string
		want bool
	}{
		{"", true},         // empty matches everything
		{"cost", true},     // name
		{"co", true},       // name prefix
		{"/cost", true},    // leading slash stripped
		{"spend", true},    // alias
		{"burn", true},     // description substring
		{"latency", false}, // unrelated
	}
	for _, tc := range cases {
		if got := c.matches(tc.q); got != tc.want {
			t.Errorf("matches(%q) = %v, want %v", tc.q, got, tc.want)
		}
	}
}

func TestMatchSlashPreservesRegistryOrder(t *testing.T) {
	cmds := []slashCmd{
		{name: "alpha"}, {name: "beta", aliases: []string{"alpha-ish"}}, {name: "gamma"},
	}
	all := matchSlash(cmds, "")
	if len(all) != 3 || all[0].name != "alpha" || all[2].name != "gamma" {
		t.Fatalf("empty query should return all in order, got %+v", all)
	}
	got := matchSlash(cmds, "alpha")
	if len(got) != 2 || got[0].name != "alpha" || got[1].name != "beta" {
		t.Fatalf("query alpha should match alpha+beta in order, got %+v", got)
	}
}

func TestDefaultSlashCommandsCoverageAndKinds(t *testing.T) {
	cmds := defaultSlashCommands()
	byName := map[string]slashCmd{}
	for _, c := range cmds {
		byName[c.name] = c
	}
	// Every resource view the operator can open is a /command.
	for _, v := range []string{"overview", "radar", "cost", "slo", "nodes", "alerts",
		"compliance", "jobs", "sync", "models", "keys", "rules", "kill", "lab", "event"} {
		c, ok := byName[v]
		if !ok {
			t.Errorf("missing view command /%s", v)
			continue
		}
		if c.kind != slashView {
			t.Errorf("/%s should be a view command", v)
		}
	}
	// The conversation wires exactly these agent controls; the palette advertises
	// only what it can handle.
	for _, a := range []string{"clear", "help"} {
		c, ok := byName[a]
		if !ok {
			t.Errorf("missing agent command /%s", a)
			continue
		}
		if c.kind != slashAgent {
			t.Errorf("/%s should be an agent command", a)
		}
	}
	// Commands the conversation does not yet wire must NOT be advertised — the
	// palette must never offer a command that replies "unknown command".
	for _, gone := range []string{"raw", "sessions", "resume", "skill"} {
		if _, ok := byName[gone]; ok {
			t.Errorf("/%s is advertised but not wired in the conversation — drop it from the palette", gone)
		}
	}
}

// typeSlash folds each rune of s into the palette via Update (as the runtime does).
func typeSlash(p slashPalette, s string) slashPalette {
	for _, r := range s {
		p, _ = p.Update(keyRunes(string(r)))
	}
	return p
}

func TestSlashPaletteFilterAndSelect(t *testing.T) {
	p := newSlashPalette(defaultSlashCommands())
	p = typeSlash(p, "cost")
	_, cmd := p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	sel, ok := cmd().(slashSelectedMsg)
	if !ok {
		t.Fatalf("enter should emit slashSelectedMsg, got %T", cmd())
	}
	if sel.cmd.name != "cost" || sel.arg != "" {
		t.Fatalf("want cost/no-arg, got %q/%q", sel.cmd.name, sel.arg)
	}
}

func TestSlashPaletteSelectWithArg(t *testing.T) {
	p := newSlashPalette(defaultSlashCommands())
	p = typeSlash(p, "event ev-9a3f")
	_, cmd := p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	sel, ok := cmd().(slashSelectedMsg)
	if !ok {
		t.Fatalf("enter should emit slashSelectedMsg, got %T", cmd())
	}
	if sel.cmd.name != "event" || sel.arg != "ev-9a3f" {
		t.Fatalf("want event/ev-9a3f, got %q/%q", sel.cmd.name, sel.arg)
	}
}

func TestSlashPaletteEscCloses(t *testing.T) {
	p := newSlashPalette(defaultSlashCommands())
	_, cmd := p.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if _, ok := cmd().(slashCloseMsg); !ok {
		t.Fatalf("esc should emit slashCloseMsg, got %T", cmd())
	}
}

func TestSlashPaletteNoMatchEnterIsNoop(t *testing.T) {
	p := newSlashPalette(defaultSlashCommands())
	p = typeSlash(p, "zzzznotacommand")
	if _, cmd := p.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("enter with no match should be a no-op, got cmd %T", cmd())
	}
}

func TestSlashPaletteArrowsClampWithinMatches(t *testing.T) {
	p := newSlashPalette(defaultSlashCommands())
	p = typeSlash(p, "c") // cost, compliance, clear, … (>1 match)
	if len(p.matches) < 2 {
		t.Fatalf("expected multiple matches for 'c', got %d", len(p.matches))
	}
	// Up at the top stays put.
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if p.cursor != 0 {
		t.Fatalf("cursor should clamp at 0, got %d", p.cursor)
	}
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursor != 1 {
		t.Fatalf("down should move to 1, got %d", p.cursor)
	}
	// Up from 1 decrements back to 0.
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if p.cursor != 0 {
		t.Fatalf("up should move back to 0, got %d", p.cursor)
	}
	// Down past the last match clamps at the bottom.
	last := len(p.matches) - 1
	for i := 0; i < len(p.matches)+2; i++ {
		p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if p.cursor != last {
		t.Fatalf("down should clamp at the last match %d, got %d", last, p.cursor)
	}
}

func TestSlashPaletteView(t *testing.T) {
	p := newSlashPalette(defaultSlashCommands())
	out := p.View()
	if !strings.Contains(out, "Commands") || !strings.Contains(out, "/cost") {
		t.Fatalf("view should list commands with descriptions, got:\n%s", out)
	}
	p = typeSlash(p, "zzzznotacommand")
	if !strings.Contains(p.View(), "no matching command") {
		t.Fatalf("an empty match set should show the no-match hint, got:\n%s", p.View())
	}
}
