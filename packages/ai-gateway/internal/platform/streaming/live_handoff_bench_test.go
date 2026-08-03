package streaming

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The live pipeline hands every parsed SSE event from a reader goroutine to
// the delivery loop over a buffered channel. That handoff is the cost of the
// reader goroutine; the queue it creates is also what lets the delivery loop
// drain a burst and issue ONE flush for it. These two benchmarks measure both
// sides so the trade can be decided on numbers rather than on the shape of the
// code.
//
// Cost unit for both: per SSE frame.

// benchChunk mirrors the pipeline's internal chunk struct: three strings
// carried across the goroutine boundary.
type benchChunk struct {
	eventType string
	data      string
	rawData   string
}

// BenchmarkLiveEventHandoff measures one frame crossing the buffered channel
// with a producer goroutine feeding a consumer, at the pipeline's default
// channel size. This is what removing the reader goroutine would save.
func BenchmarkLiveEventHandoff(b *testing.B) {
	data := `{"type":"content_block_delta","delta":{"text":"hello"}}`
	ch := make(chan benchChunk, defaultEventChannelSize)
	done := make(chan struct{})

	go func() {
		defer close(done)
		for range ch {
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ch <- benchChunk{eventType: "content_block_delta", data: data, rawData: data}
	}
	b.StopTimer()
	close(ch)
	<-done
}

// BenchmarkLiveClientFlush measures one http.Flusher flush on a real TCP
// connection — the syscall the drain-then-flush loop coalesces away when
// several frames are already queued. Without a queue there is nothing to
// drain, so the delivery loop would pay this once per frame.
func BenchmarkLiveClientFlush(b *testing.B) {
	line := []byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\n")

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(done)
		flusher, ok := w.(http.Flusher)
		if !ok {
			b.Error("ResponseWriter is not an http.Flusher")
			return
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			_, _ = w.Write(line)
			flusher.Flush()
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

// BenchmarkLiveClientWriteNoFlush is the floor for the benchmark above: the
// same write with no flush, so the difference is the flush itself.
func BenchmarkLiveClientWriteNoFlush(b *testing.B) {
	line := []byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\n")

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(done)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			_, _ = w.Write(line)
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
