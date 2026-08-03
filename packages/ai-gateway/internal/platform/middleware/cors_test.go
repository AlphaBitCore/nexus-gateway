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

// TestCORS_VaryOriginOnEveryCORSResponse pins the cache-poisoning guard:
// any response to a request carrying an Origin varies by that Origin —
// including the disallowed-origin response, whose distinguishing feature
// is the ABSENCE of CORS headers. Without Vary on that path a shared
// cache may store the bare copy and serve it to an allowed origin (or
// the allowed copy to a disallowed one).
func TestCORS_VaryOriginOnEveryCORSResponse(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := CORS(CORSConfig{AllowedOrigins: []string{"https://app.example.com"}})(inner)

	for _, tc := range []struct {
		name   string
		origin string
	}{
		{"allowed origin", "https://app.example.com"},
		{"disallowed origin", "https://evil.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if got := w.Header().Values("Vary"); len(got) == 0 || !strings.Contains(strings.Join(got, ","), "Origin") {
				t.Errorf("Vary = %v, want Origin present", got)
			}
		})
	}

	// No Origin → not a CORS response → no Vary from this middleware.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("Vary"); got != "" {
		t.Errorf("Vary = %q on a non-CORS request, want empty", got)
	}
}

// TestCORS_PreflightDisallowedOriginRevealsNothing pins the preflight
// gate: a disallowed origin gets a bare 204 — no Allow-Methods /
// Allow-Headers / Max-Age readout of what the gateway would accept.
func TestCORS_PreflightDisallowedOriginRevealsNothing(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("inner handler must not run on preflight")
	})
	h := CORS(CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders: []string{"Content-Type"},
	})(inner)

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	for _, hdr := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
		"Access-Control-Max-Age",
	} {
		if got := w.Header().Get(hdr); got != "" {
			t.Errorf("%s = %q for a disallowed origin, want unset", hdr, got)
		}
	}
}

// TestUnionHeaderNames pins the composition contract the CORS request
// allowlist is built on: case-insensitive dedupe with first-seen spelling
// winning, blanks dropped, deterministic (sorted) output.
func TestUnionHeaderNames(t *testing.T) {
	got := UnionHeaderNames(
		[]string{"Content-Type", "X-Nexus-Virtual-Key", ""},
		[]string{"content-type", "anthropic-beta"},
		[]string{" X-Custom-Tag ", "ANTHROPIC-BETA"},
	)
	want := []string{"Content-Type", "X-Custom-Tag", "X-Nexus-Virtual-Key", "anthropic-beta"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (first-seen spelling, sorted, deduped)", got, want)
		}
	}
}
