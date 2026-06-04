package shell

import (
	"strings"
	"testing"
)

func TestVisibleLen(t *testing.T) {
	if got := visibleLen("abc"); got != 3 {
		t.Fatalf("plain len = %d", got)
	}
	// ESC[31m red ESC[0m → "red" is 3 visible runes, escapes excluded.
	if got := visibleLen("\x1b[31mred\x1b[0m"); got != 3 {
		t.Fatalf("styled visible len = %d, want 3", got)
	}
}

func TestRevealANSI(t *testing.T) {
	// Raw text: first n runes, no reset appended.
	if got := revealANSI("hello", 3); got != "hel" {
		t.Fatalf("raw reveal = %q", got)
	}
	if got := revealANSI("hello", 99); got != "hello" {
		t.Fatalf("over-reveal raw = %q", got)
	}
	// Styled text: escapes copied verbatim (never sliced), reset appended, only the
	// requested visible runes shown.
	styled := "\x1b[31mred\x1b[0m"
	got := revealANSI(styled, 2)
	if !strings.HasPrefix(got, "\x1b[31m") {
		t.Fatalf("the opening escape must be preserved: %q", got)
	}
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("a reset must be appended so a cut style does not bleed: %q", got)
	}
	if visibleLen(got) != 2 {
		t.Fatalf("revealed visible runes = %d, want 2", visibleLen(got))
	}
}

// TestConversationTypewriterRevealsCommands guards that command output (/help) is
// typed out: the line starts empty, fills as the reveal clock ticks, and the
// reveal tag tracks it.
func TestConversationTypewriterRevealsCommands(t *testing.T) {
	c := newConversation(testSessionLocal(), nil)
	c.appendLine("help", "abcdef")

	if c.revealTag != "help" {
		t.Fatalf("revealTag should track the typed line, got %q", c.revealTag)
	}
	if last := c.lines[len(c.lines)-1]; last.text != "" {
		t.Fatalf("a typed line starts empty, got %q", last.text)
	}
	// One tick reveals revealRunes (3) characters.
	c.revealStep()
	if last := c.lines[len(c.lines)-1]; last.text != "abc" {
		t.Fatalf("after one tick: %q, want abc", last.text)
	}
	// flushReveal snaps it to the full text.
	c.flushReveal()
	if last := c.lines[len(c.lines)-1]; last.text != "abcdef" {
		t.Fatalf("flush should show all: %q", last.text)
	}
	if c.revealing() {
		t.Fatal("nothing should remain hidden after flush")
	}
}
