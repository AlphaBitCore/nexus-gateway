//go:build vectorscan

// Package matcher — the streaming prefilter must agree with the block scan.
//
// Named failure modes:
//   - the streaming database answers "no match" for content the block scan
//     matches, so a rule silently stops being enforced on streamed responses
//   - a pattern spanning a chunk boundary is missed, which is the entire reason
//     the tail window exists and the entire reason streaming mode can replace it
//   - a stream leaks engine state or a scratch back into the ring twice
package matcher

import (
	"testing"
	"time"
)

// seedPatterns already exists in vectorscan_test.go, loading every starter-pack
// rule from the shipped yaml. Reusing it rather than reading the JSON fixtures
// keeps this file measuring the same rule set the rest of the package does — a
// second loader would be a second thing to drift.

// probeCorpus is the content both modes are asked about. Each entry names what
// it is FOR: a corpus of only-misses would let a stream that never matches
// anything pass, and a corpus of only-hits would let one that always matches.
var probeCorpus = []struct {
	name string
	text string
	want string // "hit" or "miss", as a sanity anchor on the corpus itself
}{
	{"plain prose", "The weather in Paris is mild today and the meeting is at three.", "miss"},
	{"code, no secrets", "func add(a, b int) int { return a + b }", "miss"},
	{"ssn", "the customer's social is 123-45-6789, please verify", "hit"},
	{"email", "reach me at someone@example.com when ready", "hit"},
	{"aws key shape", "AKIAIOSFODNN7EXAMPLE is the access key id", "hit"},
	{"private key header", "-----BEGIN RSA PRIVATE KEY-----\nMIIEow", "hit"},
	{"bulk insert", "INSERT INTO users (a,b) VALUES (1,2),(3,4),(5,6),(7,8),(9,10),(11,12)", "hit"},
	{"jwt-ish", "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.abc", "hit"},
}

// blockSaysMayMatch is the oracle: what the accumulate-and-rescan path answers.
func blockSaysMayMatch(m Matcher, text string) bool {
	return len(m.Scan([]string{text}, true)) > 0
}

// TestStreamAgreesWithBlockOnWholeInput is the base case: fed in one write, the
// two modes must answer identically for every probe.
func TestStreamAgreesWithBlockOnWholeInput(t *testing.T) {
	m, bad := CompileVectorscan(seedPatterns(t))
	if len(bad) > 0 {
		t.Logf("%d patterns fell to the RE2 residual (they have no streaming path either)", len(bad))
	}
	defer func() { _ = m.(interface{ Close() error }).Close() }()

	ss, ok := m.(StreamScanner)
	if !ok {
		t.Fatal("the Vectorscan matcher does not implement StreamScanner — the streaming " +
			"database failed to compile, and every streamed response is back on the " +
			"accumulate-and-rescan path")
	}

	for _, tc := range probeCorpus {
		t.Run(tc.name, func(t *testing.T) {
			block := blockSaysMayMatch(m, tc.text)
			if got := map[bool]string{true: "hit", false: "miss"}[block]; got != tc.want {
				t.Fatalf("the corpus entry is mislabelled: block scan says %s, the entry claims %s. "+
					"A corpus that does not exercise both answers cannot detect a stream that "+
					"always says one of them.", got, tc.want)
			}

			st, err := ss.OpenScanStream()
			if err != nil {
				t.Fatalf("OpenScanStream: %v", err)
			}
			defer st.Close()
			stream, err := st.Write([]byte(tc.text))
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if stream != block {
				t.Errorf("stream says mayMatch=%v, block says %v — the prefilter and the scan it "+
					"gates disagree, so a rule is either never confirmed or confirmed on every "+
					"chunk", stream, block)
			}
		})
	}
}

// TestStreamFindsPatternsSplitAcrossWrites is the case the tail window exists
// for. Feeding one byte at a time is the harshest split available: if the engine
// really carries state, a match spanning every boundary is still found.
func TestStreamFindsPatternsSplitAcrossWrites(t *testing.T) {
	m, _ := CompileVectorscan(seedPatterns(t))
	defer func() { _ = m.(interface{ Close() error }).Close() }()
	ss := m.(StreamScanner)

	for _, tc := range probeCorpus {
		if tc.want != "hit" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			for _, chunk := range []int{1, 3, 7} {
				st, err := ss.OpenScanStream()
				if err != nil {
					t.Fatalf("OpenScanStream: %v", err)
				}
				var seen bool
				for i := 0; i < len(tc.text); i += chunk {
					end := min(i+chunk, len(tc.text))
					if seen, err = st.Write([]byte(tc.text[i:end])); err != nil {
						t.Fatalf("Write: %v", err)
					}
					if seen {
						break
					}
				}
				st.Close()
				if !seen {
					t.Errorf("fed %d bytes at a time, the stream never reported a match — a value "+
						"split across writes is exactly what carrying state is for, and missing "+
						"it means a streamed response is scanned as if it were unrelated "+
						"fragments", chunk)
				}
			}
		})
	}
}

// TestStreamNeverMatchesCleanContentHoweverSplit is the other half: splitting
// must not INVENT a match either, or every clean response pays for a full
// confirm.
func TestStreamNeverMatchesCleanContentHoweverSplit(t *testing.T) {
	m, _ := CompileVectorscan(seedPatterns(t))
	defer func() { _ = m.(interface{ Close() error }).Close() }()
	ss := m.(StreamScanner)

	for _, tc := range probeCorpus {
		if tc.want != "miss" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			st, err := ss.OpenScanStream()
			if err != nil {
				t.Fatalf("OpenScanStream: %v", err)
			}
			defer st.Close()
			for i := range len(tc.text) {
				seen, err := st.Write([]byte(tc.text[i : i+1]))
				if err != nil {
					t.Fatalf("Write: %v", err)
				}
				if seen {
					t.Fatalf("clean content reported a match at byte %d — a prefilter that fires "+
						"on everything makes the confirm run on every chunk, which is the cost "+
						"the prefilter exists to avoid", i)
				}
			}
		})
	}
}

