package traffic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTracingTransport_TtfbOnFirstBodyByte_Streaming verifies TTFB anchors on
// the first body byte the caller reads (≈ first SSE chunk) and is NOT moved by
// later frames.
func TestTracingTransport_TtfbOnFirstBodyByte_Streaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		if fl != nil {
			fl.Flush() // headers out immediately; body byte comes later
		}
		time.Sleep(25 * time.Millisecond)
		_, _ = w.Write([]byte("frame1"))
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(25 * time.Millisecond)
		_, _ = w.Write([]byte("frame2"))
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewTracingTransport(http.DefaultTransport)}
	ps := NewPhaseSink()
	req, _ := http.NewRequestWithContext(WithPhaseSink(context.Background(), ps), http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 6)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read frame1: %v", err)
	}
	ttfb1 := ps.TtfbMs()
	if ttfb1 == nil {
		t.Fatal("TtfbMs must be populated after the first body byte")
	}
	// TTFB anchors on the body byte, not the (immediate) header flush.
	if *ttfb1 < 20 {
		t.Errorf("TTFB (%d ms) should reflect the pre-frame delay (~25ms), not the header flush", *ttfb1)
	}

	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read frame2: %v", err)
	}
	if ttfb2 := ps.TtfbMs(); ttfb2 == nil || *ttfb2 != *ttfb1 {
		t.Errorf("TTFB must not move on later frames: was %v, now %v", ttfb1, ttfb2)
	}
}

// TestTracingTransport_TtfbOnFirstBodyByte_NonStreaming verifies a single-write
// body populates both TTFB and total, with total >= ttfb.
func TestTracingTransport_TtfbOnFirstBodyByte_NonStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Millisecond)
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewTracingTransport(http.DefaultTransport)}
	ps := NewPhaseSink()
	req, _ := http.NewRequestWithContext(WithPhaseSink(context.Background(), ps), http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	ttfb, total := ps.TtfbMs(), ps.TotalMs()
	if ttfb == nil || *ttfb <= 0 {
		t.Fatalf("TtfbMs must populate on a non-streaming body; got %v", ttfb)
	}
	if total == nil || *total < *ttfb {
		t.Fatalf("TotalMs (%v) must be >= TtfbMs (%v)", total, ttfb)
	}
}

// TestTracingTransport_EmptyBody_TtfbNil verifies an empty body leaves TTFB nil
// (no first content byte was observed) while total still populates.
func TestTracingTransport_EmptyBody_TtfbNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // 204, no body
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewTracingTransport(http.DefaultTransport)}
	ps := NewPhaseSink()
	req, _ := http.NewRequestWithContext(WithPhaseSink(context.Background(), ps), http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if ttfb := ps.TtfbMs(); ttfb != nil {
		t.Errorf("empty body must leave TTFB nil (no content byte); got %d", *ttfb)
	}
	if total := ps.TotalMs(); total == nil {
		t.Error("TotalMs must still populate for an empty body (stamped at Close)")
	}
}

// benchBase is an in-memory RoundTripper returning a fixed body, so the
// benchmark isolates the tracing wrapper's per-RoundTrip cost.
type benchBase struct{ body string }

func (b benchBase) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(b.body)),
		Request:    req,
	}, nil
}

// BenchmarkTracingRoundTrip_WithSink measures the per-RoundTrip allocation of
// the tracing wrapper (RoundTrip + drain + close). After folding TTFB into the
// body wrapper, RoundTrip allocates only &phaseTrackedBody{} — the prior
// httptrace.ClientTrace + request-context copy + two closures are gone.
func BenchmarkTracingRoundTrip_WithSink(b *testing.B) {
	tr := NewTracingTransport(benchBase{body: "the quick brown fox jumps over the lazy dog"})
	ctx := WithPhaseSink(context.Background(), NewPhaseSink())
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://x", nil)
		resp, _ := tr.RoundTrip(req)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}
