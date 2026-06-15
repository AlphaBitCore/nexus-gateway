package cli

import (
	"strings"
	"testing"
)

// TestClipCell covers the resource-table cell clipper: an empty cell renders the
// em-dash placeholder, a short cell passes through untouched, and an over-long
// cell is rune-safely truncated with an ellipsis (never mid-multibyte-rune).
func TestClipCell(t *testing.T) {
	if got := clipCell(""); got != "—" {
		t.Fatalf("empty cell should render the em-dash placeholder, got %q", got)
	}
	if got := clipCell("short"); got != "short" {
		t.Fatalf("a cell within the width should pass through, got %q", got)
	}
	// A long multibyte string must clip to an ellipsis without splitting a rune.
	long := strings.Repeat("世", summaryCellWidth+10)
	got := clipCell(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("an over-long cell should end with an ellipsis, got %q", got)
	}
	if r := []rune(got); len(r) != summaryCellWidth {
		t.Fatalf("a clipped cell should be exactly summaryCellWidth runes, got %d", len(r))
	}
	if strings.ContainsRune(got, '�') {
		t.Fatal("clipping must be rune-safe (no replacement char)")
	}
}