// TestStreamStateIsBounded records the per-stream cost, because a caller holding
// one stream per in-flight response needs a number to budget against.
func TestStreamStateIsBounded(t *testing.T) {
	m, _ := CompileVectorscan(seedPatterns(t))
	defer func() { _ = m.(interface{ Close() error }).Close() }()

	sized, ok := m.(interface{ StreamStateBytes() int })
	if !ok {
		t.Skip("matcher does not report stream state size")
	}
	n := sized.StreamStateBytes()
	if n <= 0 {
		t.Fatalf("StreamStateBytes = %d — a caller cannot budget concurrent streams against it", n)
	}
	// Not a performance assertion, a blast-radius one: an engine change that made
	// this megabytes would turn 1000 concurrent streams into gigabytes.
	if n > 64*1024 {
		t.Errorf("per-stream state is %d B; at 1000 concurrent streams that is %.1f MB, which "+
			"is no longer a rounding error", n, float64(n*1000)/(1024*1024))
	}
	t.Logf("per-stream engine state: %d B (1000 concurrent streams = %.2f MB)",
		n, float64(n*1000)/(1024*1024))
}

// TestStreamClosesAreIdempotent guards the scratch ring: a double Close would
// return the same scratch twice and hand one buffer to two concurrent scans.
func TestStreamClosesAreIdempotent(t *testing.T) {
	m, _ := CompileVectorscan(seedPatterns(t))
	defer func() { _ = m.(interface{ Close() error }).Close() }()
	ss := m.(StreamScanner)

	st, err := ss.OpenScanStream()
	if err != nil {
		t.Fatalf("OpenScanStream: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("second Close returned %v; it must be a no-op, not a second release of the "+
			"same scratch", err)
	}
	if _, err := st.Write([]byte("x")); err == nil {
		t.Error("Write after Close succeeded — it would scan through a freed stream handle")
	}
}

// TestStreamEmptyWriteIsNotAnAnswer: an empty write must neither error nor
// change the verdict, because a chunk carrying no redactable content is normal
// (a role-only frame, a usage frame) and must not be mistaken for a miss.
//
// It deliberately does NOT call the matcher's Close while the stream is open.
// That combination DEADLOCKS: Close spin-waits for the inflight count to reach
// zero, and an open stream holds one for its whole life — which for a real
// response is seconds, not the microseconds a Scan holds it for. Writing this
// test the other way round is how that was found, and the same hazard is real
// for a config swap that closes a matcher while responses are still streaming.
func TestStreamEmptyWriteIsNotAnAnswer(t *testing.T) {
	m, _ := CompileVectorscan(seedPatterns(t))
	ss := m.(StreamScanner)
	st, err := ss.OpenScanStream()
	if err != nil {
		t.Fatalf("OpenScanStream: %v", err)
	}
	if seen, err := st.Write(nil); err != nil || seen {
		t.Errorf("empty write: seen=%v err=%v, want false/nil", seen, err)
	}
	st.Close()
	_ = m.(interface{ Close() error }).Close()
}

// TestCloseDoesNotWaitForOpenStreams is the gate on the hazard that writing an
// earlier test the wrong way round exposed.
//
// A Scan holds the matcher's inflight count for microseconds, so Close
// spin-waiting on it is fine. A scan STREAM holds a reference for the length of
// a response — seconds — and if that reference were the same counter, a config
// swap during live streaming would busy-wait on runtime.Gosched() until the
// response finished. The first version did exactly that and hung the test
// binary for its full timeout.
//
// So Close must return while streams are still open, and the last stream to
// close must be the one that frees the streaming database. The deadline here is
// generous on purpose: it is not measuring speed, it is distinguishing "returns"
// from "waits for the stream".
func TestCloseDoesNotWaitForOpenStreams(t *testing.T) {
	m, _ := CompileVectorscan(seedPatterns(t))
	ss := m.(StreamScanner)

	st, err := ss.OpenScanStream()
	if err != nil {
		t.Fatalf("OpenScanStream: %v", err)
	}

	done := make(chan struct{})
	go func() {
		_ = m.(interface{ Close() error }).Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Leaving the stream open on purpose: closing it here would let the
		// blocked Close finish and turn a hang into a pass.
		t.Fatal("Close did not return while a scan stream was open. It is waiting for the " +
			"stream to finish, which for a real response is seconds of busy-wait on a config " +
			"swap.")
	}

	// The database must still be usable by the stream that outlived Close.
	if _, err := st.Write([]byte("the customer's social is 123-45-6789")); err != nil {
		t.Errorf("Write after the matcher closed: %v — the stream held a reference and must "+
			"still be able to finish its response", err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("Close of the surviving stream: %v", err)
	}
}

// TestOpenScanStreamRefusesAfterClose pins the other side: once closed, no NEW
// stream may take a reference, or the database could be resurrected after its
// last release.
func TestOpenScanStreamRefusesAfterClose(t *testing.T) {
	m, _ := CompileVectorscan(seedPatterns(t))
	ss := m.(StreamScanner)
	if err := m.(interface{ Close() error }).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := ss.OpenScanStream(); err == nil {
		t.Error("OpenScanStream succeeded on a closed matcher — it would scan through a freed " +
			"database")
	}
}
