package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// armMode says how often the benchmark lets the writer reset the connection
// write deadline, so the two production shapes can be compared directly.
type armMode int

const (
	// armEveryWrite is the pre-coalescing behaviour: every Write resets.
	armEveryWrite armMode = iota
	// armPerFrame is what the coalescing window produces in production,
	// where frames arrive milliseconds apart: one reset per frame.
	armPerFrame
	// armNever is the floor — the same writes with no deadline at all.
	armNever
)

// benchStreamIdleFrame measures one SSE frame's worth of writes through
// streamIdleWriter over a real TCP connection, so SetWriteDeadline reaches the
// runtime poller rather than a no-op test double. A frame is three writes —
// the `event:` line, the `data:` line and the terminating blank line — which is
// what format.WriteTypedEvent issues for a typed stream.
//
// Cost unit: per SSE frame, i.e. roughly per token on a token-per-chunk
// stream. The arm count is forced rather than left to the clock because a
// benchmark's tight loop runs thousands of frames inside one coalescing
// window, which would collapse far more resets than real token pacing does and
// so overstate the saving.
func benchStreamIdleFrame(b *testing.B, mode armMode) {
	b.Helper()
	eventLine := []byte("event: content_block_delta\n")
	dataLine := []byte(`data: {"type":"content_block_delta","delta":{"text":"hello"}}` + "\n")
	blank := []byte("\n")

	idle := 30 * time.Second
	if mode == armNever {
		idle = 0
	}

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(done)
		sw := &streamIdleWriter{
			ResponseWriter: w,
			rc:             http.NewResponseController(w),
			idle:           idle,
		}
		write := func(p []byte) {
			if mode == armEveryWrite {
				sw.rearmAt = time.Time{}
			}
			_, _ = sw.Write(p)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if mode == armPerFrame {
				sw.rearmAt = time.Time{}
			}
			write(eventLine)
			write(dataLine)
			write(blank)
		}
		b.StopTimer()
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		b.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	<-done
}

func BenchmarkStreamIdleWriterFrameArmEveryWrite(b *testing.B) {
	benchStreamIdleFrame(b, armEveryWrite)
}
func BenchmarkStreamIdleWriterFrameArmPerFrame(b *testing.B) { benchStreamIdleFrame(b, armPerFrame) }
func BenchmarkStreamIdleWriterFrameNoDeadline(b *testing.B)  { benchStreamIdleFrame(b, armNever) }
