package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestCORSPreflight(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := CORS(CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
		MaxAge:         3600,
	})(inner)

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("ACAO = %q", got)
	}
	if w.Header().Get("Access-Control-Max-Age") != "3600" {
		t.Errorf("max-age = %q", w.Header().Get("Access-Control-Max-Age"))
	}
}

func TestCORSRejectsUnknownOrigin(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := CORS(CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
	})(inner)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("should not set ACAO for unknown origin")
	}
}

func TestCORSWildcard(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := CORS(CORSConfig{
		AllowedOrigins: []string{"*"},
	})(inner)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://anything.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://anything.example.com" {
		t.Errorf("ACAO = %q, want origin echoed with wildcard", got)
	}
}

func TestCORSNoOriginHeader(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	h := CORS(CORSConfig{AllowedOrigins: []string{"*"}})(inner)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !called {
		t.Error("inner handler should be called when no Origin header")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("no ACAO header expected when no Origin")
	}
}

// TestCORS_ExposeMarkerHeaders verifies that all shared/traffic marker headers
// are advertised in Access-Control-Expose-Headers so browser JS can read them.
func TestCORS_ExposeMarkerHeaders(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := CORS(CORSConfig{
		AllowedOrigins: []string{"http://localhost"},
		ExposeHeaders:  traffic.ExposeHeaders,
	})(inner)

	// Check that the expose list is present on a preflight OPTIONS request.
	t.Run("preflight", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
		req.Header.Set("Origin", "http://localhost")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		got := w.Header().Get("Access-Control-Expose-Headers")
		for _, want := range []string{"x-nexus-via", "x-nexus-cache", "x-nexus-hook"} {
			if !strings.Contains(strings.ToLower(got), want) {
				t.Errorf("Expose-Headers missing %q; got %q", want, got)
			}
		}
	})

	// Check that the expose list is also present on a regular CORS GET request.
	t.Run("actual_request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Origin", "http://localhost")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		got := w.Header().Get("Access-Control-Expose-Headers")
		for _, want := range []string{"x-nexus-via", "x-nexus-cache", "x-nexus-hook"} {
			if !strings.Contains(strings.ToLower(got), want) {
				t.Errorf("Expose-Headers missing %q; got %q", want, got)
			}
		}
	})

	// Verify the full set — the slice reference covers all 30 markers, not a hand list.
	t.Run("full_set_via_slice", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Origin", "http://localhost")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		got := strings.ToLower(w.Header().Get("Access-Control-Expose-Headers"))
		for _, h := range traffic.ExposeHeaders {
			if !strings.Contains(got, strings.ToLower(h)) {
				t.Errorf("Expose-Headers missing marker %q; got %q", h, got)
			}
		}
	})
}
