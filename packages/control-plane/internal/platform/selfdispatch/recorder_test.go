package selfdispatch

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
)

// TestRecorder_WriteImplicitOK pins the ResponseWriter contract: a Write before
// any WriteHeader implies status 200 (so a handler that writes a body without an
// explicit code is recorded as OK), and a second Write does not overwrite the
// already-set code.
func TestRecorder_WriteImplicitOK(t *testing.T) {
	r := &recorder{}
	if _, err := r.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if r.code != http.StatusOK {
		t.Fatalf("code after implicit-OK Write = %d; want 200", r.code)
	}
	if _, err := r.Write([]byte(" more")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := r.buf.String(); got != "hi more" {
		t.Errorf("buffered body = %q; want %q", got, "hi more")
	}
}

// TestRecorder_WriteHeaderStickyFirst pins that the FIRST status wins: a handler
// that sets 404 then (via middleware) tries 200 keeps 404, mirroring net/http's
// single-WriteHeader contract.
func TestRecorder_WriteHeaderStickyFirst(t *testing.T) {
	r := &recorder{}
	r.WriteHeader(http.StatusNotFound)
	r.WriteHeader(http.StatusOK) // ignored — header already written
	if r.code != http.StatusNotFound {
		t.Errorf("code = %d; want 404 (first WriteHeader wins)", r.code)
	}
}

// TestRoundTrip_ImplicitOKWhenHandlerWritesNothing covers the RoundTrip default:
// an in-process handler that neither writes a body nor sets a status leaves the
// recorder code at 0, and RoundTrip must surface 200 (not 0).
func TestRoundTrip_ImplicitOKWhenHandlerWritesNothing(t *testing.T) {
	e := echo.New()
	e.GET("/api/admin/silent", func(c echo.Context) error { return nil })
	tr := New(Config{Handler: e, CPBaseURL: "http://localhost:9999"})

	resp, _ := doRoundTrip(t, tr, http.MethodGet, "http://localhost:9999/api/admin/silent", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200 (a silent handler defaults to OK)", resp.StatusCode)
	}
}

// TestRoundTrip_FlushIsNoOp drives a handler that flushes mid-response so the
// recorder's Flusher path is exercised through the real dispatch (echo's Response
// probes for http.Flusher; the buffered recorder's Flush must be a safe no-op).
func TestRoundTrip_FlushIsNoOp(t *testing.T) {
	e := echo.New()
	e.GET("/api/admin/flush", func(c echo.Context) error {
		c.Response().WriteHeader(http.StatusOK)
		_, _ = c.Response().Write([]byte("chunk"))
		c.Response().Flush()
		return nil
	})
	tr := New(Config{Handler: e, CPBaseURL: "http://localhost:9999"})

	resp, _ := doRoundTrip(t, tr, http.MethodGet, "http://localhost:9999/api/admin/flush", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
}
