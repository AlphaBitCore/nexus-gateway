package modela

// heldQueue is the FIFO of units the engine is holding back from the client,
// paired with each unit's scan length.
//
// It exists because the obvious spelling leaks memory in a way that is invisible
// until you profile it. `held = append(held, u)` to grow at the back and
// `held = held[1:]` to drop from the front is idiomatic Go on its own, but
// combined the front marches into the backing array and never gives that space
// back, so every append past the remaining capacity allocates a WHOLE NEW array
// and copies. Measured on the streaming hooks-on arm, that single append line
// was 1,015 MB — 96% of everything the engine allocated and 34% of the entire
// profile.
//
// Keeping the array and an offset turns that into a slide: the live window is
// copied back to the front of the array it already owns, once the front has
// consumed at least half the length, which is amortized O(1) per unit.
//
// The two slices move together on purpose. Their indices are parallel — entry i
// of lens describes unit i of units — and compacting one without the other
// would misattribute scan lengths to units, which is the sort of off-by-one
// that delivers unscanned content.
type heldQueue[U any] struct {
	units []U
	lens  []int
	off   int
}

// Len is the number of units still held.
func (q *heldQueue[U]) Len() int { return len(q.units) - q.off }

// Window is the live units, oldest first. The engine hands this to Escalate,
// which must see exactly what is still undelivered.
func (q *heldQueue[U]) Window() []U { return q.units[q.off:] }

// Front is the oldest held unit. Only valid when Len() > 0.
func (q *heldQueue[U]) Front() U { return q.units[q.off] }

// FrontLen is the scan length of the oldest held unit.
func (q *heldQueue[U]) FrontLen() int { return q.lens[q.off] }

// Push appends a unit and its scan length.
func (q *heldQueue[U]) Push(u U, scanLen int) {
	q.units = append(q.units, u)
	q.lens = append(q.lens, scanLen)
}

// Pop drops the oldest unit, zeroing its slot so the payload reaches the GC
// immediately rather than staying reachable through the backing array.
func (q *heldQueue[U]) Pop() {
	var zero U
	q.units[q.off] = zero
	q.off++
	// Slide once the dead prefix is at least as long as the live window. That
	// bound is what keeps the copying amortized: each byte moves at most once
	// per doubling of the live window, rather than on every delivery.
	if q.off >= q.Len() && q.off > 0 {
		n := copy(q.units, q.units[q.off:])
		// Clear the vacated tail. The slide leaves stale copies of the moved
		// units beyond the new length but inside the capacity, and those are
		// still reachable through the backing array — so a delivered response
		// body would stay resident for the life of the stream even though the
		// queue no longer refers to it. Zeroing costs one pass over a region
		// bounded by the live window.
		var zero U
		for i := n; i < len(q.units); i++ {
			q.units[i] = zero
		}
		q.units = q.units[:n]
		m := copy(q.lens, q.lens[q.off:])
		q.lens = q.lens[:m]
		q.off = 0
	}
}
