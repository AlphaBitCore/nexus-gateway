package localfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, root string) *Store {
	t.Helper()
	st, err := New(Options{Root: root})
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	return st
}

func seedObjects(t *testing.T, root string, n int) {
	t.Helper()
	day := filepath.Join(root, "2026-07-28")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := range n {
		p := filepath.Join(day, "obj-"+itoa(i)+"-request.bin")
		if err := os.WriteFile(p, []byte("body"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// A spill root larger than the scan bound must report a LOWER BOUND, not a total.
// The failure this guards is the quiet one: returning the partial count unlabelled,
// so an operator reading "12 objects" concludes the store is nearly empty when it
// holds a retention window's worth.
func TestStat_OverTheScanLimitReportsTruncated(t *testing.T) {
	root := t.TempDir()
	seedObjects(t, root, 12)

	orig := statScanLimit
	statScanLimit = 5
	t.Cleanup(func() { statScanLimit = orig })

	st, err := newTestStore(t, root).Stat(context.Background())
	if err != nil {
		t.Fatalf("a bounded scan is not a failure: %v", err)
	}
	if !st.Truncated {
		t.Error("Truncated must be set when the walk stops at the limit")
	}
	if st.ScanLimit != 5 {
		t.Errorf("ScanLimit = %d; want the bound that was applied", st.ScanLimit)
	}
	if st.ObjectCount != 5 {
		t.Errorf("ObjectCount = %d; want exactly the bound (%d)", st.ObjectCount, 5)
	}
}

// Under the bound, nothing is truncated and the numbers are totals.
func TestStat_UnderTheLimitIsNotTruncated(t *testing.T) {
	root := t.TempDir()
	seedObjects(t, root, 3)
	st, err := newTestStore(t, root).Stat(context.Background())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Truncated {
		t.Error("a complete scan must not report Truncated")
	}
	if st.ObjectCount != 3 || st.TotalBytes != 12 {
		t.Errorf("ObjectCount/TotalBytes = %d/%d; want 3/12", st.ObjectCount, st.TotalBytes)
	}
	if st.OldestAt.IsZero() || st.NewestAt.IsZero() {
		t.Error("the age window must be populated from the objects walked")
	}
}

// A cancelled context must stop the walk AND surface the cancellation. Returning
// nil error with a small count would make a timeout indistinguishable from an
// almost-empty store — the reason this scan had no production consumer before.
func TestStat_CancelledContextStopsAndReportsBoth(t *testing.T) {
	root := t.TempDir()
	seedObjects(t, root, 20)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	st, err := newTestStore(t, root).Stat(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want context.Canceled so a timeout reads as a timeout", err)
	}
	if !st.Truncated {
		t.Error("a cancelled scan's numbers are lower bounds and must say so")
	}
}

// A deadline that expires mid-walk behaves the same way.
func TestStat_DeadlineExceededIsReported(t *testing.T) {
	root := t.TempDir()
	seedObjects(t, root, 20)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, err := newTestStore(t, root).Stat(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; want context.DeadlineExceeded", err)
	}
}

// A spill root that is gone — deleted, or a mount that dropped — must be an
// ERROR, not an empty store. The operator reaching for this number is usually
// doing so BECAUSE spilled bodies are unreadable; answering "configured, 0
// objects" sends them to look somewhere else. filepath.Walk routes a root-level
// lstat failure through the same callback that deliberately tolerates per-entry
// errors, which is how the two became indistinguishable.
func TestStat_MissingRootIsAnErrorNotAnEmptyStore(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(root, "spill")
	if err := os.MkdirAll(gone, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	st := newTestStore(t, gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}

	stats, err := st.Stat(context.Background())
	if err == nil {
		t.Fatalf("a missing spill root reported as a healthy store: %+v", stats)
	}
	if !strings.Contains(err.Error(), "spill root unreadable") {
		t.Errorf("err = %v; the message must name the root, or the operator learns nothing", err)
	}
	if stats.ObjectCount != 0 || stats.TotalBytes != 0 {
		t.Errorf("a failed measurement must not report counts: %+v", stats)
	}
}
