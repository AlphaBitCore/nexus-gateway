package store

import (
	"testing"
	"time"
)

// Covers is the SQL predicate transcribed into Go, and the transcription is
// where an off-by-one would enter. The two boundaries are not symmetric —
// an assignment IS in force at its own assigned_at and is NOT at its
// released_at — and getting either wrong attributes a request to the wrong
// person for the length of one window edge.
func TestAssignmentWindowCovers(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 8, 30, h, 0, 0, 0, time.UTC) }
	released := at(12)

	open := DeviceAssignmentWindow{AssignedAt: at(9)}
	closed := DeviceAssignmentWindow{AssignedAt: at(9), ReleasedAt: &released}

	cases := []struct {
		name string
		w    DeviceAssignmentWindow
		ts   time.Time
		want bool
	}{
		{"before assignment", open, at(8), false},
		{"exactly at assignment is INSIDE", open, at(9), true},
		{"after assignment, never released", open, at(23), true},
		{"inside a closed window", closed, at(10), true},
		{"exactly at release is OUTSIDE", closed, released, false},
		{"after release", closed, at(13), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.Covers(tc.ts); got != tc.want {
				t.Errorf("Covers(%v) = %v, want %v — this is the boundary the SQL states as "+
					"assigned_at <= ts AND (released_at IS NULL OR released_at > ts)",
					tc.ts, got, tc.want)
			}
		})
	}
}
