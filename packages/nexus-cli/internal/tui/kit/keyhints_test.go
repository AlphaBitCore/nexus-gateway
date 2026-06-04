package kit

import (
	"strings"
	"testing"
)

func TestGlobalHints(t *testing.T) {
	h := GlobalHints()
	for _, want := range []string{"tab", "quit"} {
		if !strings.Contains(h, want) {
			t.Errorf("GlobalHints missing %q: %s", want, h)
		}
	}
}

func TestHelpReference(t *testing.T) {
	h := HelpReference()
	// The reference is the full cheat sheet: navigation, chat, and slash commands.
	for _, want := range []string{"keys & commands", "Slash commands", "/model", "/resource", "/event"} {
		if !strings.Contains(h, want) {
			t.Errorf("HelpReference missing %q", want)
		}
	}
	// pageHint (OS-aware) is embedded — the scroll-keys line is present either way.
	if !strings.Contains(h, "scroll the transcript") {
		t.Errorf("HelpReference should describe transcript scrolling: %s", h)
	}
}
