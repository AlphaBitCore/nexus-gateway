package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelContextWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"provider":{"id":"p","name":"X"},"models":[{"code":"m1","name":"M1","maxContextTokens":200000}]}]}`)
	}))
	defer srv.Close()
	a := newTestApp(srv, false)

	if got := a.modelContextWindow("m1"); got != 200000 {
		t.Fatalf("resolved window = %d, want 200000", got)
	}
	if got := a.modelContextWindow("no-such-model"); got != 0 {
		t.Fatalf("unknown model must be 0, got %d", got)
	}
	if got := a.modelContextWindow(""); got != 0 {
		t.Fatalf("empty code must be 0, got %d", got)
	}

	// Catalog unavailable → 0 (best-effort; the indicator shows "window unknown").
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	if got := newTestApp(bad, false).modelContextWindow("m1"); got != 0 {
		t.Fatalf("a failed catalog fetch must yield 0, got %d", got)
	}
}
