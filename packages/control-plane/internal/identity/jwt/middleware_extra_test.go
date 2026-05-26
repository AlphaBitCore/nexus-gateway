package jwtverifier_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
)

// erroringRevoker returns a sentinel that is NOT in descForError's allow-list,
// so the middleware must emit the bare invalid_token challenge (no
// error_description). Used to pin the "desc == \"\"" branch.
type erroringRevoker struct{}

func (erroringRevoker) IsRevoked(_ context.Context, _ *jwtverifier.Claims) (bool, error) {
	return false, errors.New("unclassified upstream blip")
}

// TestMiddleware_UnclassifiedError_BareInvalidTokenChallenge pins the branch
// where descForError returns "" because the error is not a known sentinel.
// The middleware must emit the bare `Bearer error="invalid_token"` challenge
// without an error_description — a leaking arbitrary err.Error() would let an
// upstream component inject characters into the response header.
func TestMiddleware_UnclassifiedError_BareInvalidTokenChallenge(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t, erroringRevoker{})
	e := newEchoWithMiddleware(f.verifier)
	raw := f.sign(t, validClaims())

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := `Bearer error="invalid_token"`
	if got != want {
		t.Errorf("WWW-Authenticate = %q, want %q (unclassified error must NOT emit error_description)", got, want)
	}
}
