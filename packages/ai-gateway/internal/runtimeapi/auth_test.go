package runtimeapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuth_RejectsMissingBearer(t *testing.T) {
	a := newAuth("api-t")
	h := a.require(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/runtime/config", nil)
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAuth_AcceptsAPIToken(t *testing.T) {
	a := newAuth("api-t")
	h := a.require(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/runtime/config", nil)
	req.Header.Set("Authorization", "Bearer api-t")
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

func TestAuth_RejectsWrongToken(t *testing.T) {
	a := newAuth("api-t")
	h := a.require(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/runtime/config", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

// TestAuth_UnconfiguredTokenFailsClosed — when the runtime API token
// is unset, every request gets 503 Service Unavailable (fail closed), never a
// 401 that could be misread as a credential failure, and never 200. This
// mirrors compliance-proxy runtime/auth and the other platform token gates.
func TestAuth_UnconfiguredTokenFailsClosed(t *testing.T) {
	a := newAuth("") // token never provisioned
	h := a.require(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name   string
		bearer string
	}{
		{"no bearer", ""},
		{"empty bearer", "Bearer "},
		{"any non-empty bearer", "Bearer anything"},
		{"empty-string bearer matching empty token", "Bearer "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/runtime/config", nil)
			if tc.bearer != "" {
				req.Header.Set("Authorization", tc.bearer)
			}
			w := httptest.NewRecorder()
			h(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("want 503 for unconfigured token, got %d", w.Code)
			}
			if body := w.Body.String(); !strings.Contains(body, "not configured") {
				t.Errorf("expected 'not configured' in 503 body, got %q", body)
			}
		})
	}
}
