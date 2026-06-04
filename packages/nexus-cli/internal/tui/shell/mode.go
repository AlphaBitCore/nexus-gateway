package shell

import tea "charm.land/bubbletea/v2"

// inputMode is the interaction mode (design §3). NORMAL routes single-key
// hotkeys; INPUT routes the whole keyboard (including IME / multibyte text) to a
// focused field. The zero value is NORMAL.
type inputMode int

const (
	modeNormal inputMode = iota
	modeInput
)

// isHotkey reports whether a key event is a valid NORMAL-mode hotkey. It is the
// IME guard: a NORMAL-mode key is accepted only if it is a single printable-ASCII
// rune or a named special key (enter/esc/tab/arrows/ctrl-*/space). Multibyte runes
// (CJK and other composed input) and multi-rune bursts are rejected — they belong
// to INPUT mode and must never act as (or misfire) a hotkey. This fixes the bug
// where an active CJK IME's composition leaked into the UI as stray commands.
func isHotkey(k tea.KeyPressMsg) bool {
	// A printable key carries its character(s) in Text. Accept exactly one
	// printable-ASCII rune; reject multibyte (CJK/composed) and multi-rune bursts.
	if k.Text != "" {
		r := []rune(k.Text)
		return len(r) == 1 && r[0] < 0x80
	}
	// Named keys (enter, esc, tab, up/down, ctrl+c, space, …) carry a Code and no
	// Text. A fully-empty key (no Code, no Text) is degenerate and not a hotkey.
	return k.Code != 0
}
