package shell

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestIsHotkeyAcceptsAsciiAndNamedKeys(t *testing.T) {
	cases := []tea.KeyPressMsg{
		{Code: 'q', Text: "q"},
		{Code: '/', Text: "/"},
		{Code: '>', Text: ">"},
		{Code: tea.KeyEnter},
		{Code: tea.KeyEsc},
		{Code: tea.KeyTab},
		{Code: tea.KeyUp},
		{Code: 'c', Mod: tea.ModCtrl},
		{Code: tea.KeySpace, Text: " "},
	}
	for _, k := range cases {
		if !isHotkey(k) {
			t.Errorf("expected %q to be a NORMAL-mode hotkey", k.String())
		}
	}
}

func TestIsHotkeyRejectsMultibyteAndComposedRunes(t *testing.T) {
	// A CJK rune from an active IME composition — the exact bug this fixes: a
	// composed character must never act as (or misfire) a hotkey.
	if isHotkey(tea.KeyPressMsg{Code: '中', Text: "中"}) {
		t.Error("a CJK rune must not be a NORMAL-mode hotkey (IME guard)")
	}
	// A paste / multi-rune burst (the printable Text carries multiple runes).
	if isHotkey(tea.KeyPressMsg{Text: "ab"}) {
		t.Error("a multi-rune key must not be a NORMAL-mode hotkey")
	}
	// A non-ASCII accented composed rune.
	if isHotkey(tea.KeyPressMsg{Code: 'é', Text: "é"}) {
		t.Error("a non-ASCII composed rune must not be a NORMAL-mode hotkey")
	}
	// A fully-empty key (no Code, no Text) is degenerate and not a hotkey.
	if isHotkey(tea.KeyPressMsg{}) {
		t.Error("an empty key must not be a hotkey")
	}
}

func TestInputModeZeroValueIsNormal(t *testing.T) {
	var m inputMode
	if m != modeNormal {
		t.Fatalf("the zero-value mode must be NORMAL, got %v", m)
	}
	if modeInput == modeNormal {
		t.Fatal("INPUT and NORMAL must be distinct modes")
	}
}
