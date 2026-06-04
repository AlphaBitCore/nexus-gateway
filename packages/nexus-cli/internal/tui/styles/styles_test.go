package styles

import (
	"image/color"
	"testing"
)

func TestStatusColor(t *testing.T) {
	cases := map[int]color.Color{
		503: Red,
		404: Amber,
		200: Green,
		204: Green,
		100: Amber, // informational → amber default
	}
	for code, want := range cases {
		if got := StatusColor(code); got != want {
			t.Errorf("StatusColor(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestPanelFocusedDiffersFromPanel(t *testing.T) {
	if PanelFocused.GetBorderTopForeground() == Panel.GetBorderTopForeground() {
		t.Fatal("PanelFocused must use a distinct (brand) border color to mark the focused pane")
	}
}

func TestDeltaColor(t *testing.T) {
	if DeltaColor(-0.5) != Red {
		t.Error("negative delta should be red")
	}
	if DeltaColor(0) != Green {
		t.Error("zero delta should be green")
	}
	if DeltaColor(1.2) != Green {
		t.Error("positive delta should be green")
	}
}
