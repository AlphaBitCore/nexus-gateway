package iam

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
)

// TestRegisterInternalAuthRoutes_ExposesReplayWithoutIAM pins the ROUTE, which
// is the thing that was wrong. The revocation checker holds the internal service
// token and no IAM identity, so the replay endpoint has to exist on the
// rstokenauth-gated internal group; it previously existed only on the admin
// group behind admin:revocation.read, and the checker took a 401 on every poll.
//
// The assertion is on the router's own table rather than on a handler call,
// because a handler that works when invoked directly says nothing about whether
// anything routes to it.
func TestRegisterInternalAuthRoutes_ExposesReplayWithoutIAM(t *testing.T) {
	e := echo.New()
	g := e.Group("/api/internal")
	// No middleware is attached on purpose: in production the group carries
	// rstokenauth and NOT iamMW, and this method must not add auth of its own.
	defaultHandler().RegisterInternalAuthRoutes(g)

	want := map[string]string{
		"/api/internal/revocations":        http.MethodGet,
		"/api/internal/auth/revoke-device": http.MethodPost,
	}
	got := map[string]string{}
	for _, r := range e.Routes() {
		if _, ok := want[r.Path]; ok {
			got[r.Path] = r.Method
		}
	}
	for path, method := range want {
		if got[path] != method {
			t.Errorf("internal group must expose %s %s; router has %q", method, path, got[path])
		}
	}
}

// TestListRevocations_NeedsNoAdminIdentity is the safety half: serving the
// replay route without an admin identity is only sound because the handler never
// reads one. If a future edit starts deriving an actor from the admin context,
// this fails here rather than 500-ing in production on every catchup poll.
func TestListRevocations_NeedsNoAdminIdentity(t *testing.T) {
	h := defaultHandler()
	// No revocation store wired → 503 is the documented answer. What matters is
	// that it is 503 and not a panic or a 401: the handler got far enough to
	// check its dependency without ever asking who the caller is.
	// Empty userID => adminAuthCtx skips WithAdminAuth, so the context carries no
	// admin principal at all — exactly what the internal group hands the handler.
	c, rec := adminAuthCtx(http.MethodGet, "/api/internal/revocations?since=0&limit=5", nil, "", "")
	if err := h.ListRevocations(c); err != nil {
		t.Fatalf("ListRevocations must not error out without an admin identity: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (store unwired) — anything auth-shaped here means the handler reads an admin identity", rec.Code)
	}
}
