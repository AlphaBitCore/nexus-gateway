//go:build vectorscan

package matcher

import (
	"sync"
	"testing"
	"time"
)

// A scan stream lives for the length of a response, which is orders of
// magnitude longer than a Scan. Everything it borrows therefore outlives the
// window in which the matcher can be closed by a config swap, and every one of
// those borrows was wrong:
//
//   - the scratch came from the BLOCK database's ring, and was used to scan the
//     STREAMING database;
//   - it was returned to that ring after Close had already drained it;
//   - and the allocation itself ran with no reference held, so Close could free
//     the database out from under it.
//
// These are one defect with three faces, so they are gated together.

func openTestStream(t *testing.T) (Matcher, ScanStream) {
	t.Helper()
	m, _ := CompileVectorscan(seedPatterns(t))
	ss, ok := m.(StreamScanner)
	if !ok {
		t.Skip("this matcher has no streaming prefilter")
	}
	st, err := ss.OpenScanStream()
	if err != nil {
		t.Fatalf("OpenScanStream: %v", err)
	}
	return m, st
}

// TestStreamScratchIsNotParkedInTheBlockRing pins the leak. Close drains the
// idle ring exactly once; anything returned to it afterwards is never freed.
func TestStreamScratchIsNotParkedInTheBlockRing(t *testing.T) {
	m, st := openTestStream(t)
	vm := m.(*vectorscanMatcher)

	if err := m.(interface{ Close() error }).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	drained := len(vm.idle)
	if drained != 0 {
		t.Fatalf("the idle ring held %d scratches after Close drained it; the premise of this "+
			"gate is that Close leaves it empty", drained)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("stream Close: %v", err)
	}
	if parked := len(vm.idle); parked != 0 {
		t.Errorf("the stream returned %d scratch(es) to a ring nobody will drain again — one "+
			"leaked Hyperscan scratch per config swap that races a live stream", parked)
	}
}

// TestStreamPrefilterStaysNegativeOnCleanText is the gate on the wrong-database
// scratch. Feeding a stream scratch that was sized for a different database
// makes hs_scan_stream fail, and the failure path sets seen = true — so the
// prefilter answers "look closer" for everything, the optimisation disappears,
// and nothing else in the suite notices.
func TestStreamPrefilterStaysNegativeOnCleanText(t *testing.T) {
	m, st := openTestStream(t)
	defer m.(interface{ Close() error }).Close()
	defer st.Close()

	clean := []byte("the weather today is mild and the meeting starts at ten")
	for i := 0; i < 8; i++ {
		seen, err := st.Write(clean)
		if err != nil {
			t.Fatalf("write %d: %v — a prefilter that errors reports a match it did not see", i, err)
		}
		if seen {
			t.Fatalf("write %d reported a possible match on text carrying none; the prefilter is "+
				"answering yes unconditionally, which costs a full authoritative scan per chunk "+
				"while looking exactly like correct behaviour", i)
		}
	}
}

// TestStreamStateBytesAfterCloseDoesNotTouchAFreedDatabase covers the reader
// that had no guard at all: it read m.streamDB and called into the engine with
// it while the release was freeing and nilling that same field.
func TestStreamStateBytesAfterCloseDoesNotTouchAFreedDatabase(t *testing.T) {
	m, _ := CompileVectorscan(seedPatterns(t))
	if _, ok := m.(StreamScanner); !ok {
		t.Skip("this matcher has no streaming prefilter")
	}
	if n := m.(*vectorscanMatcher).StreamStateBytes(); n <= 0 {
		t.Fatalf("StreamStateBytes()=%d on a live matcher; the gate below needs a non-zero "+
			"baseline to be meaningful", n)
	}
	if err := m.(interface{ Close() error }).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := m.(*vectorscanMatcher).StreamStateBytes(); n != 0 {
		t.Errorf("StreamStateBytes()=%d after Close; it must report zero rather than read a "+
			"database that has been freed", n)
	}
}

// lifecyclePatterns is deliberately tiny. The subject is the database's
// lifecycle, not what it matches, and the seed rule set costs seconds to compile
// under -race — enough that a loop wide enough to actually hit the interleave
// would time out before reaching it, which reads as "no race found".
func lifecyclePatterns() []Pattern {
	return []Pattern{
		{ID: 1, Expr: `\d{3}-\d{2}-\d{4}`, Flags: ""},
		{ID: 2, Expr: `social`, Flags: "i"},
	}
}

// TestOpenScanStreamRacesCloseSafely is the crash case, reproduced by the
// original report with -race and a SIGSEGV inside hs_alloc_scratch. Run this
// package with -race for it to mean what it says.
//
// Two things make it more than a smoke test. Several openers per round widen the
// window in which one of them is inside acquireStreamDB while Close drops the
// matcher's own reference — a single opener races the goroutine scheduler for
// that window and usually loses. And the run is only meaningful if some opener
// WINS: a round where Close always got there first exercised the refusal path,
// which TestOpenScanStreamRefusesAfterClose already covers, and says nothing
// about a stream that opened ACROSS the close. Without the floor at the bottom,
// this test stays green while never once reaching the case it is named for.
func TestOpenScanStreamRacesCloseSafely(t *testing.T) {
	const (
		rounds  = 300
		openers = 8
	)

	won := 0

	for round := 0; round < rounds; round++ {
		m, err := CompileVectorscan(lifecyclePatterns())
		if err != nil {
			t.Fatalf("round %d: CompileVectorscan: %v", round, err)
		}
		ss, ok := m.(StreamScanner)
		if !ok {
			t.Skip("this matcher has no streaming prefilter")
		}
		closer := m.(interface{ Close() error })

		var wg sync.WaitGroup
		start := make(chan struct{})
		streams := make([]ScanStream, openers)

		wg.Add(openers + 1)
		go func() {
			defer wg.Done()
			<-start
			_ = closer.Close()
		}()
		for j := 0; j < openers; j++ {
			go func(j int) {
				defer wg.Done()
				<-start
				// Either outcome is correct: a stream opened before the close, or
				// a refusal. What must not happen is a scratch allocated against
				// a database another goroutine is freeing.
				if st, err := ss.OpenScanStream(); err == nil {
					streams[j] = st
				}
			}(j)
		}
		close(start)

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Close and OpenScanStream deadlocked against each other")
		}

		// A stream that opened holds a reference, so the database must still be
		// there for it to finish its response on.
		for _, st := range streams {
			if st == nil {
				continue
			}
			won++
			if _, err := st.Write([]byte("the customer's social is 123-45-6789")); err != nil {
				t.Fatalf("round %d: write on a stream that opened successfully: %v — the "+
					"reference it took must keep the database alive", round, err)
			}
			if err := st.Close(); err != nil {
				t.Fatalf("round %d: closing a surviving stream: %v", round, err)
			}
		}
	}

	if won == 0 {
		t.Fatalf("no opener ever won the race across %d rounds — every round took the refusal "+
			"path, so this test never reached the open-across-close interleave it exists for",
			rounds)
	}
	t.Logf("streams that opened across a concurrent Close: %d over %d rounds", won, rounds)
}
