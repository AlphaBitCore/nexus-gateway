package styles

import "testing"

func TestStatusColor(t *testing.T) {
	cases := map[int]string{
		503: string(Red),
		404: string(Amber),
		200: string(Green),
		204: string(Green),
		100: string(Amber), // informational → amber default
	}
	for code, want := range cases {
		if got := string(StatusColor(code)); got != want {
			t.Errorf("StatusColor(%d) = %s, want %s", code, got, want)
		}
	}
}

func TestDeltaColor(t *testing.T) {
	if string(DeltaColor(-0.5)) != string(Red) {
		t.Error("negative delta should be red")
	}
	if string(DeltaColor(0)) != string(Green) {
		t.Error("zero delta should be green")
	}
	if string(DeltaColor(1.2)) != string(Green) {
		t.Error("positive delta should be green")
	}
}
