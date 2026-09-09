package modela

import "testing"

// The queue's contract is what the engine's flush-before-deliver invariant rests
// on: the front unit and the front scan length must always describe the SAME
// unit, and the window handed to Escalate must be exactly what is still
// undelivered. The compaction is invisible to all of that, which is the point —
// so it is tested against the sequence of values, not against the internals.
func TestHeldQueueOrderSurvivesCompaction(t *testing.T) {
	var q heldQueue[int]

	// Push and pop in a pattern that forces several compactions: the front
	// consumes the array repeatedly while the back keeps growing.
	next := 0
	var got []int
	var gotLens []int
	for round := range 200 {
		for range 3 {
			q.Push(next, next*10)
			next++
		}
		if round%2 == 0 && q.Len() > 0 {
			got = append(got, q.Front())
			gotLens = append(gotLens, q.FrontLen())
			q.Pop()
		}
	}
	for q.Len() > 0 {
		got = append(got, q.Front())
		gotLens = append(gotLens, q.FrontLen())
		q.Pop()
	}

	if len(got) != next {
		t.Fatalf("popped %d units, pushed %d", len(got), next)
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("unit %d came out as %d — the FIFO reordered across a compaction", i, v)
		}
		if gotLens[i] != i*10 {
			t.Fatalf("unit %d carries scan length %d, want %d — the two slices compacted "+
				"independently and the lengths no longer describe their units",
				i, gotLens[i], i*10)
		}
	}
}

// Window is what Escalate re-evaluates. A stale or over-long window would hand
// the redactor units that were already delivered.
func TestHeldQueueWindowIsExactlyWhatIsUndelivered(t *testing.T) {
	var q heldQueue[string]
	for _, s := range []string{"a", "b", "c", "d"} {
		q.Push(s, len(s))
	}
	q.Pop()
	q.Pop()

	w := q.Window()
	if len(w) != 2 || w[0] != "c" || w[1] != "d" {
		t.Fatalf("window = %v, want [c d]", w)
	}
	if q.Len() != len(w) {
		t.Errorf("Len()=%d disagrees with len(Window())=%d", q.Len(), len(w))
	}
}

// Pop must drop the reference, or a delivered response body stays reachable
// through the backing array for as long as the stream runs.
func TestHeldQueuePopReleasesThePayload(t *testing.T) {
	var q heldQueue[*int]
	v := new(int)
	q.Push(v, 1)
	q.Push(new(int), 1)
	q.Pop()

	// The popped payload must be unreachable through the WHOLE backing array,
	// not merely absent from the live window: a compaction slides elements left
	// and leaves stale copies behind it, so checking one index would pass while
	// the reference survived somewhere else.
	full := q.units[:cap(q.units)]
	for i, p := range full {
		if p == v {
			t.Errorf("the delivered unit is still reachable at index %d of the backing array "+
				"(len=%d cap=%d); a streamed response would stay resident until the whole "+
				"stream ended", i, len(q.units), cap(q.units))
		}
	}
}

// The allocation property is the reason this type exists. Append-plus-reslice
// reallocates the backing array every time the front marches through it, which
// measured as 96% of the engine's allocations; keeping the array and sliding
// the window keeps a long stream flat.
func TestHeldQueueDoesNotReallocatePerUnit(t *testing.T) {
	var q heldQueue[int]
	const steady = 32

	for i := range steady {
		q.Push(i, i)
	}
	capAfterFill := cap(q.units)

	// Steady state: one in, one out, thousands of times. A queue that reslices
	// its way forward reallocates roughly every capAfterFill iterations.
	for i := range 100_000 {
		q.Push(i, i)
		q.Pop()
	}

	if cap(q.units) > capAfterFill*4 {
		t.Errorf("backing array grew from %d to %d over a steady-state stream; the front is "+
			"marching into the array instead of the window sliding back", capAfterFill, cap(q.units))
	}
	if q.Len() != steady {
		t.Errorf("steady-state length drifted to %d, want %d", q.Len(), steady)
	}
}
