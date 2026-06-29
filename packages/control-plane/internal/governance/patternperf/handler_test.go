package patternperf

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestHandler_ForwardsAndRelays(t *testing.T) {
	var gotAuth, gotBody string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"compiles":true,"verdict":"ok","adversarialScanUs":42}`))
	}))
	defer gw.Close()

	h := New(gw.URL, "tok-123", nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/rule-packs/pattern-perf-test",
		strings.NewReader(`{"pattern":"\\bAKIA[0-9A-Z]{16}\\b","flags":"i"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := h.Test(e.NewContext(req, rec)); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("relayed body not JSON: %v", err)
	}
	if out["verdict"] != "ok" {
		t.Errorf("relayed verdict=%v, want ok", out["verdict"])
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("forwarded auth=%q, want Bearer tok-123", gotAuth)
	}
	if !strings.Contains(gotBody, "AKIA") || !strings.Contains(gotBody, `"flags":"i"`) {
		t.Errorf("forwarded body did not carry pattern+flags: %q", gotBody)
	}
}

func TestHandler_RejectsEmptyPattern(t *testing.T) {
	h := New("http://127.0.0.1:1", "tok", nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"pattern":"   "}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := h.Test(e.NewContext(req, rec)); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty pattern status=%d, want 400", rec.Code)
	}
}

func TestHandler_RejectsMalformedBody(t *testing.T) {
	h := New("http://127.0.0.1:1", "tok", nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{not valid json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := h.Test(e.NewContext(req, rec)); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body status=%d, want 400", rec.Code)
	}
}

func TestHandler_BadGatewayURLIsGraceful(t *testing.T) {
	// A gateway URL with a control character fails http.NewRequest → the handler
	// returns a graceful 200 + success:false rather than panicking.
	h := New("http://exa\x7fmple", "tok", nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"pattern":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := h.Test(e.NewContext(req, rec)); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("bad URL status=%d, want 200", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["success"] != false {
		t.Errorf("expected success:false on build-request failure, got %v", out)
	}
}

func TestHandler_GatewayUnreachableIsGraceful(t *testing.T) {
	h := New("http://127.0.0.1:1", "tok", nil) // nothing listening
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"pattern":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := h.Test(e.NewContext(req, rec)); err != nil {
		t.Fatalf("Test: %v", err)
	}
	// Transport failure is a graceful 200 + success:false so the editor can show
	// a readable message, not an opaque 5xx.
	if rec.Code != http.StatusOK {
		t.Errorf("unreachable status=%d, want 200", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["success"] != false {
		t.Errorf("expected success:false on unreachable gateway, got %v", out)
	}
}
