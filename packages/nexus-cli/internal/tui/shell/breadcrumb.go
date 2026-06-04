package shell

import "strings"

// breadcrumbTrail renders the navigation path as "nexus › traffic › ev-9a3f".
// The root segment ("nexus") is always present; the supplied labels are the
// path from the cockpit down to the active view (the last label is where the
// operator is now). An empty slice renders the bare root.
func breadcrumbTrail(labels []string) string {
	if len(labels) == 0 {
		return "nexus"
	}
	return "nexus › " + strings.Join(labels, " › ")
}

// navStack records the view indices the operator drilled through so `esc` walks
// back up the path. It holds ancestors only — the active view index lives on the
// root model, not the stack. Popping an empty stack returns the cockpit index
// (0): `esc` past the root always lands on Mission Control, never nowhere.
type navStack struct{ stack []int }

// push records the index the operator is leaving so a later pop returns to it.
func (n *navStack) push(i int) { n.stack = append(n.stack, i) }

// pop removes and returns the most recent ancestor. When the stack is empty it
// returns the cockpit index (0) with ok=false, so the caller can distinguish a
// real walk-back (ok) from the root fall-through (land on the cockpit).
func (n *navStack) pop() (idx int, ok bool) {
	if len(n.stack) == 0 {
		return 0, false
	}
	idx = n.stack[len(n.stack)-1]
	n.stack = n.stack[:len(n.stack)-1]
	return idx, true
}

// peek returns the top ancestor without removing it; ok=false on an empty stack.
func (n *navStack) peek() (idx int, ok bool) {
	if len(n.stack) == 0 {
		return 0, false
	}
	return n.stack[len(n.stack)-1], true
}

// depth is the number of ancestors recorded (the number of `esc` walk-backs
// available before the root fall-through).
func (n *navStack) depth() int { return len(n.stack) }

// reset clears the stack (used when jumping directly to a top-level view via the
// slash palette, which starts a fresh path rather than deepening the current one).
func (n *navStack) reset() { n.stack = n.stack[:0] }
