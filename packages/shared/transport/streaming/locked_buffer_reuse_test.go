package streaming

import (
	"bytes"
	"testing"
)

// SnapshotInto reuses its destination across calls (finding C-22). These pin the two
// properties that makes it safe to substitute for Snapshot at the pre-hook call site,
// plus the exported contract it must NOT have changed.

// TestSnapshotInto_ReuseAcrossGrowingWrites is the correctness assertion for reuse. The
// LivePipeline calls this once per checkpoint on a buffer that only ever grows, handing
// the result to the pre-hook, so a stale tail or a short read would feed hooks a
// corrupted transcript — and hooks would scan it and approve, with no error anywhere.
func TestSnapshotInto_ReuseAcrossGrowingWrites(t *testing.T) {
	var l LockedByteBuffer
	var dst []byte

	steps := []string{
		"data: one\n\n",
		"data: two-is-longer\n\n",
		"data: three\n\n",
	}
	var want bytes.Buffer
	for i, chunk := range steps {
		if _, err := l.Write([]byte(chunk)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		want.WriteString(chunk)

		dst = l.SnapshotInto(dst)
		if string(dst) != want.String() {
			t.Fatalf("snapshot %d = %q, want %q (reuse corrupted the transcript)", i, dst, want.String())
		}
	}
}

// TestSnapshotInto_ShrinkingDestinationLeavesNoStaleTail covers the case the growing
// sequence above cannot reach: a destination that already holds MORE bytes than the
// source. The accumulator never shrinks in production, so this is defence for a future
// caller reusing one destination across two different buffers — the shape in which a
// stale tail would otherwise leak one stream's bytes into another's hook input.
func TestSnapshotInto_ShrinkingDestinationLeavesNoStaleTail(t *testing.T) {
	var big LockedByteBuffer
	big.Write([]byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	dst := big.SnapshotInto(nil)

	var small LockedByteBuffer
	small.Write([]byte("BBB"))
	dst = small.SnapshotInto(dst)

	if string(dst) != "BBB" {
		t.Fatalf("snapshot = %q, want %q: a longer previous snapshot leaked its tail", dst, "BBB")
	}
}

// TestSnapshot_StillReturnsAnIndependentCopy pins the EXPORTED contract that was
// deliberately left alone. Snapshot promises the caller may retain and mutate the
// result, and ai-gateway/internal/platform/streaming relies on that; SnapshotInto was
// added as a sibling rather than a change for exactly this reason. If someone later
// "unifies" the two by making Snapshot delegate to a shared destination, this fails.
func TestSnapshot_StillReturnsAnIndependentCopy(t *testing.T) {
	var l LockedByteBuffer
	l.Write([]byte("original"))

	first := l.Snapshot()
	first[0] = 'X' // the documented right to mutate

	if got := string(l.Snapshot()); got != "original" {
		t.Fatalf("second snapshot = %q, want %q: mutating one snapshot must not affect the buffer or another", got, "original")
	}
	l.Write([]byte("-more"))
	if got := string(first); got != "Xriginal" {
		t.Fatalf("retained snapshot = %q, want %q: a later write must not disturb a retained snapshot", got, "Xriginal")
	}
}
