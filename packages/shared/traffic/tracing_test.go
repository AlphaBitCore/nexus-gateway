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

func TestTracingTransport_CapturesTtfbAndTotal(t *testing.T) {
	// Fake server that delays the response body slightly so TTFB < TotalMs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	}))
	defer srv.Close()

	tr := NewTracingTransport(http.DefaultTransport)
	client := &http.Client{Transport: tr}

	ps := NewPhaseSink()
	ctx := WithPhaseSink(context.Background(), ps)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	ttfb := ps.TtfbMs()
	total := ps.TotalMs()
	if ttfb == nil {
		t.Fatalf("TtfbMs should be populated after a successful response")
	}
	if total == nil {
		t.Fatalf("TotalMs should be populated after body close")
	}
	if *total < *ttfb {
		t.Errorf("TotalMs (%d) should be >= TtfbMs (%d)", *total, *ttfb)
	}
}

func TestTracingTransport_NoSinkOnContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tr := NewTracingTransport(http.DefaultTransport)
	client := &http.Client{Transport: tr}

	// No sink on context — must not panic, must succeed.
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "ok") {
		t.Errorf("unexpected body: %q", body)
	}
}

func TestPhaseSink_NilSafe(t *testing.T) {
	var ps *PhaseSink
	if v := ps.TtfbMs(); v != nil {
		t.Errorf("nil sink TtfbMs must be nil, got %v", *v)
	}
	if v := ps.TotalMs(); v != nil {
		t.Errorf("nil sink TotalMs must be nil")
	}
}

func TestPhaseSinkFromContext_NilContext(t *testing.T) {
	//nolint:staticcheck // SA1012: intentionally tests the nil-Context defensive path
	if v := PhaseSinkFromContext(nil); v != nil {
		t.Errorf("nil context must return nil sink")
	}
	if v := PhaseSinkFromContext(context.Background()); v != nil {
		t.Errorf("context without sink must return nil")
	}
}
