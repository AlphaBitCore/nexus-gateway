//go:build vectorscan

package matcher

import (
	"runtime"
	"sync"
	"testing"
)

func TestCgoScanLimit_Parse(t *testing.T) {
	adaptive := adaptiveCgoScanLimit(runtime.GOMAXPROCS(0))
	cases := []struct {
		env  string
		want int
	}{
		{"", adaptive},     // unset → adaptive (DEFAULT; opt-out)
		{"8", 8},           // explicit cap
		{"0", 0},           // explicit disable
		{"-3", 0},          // negative → disable
		{"abc", 0},         // unparsable → disable
		{" 4 ", 4},         // trimmed
		{"auto", adaptive}, // adaptive default from GOMAXPROCS
		{"AUTO", adaptive}, // case-insensitive
	}
	for _, c := range cases {
		t.Setenv("NEXUS_CGO_SCAN_LIMIT", c.env)
		if got := cgoScanLimit(); got != c.want {
			t.Errorf("cgoScanLimit(env=%q) = %d, want %d", c.env, got, c.want)
		}
	}
}

func TestAdaptiveCgoScanLimit(t *testing.T) {
	cases := []struct{ procs, want int }{
		{12, 10}, // 12 - max(2, 1) = 10
		{8, 6},   // 8  - max(2, 1) = 6
		{4, 2},   // 4  - 2 = 2
		{32, 28}, // 32 - max(2, 4) = 28
		{2, 1},   // 2  - 2 = 0 → floor 1
		{1, 1},   // 1  - 2 = -1 → floor 1
		{0, 1},   // procs clamped to 1 → floor 1
	}
	for _, c := range cases {
		if got := adaptiveCgoScanLimit(c.procs); got != c.want {
			t.Errorf("adaptiveCgoScanLimit(%d) = %d, want %d", c.procs, got, c.want)
		}
	}
}

// TestVectorscan_ScanUnderCgoLimit installs a small semaphore directly and drives
// many concurrent scans through it, proving the cgo concurrency cap does not
// corrupt or drop results (correctness under the limiter).
func TestVectorscan_ScanUnderCgoLimit(t *testing.T) {
	old := cgoScanSem
	cgoScanSem = make(chan struct{}, 2) // cap 2 → at most 2 concurrent cgo scans
	defer func() { cgoScanSem = old }()

	m, bad := CompileVectorscan([]Pattern{{ID: 0, Expr: `AKIA[0-9A-Z]{16}`}})
	if len(bad) != 0 {
		t.Fatalf("unexpected bad patterns: %+v", bad)
	}
	defer m.(*vectorscanMatcher).Close()

	const workers = 64
	var wg sync.WaitGroup
	errc := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := m.Scan([]string{"creds AKIA1234567890ABCDEF here"}, true)
			ok := false
			for _, hit := range h {
				if hit.ID == 0 {
					ok = true
				}
			}
			if !ok {
				errc <- "missing expected hit under cgo limit"
			}
		}()
	}
	wg.Wait()
	close(errc)
	if msg, bad := <-errc; bad {
		t.Fatalf("scan under cgo limit wrong: %s", msg)
	}
}

// TestVectorscan_ScanComplete_ReportsCompletion covers the CompleteScanner path:
// a normal (non-truncated) scan reports complete=true and still returns its hits;
// an empty-segment scan is vacuously complete.
func TestVectorscan_ScanComplete_ReportsCompletion(t *testing.T) {
	m, bad := CompileVectorscan([]Pattern{{ID: 0, Expr: `AKIA[0-9A-Z]{16}`}})
	if len(bad) != 0 {
		t.Fatalf("unexpected bad patterns: %+v", bad)
	}
	defer m.(*vectorscanMatcher).Close()

	cs, ok := m.(CompleteScanner)
	if !ok {
		t.Fatal("vectorscan matcher must implement CompleteScanner")
	}
	hits, complete := cs.ScanComplete([]string{"creds AKIA1234567890ABCDEF"}, true)
	if !complete {
		t.Error("a normal scan must report complete=true")
	}
	found := false
	for _, h := range hits {
		if h.ID == 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("ScanComplete must return the hits; got %+v", hits)
	}
	if h2, c2 := cs.ScanComplete(nil, true); !c2 || len(h2) != 0 {
		t.Errorf("empty-segment scan: want (no hits, complete); got (%v, %v)", h2, c2)
	}
}
