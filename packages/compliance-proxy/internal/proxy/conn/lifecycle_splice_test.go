package conn

import (
	"io"
	"net"
	"testing"
	"time"
)

// IdleConn must NOT expose io.ReaderFrom or io.WriterTo, and that is a decision rather
// than an oversight (finding C-8).
//
// Exposing them would let io.Copy reach *net.TCPConn's fast path — splice(2) on Linux —
// on the CONNECT passthrough relay, which is where the whole tunnel's throughput lives.
// It would also break this wrapper's guarantee. The idle guard works by observing
// userspace Read/Write calls and resetting a time.AfterFunc on each one; splice moves the
// copy loop into the kernel and Go's poll.Splice does not return between syscalls, so a
// single ReadFrom can span an entire multi-minute transfer without one reset. The 300s
// timer configured in both compliance-proxy.config.yaml and .dev.yaml would then fire
// while bytes are flowing normally and close a healthy tunnel mid-download — turning
// "idle for 300s" into "open for 300s".
//
// So the two features are incompatible as designed. Recovering splice requires observing
// progress from OUTSIDE the copy path (byte counters, or socket deadlines extended by a
// watchdog), not delegating these methods. This test exists so that anyone who adds the
// delegation to chase throughput fails here and reads why first.
func TestIdleConn_DoesNotExposeSpliceFastPath(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	ic := NewIdleConn(c2, time.Minute)
	defer ic.Close()

	if _, ok := any(ic).(io.ReaderFrom); ok {
		t.Fatal("IdleConn implements io.ReaderFrom. io.Copy would then hand the copy loop to " +
			"the kernel via splice, and the idle guard — which resets its timer from Read/Write — " +
			"would stop being reset for the duration of the transfer, closing a healthy tunnel " +
			"when the timer expires. See this test's doc comment before delegating.")
	}
	if _, ok := any(ic).(io.WriterTo); ok {
		t.Fatal("IdleConn implements io.WriterTo — same incompatibility as io.ReaderFrom above, " +
			"in the other direction.")
	}
}

// TestIdleConn_ResetsOnlyOnProgress pins the property the incompatibility rests on: the
// timer is reset by observed byte movement, not by the call itself. A wrapper that reset on
// every call regardless would not have the same conflict, so the reason matters.
func TestIdleConn_ResetsOnlyOnProgress(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()

	ic := NewIdleConn(c2, 50*time.Millisecond)

	// A read that moves bytes keeps the connection alive past the original deadline.
	go func() {
		for range 4 {
			time.Sleep(20 * time.Millisecond)
			_, _ = c1.Write([]byte("x"))
		}
	}()

	buf := make([]byte, 1)
	for i := range 4 {
		if _, err := ic.Read(buf); err != nil {
			t.Fatalf("read %d failed at %v: the idle timer fired despite bytes moving, so it is "+
				"not being reset on progress", i, err)
		}
	}
	_ = ic.Close()
}
