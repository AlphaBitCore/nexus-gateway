package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// deadlineRecorder is a ResponseWriter that records SetWriteDeadline calls so
// the test can assert the idle-extension behaviour without a real connection.
// http.NewResponseController reaches SetWriteDeadline via the method set, so
// embedding a recorder that also exposes Flush lets us verify forwarding too.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	flushes   int
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func (d *deadlineRecorder) Flush() { d.flushes++ }

// TestStreamIdleWriter_WriteResetsDeadlinePerChunk proves the core property:
// every chunk write pushes the connection write deadline forward by ~idle, so
// an actively-producing stream is never cut while a stalled one eventually is.
func TestStreamIdleWriter_WriteResetsDeadlinePerChunk(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	rc := http.NewResponseController(rec)
	w := &streamIdleWriter{ResponseWriter: rec, rc: rc, idle: 90 * time.Second}

	before := time.Now()
	n, err := w.Write([]byte("data: one\n\n"))
	if err != nil || n == 0 {
		t.Fatalf("write 1: n=%d err=%v", n, err)
	}
	if len(rec.deadlines) != 1 {
		t.Fatalf("first write should set exactly one deadline, got %d", len(rec.deadlines))
	}
	// Deadline should be roughly now+idle (allow generous slack for slow CI).
	if got := rec.deadlines[0]; got.Before(before.Add(80*time.Second)) || got.After(before.Add(120*time.Second)) {
		t.Errorf("deadline %v not ~ now+90s (base %v)", got, before)
	}

	// The remaining writes of the same frame land inside the coalescing
	// window and share that one reset — see writeDeadlineArmGranularity.
	if _, err := w.Write([]byte("data: two\n\n")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if len(rec.deadlines) != 1 {
		t.Errorf("writes within one granularity must share a reset; got %d", len(rec.deadlines))
	}

	// Once the window has passed, a producing stream must push the deadline
	// forward again — otherwise a long stream would eventually be cut while
	// still actively sending, which is exactly what this writer prevents.
	time.Sleep(2 * writeDeadlineArmGranularity)
	mid := time.Now()
	if _, err := w.Write([]byte("data: three\n\n")); err != nil {
		t.Fatalf("write 3: %v", err)
	}
	if len(rec.deadlines) != 2 {
		t.Fatalf("write after the window must reset the deadline; got %d", len(rec.deadlines))
	}
	if !rec.deadlines[1].After(rec.deadlines[0]) {
		t.Errorf("second reset %v must be later than the first %v", rec.deadlines[1], rec.deadlines[0])
	}
	if got := rec.deadlines[1]; got.Before(mid.Add(80*time.Second)) || got.After(mid.Add(120*time.Second)) {
		t.Errorf("deadline %v not ~ now+90s (base %v)", got, mid)
	}

	// The bytes actually reach the underlying writer.
	if body := rec.Body.String(); body != "data: one\n\ndata: two\n\ndata: three\n\n" {
		t.Errorf("underlying body = %q", body)
	}
}

// TestStreamIdleWriter_ZeroIdleSkipsDeadline covers the defensive idle<=0 path
// (a misconfigured zero must not call SetWriteDeadline with now, which would
// instantly fail every write).
func TestStreamIdleWriter_ZeroIdleSkipsDeadline(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &streamIdleWriter{ResponseWriter: rec, rc: http.NewResponseController(rec), idle: 0}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(rec.deadlines) != 0 {
		t.Errorf("idle=0 must not set a deadline, got %d", len(rec.deadlines))
	}
}

// TestStreamIdleWriter_FlushAndUnwrapForward proves the tee's flusher and
// http.NewResponseController still reach the underlying writer through the
// wrapper.
func TestStreamIdleWriter_FlushAndUnwrapForward(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &streamIdleWriter{ResponseWriter: rec, rc: http.NewResponseController(rec), idle: time.Second}

	w.Flush()
	if rec.flushes != 1 {
		t.Errorf("Flush not forwarded: got %d", rec.flushes)
	}
	if w.Unwrap() != http.ResponseWriter(rec) {
		t.Error("Unwrap must return the embedded ResponseWriter")
	}
}
