package auth

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// okHandler is a simple handler that returns 200 OK.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
})

func setTokenEnv(t *testing.T, tok string) {
	t.Helper()
	t.Setenv("COMPLIANCE_PROXY_API_TOKEN", tok)
}

func TestTokenAuth_NoToken(t *testing.T) {
	setTokenEnv(t, "secret")
	auth := NewTokenAuth(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	handler := auth.Require(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestTokenAuth_ValidToken(t *testing.T) {
	setTokenEnv(t, "secret")
	auth := NewTokenAuth(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	handler := auth.Require(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestTokenAuth_InvalidToken(t *testing.T) {
	setTokenEnv(t, "secret")
	auth := NewTokenAuth(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	handler := auth.Require(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestTokenAuth_Disabled(t *testing.T) {
	setTokenEnv(t, "")
	auth := NewTokenAuth(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if !auth.disabled {
		t.Fatal("expected auth to be disabled when env var is empty")
	}

	handler := auth.Require(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (auth disabled), got %d", rec.Code)
	}
}

// TestExtractBearer_MalformedHeaderShapes covers extractBearer's
// remaining branches: header present but too short to carry the
// "Bearer " prefix, and header with non-Bearer scheme. Both return
// empty so the middleware uniformly rejects with 401.
func TestExtractBearer_MalformedHeaderShapes(t *testing.T) {
	cases := []struct {
		name string
		auth string
		want string
	}{
		{"absent", "", ""},
		{"too short", "Be", ""},
		{"non-bearer scheme", "Basic abcdef", ""},
		{"bearer mixed case still accepted (EqualFold)", "bearer mytoken", "mytoken"},
		{"valid bearer", "Bearer mytoken", "mytoken"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			if got := extractBearer(req); got != tc.want {
				t.Errorf("extractBearer(%q) = %q, want %q", tc.auth, got, tc.want)
			}
		})
	}
}
