package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// okHandler is a simple handler that returns 200 OK.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
})

func TestTokenAuth_NoAuthHeader(t *testing.T) {
	a := NewTokenAuth("secret")
	handler := a.Require(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestTokenAuth_ValidToken(t *testing.T) {
	a := NewTokenAuth("secret")
	handler := a.Require(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestTokenAuth_InvalidToken(t *testing.T) {
	a := NewTokenAuth("secret")
	handler := a.Require(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

// TestTokenAuth_EmptyToken_FailClosed verifies the invariant:
// an empty token must never open the runtime API surface — it must return
// 503 so the mutating break-glass verb is unreachable without a credential.
func TestTokenAuth_EmptyToken_FailClosed(t *testing.T) {
	a := NewTokenAuth("") // boot validation prevents this in prod; defence-in-depth
	handler := a.Require(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (fail-closed on empty token), got %d", rec.Code)
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
