package shell

import "testing"

func TestBreadcrumbTrail(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{"empty is bare root", nil, "nexus"},
		{"single segment", []string{"Cost"}, "nexus › Cost"},
		{"drill path", []string{"Radar", "ev-9a3f"}, "nexus › Radar › ev-9a3f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := breadcrumbTrail(tc.labels); got != tc.want {
				t.Fatalf("breadcrumbTrail(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

func TestNavStackPushPopPeek(t *testing.T) {
	var n navStack
	if n.depth() != 0 {
		t.Fatalf("fresh stack depth = %d, want 0", n.depth())
	}
	// Pop on an empty stack falls through to the cockpit (index 0), ok=false.
	if idx, ok := n.pop(); idx != 0 || ok {
		t.Fatalf("empty pop = (%d,%v), want (0,false)", idx, ok)
	}

	n.push(2) // drilled from Radar (say)
	n.push(5) // then into a sub-view
	if n.depth() != 2 {
		t.Fatalf("depth after two pushes = %d, want 2", n.depth())
	}
	if idx, ok := n.peek(); idx != 5 || !ok {
		t.Fatalf("peek = (%d,%v), want (5,true)", idx, ok)
	}

	if idx, ok := n.pop(); idx != 5 || !ok {
		t.Fatalf("first pop = (%d,%v), want (5,true)", idx, ok)
	}
	if idx, ok := n.pop(); idx != 2 || !ok {
		t.Fatalf("second pop = (%d,%v), want (2,true)", idx, ok)
	}
	// Past the root: cockpit fall-through.
	if idx, ok := n.pop(); idx != 0 || ok {
		t.Fatalf("root fall-through pop = (%d,%v), want (0,false)", idx, ok)
	}
}

func TestNavStackReset(t *testing.T) {
	var n navStack
	n.push(3)
	n.push(7)
	n.reset()
	if n.depth() != 0 {
		t.Fatalf("depth after reset = %d, want 0", n.depth())
	}
	if _, ok := n.peek(); ok {
		t.Fatal("peek after reset should report empty")
	}
}
