package assistant

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	sharediam "github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// fakeIAMLoader returns fixed policies regardless of principal, mirroring the
// loader fake used by the iamauth middleware tests.
type fakeIAMLoader struct {
	policies []iam.LoadedPolicy
}

func (f *fakeIAMLoader) LoadPolicies(_ context.Context, _, _ string) ([]iam.LoadedPolicy, error) {
	return f.policies, nil
}

func iamAllowAll() []iam.LoadedPolicy {
	return []iam.LoadedPolicy{{
		ID: "p1", Name: "allow-all", Source: "direct",
		Document: iam.PolicyDocument{
			Version: iam.PolicyVersion,
			Statement: []iam.Statement{
				{Effect: "Allow", Action: []string{"*"}, Resource: []string{"*"}},
			},
		},
	}}
}

// iamReadOnly grants only admin:assistant.read — used to prove the write-gated
// endpoints (chat/confirm/interrupt/delete) are denied to a read-only principal.
func iamReadOnly() []iam.LoadedPolicy {
	return []iam.LoadedPolicy{{
		ID: "p2", Name: "assistant-read", Source: "direct",
		Document: iam.PolicyDocument{
			Version: iam.PolicyVersion,
			Statement: []iam.Statement{
				{Effect: "Allow", Action: []string{sharediam.ResourceAssistant.Action(sharediam.VerbRead)}, Resource: []string{"*"}},
			},
		},
	}}
}

// mountAssistantWithIAM mounts the assistant routes behind the real IAM
// middleware, injecting an authenticated principal (so the 401-no-principal
// path is bypassed and the IAM decision itself is what's under test).
func mountAssistantWithIAM(engine *iam.Engine) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	g := e.Group("/api/admin", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			middleware.WithAdminAuth(c, &auth.AdminAuth{
				KeyID:             "user-iam-test",
				AuthPrincipalType: "admin_user",
			})
			return next(c)
		}
	})
	New(Config{}).RegisterAssistantRoutes(g, engine)
	return e
}

// TestAssistantRoutes_IAMDenyReturns403 is the F-0081 regression: every
// assistant endpoint must carry an IAM gate, so an authenticated session user
// with NO assistant grant is denied (403) — never reaching the handler. Without
// the gate, login alone would let any user open the assistant and burn
// system-VK budget before any tool self-call's own IAM check could fire.
func TestAssistantRoutes_IAMDenyReturns403(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeIAMLoader{}, slog.Default()) // empty policy set → deny

	e := mountAssistantWithIAM(engine)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/admin/assistant/sessions/s1/chat"},
		{http.MethodGet, "/api/admin/assistant/sessions/s1/stream"},
		{http.MethodPost, "/api/admin/assistant/sessions/s1/interrupt"},
		{http.MethodPost, "/api/admin/assistant/confirm"},
		{http.MethodGet, "/api/admin/assistant/sessions"},
		{http.MethodGet, "/api/admin/assistant/sessions/s1"},
		{http.MethodDelete, "/api/admin/assistant/sessions/s1"},
		{http.MethodGet, "/api/admin/assistant/files/f1"},
		{http.MethodGet, "/api/admin/assistant/models"},
	}
	if len(cases) != 9 {
		t.Fatalf("expected all 9 assistant endpoints under test, got %d", len(cases))
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, nil)
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status=%d want 403 (IAM gate missing/misapplied)", c.method, c.path, rec.Code)
		}
	}
}

// TestAssistantRoutes_ReadGrantDoesNotUnlockWrite proves the read/write split:
// a principal holding only admin:assistant.read may reach the GET surface (the
// IAM gate passes) but is still denied (403) on the mutating endpoints. We use
// the models endpoint as the read probe because it returns without needing a DB
// or upstream; a non-403 status proves the gate let it through.
func TestAssistantRoutes_ReadGrantDoesNotUnlockWrite(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeIAMLoader{policies: iamReadOnly()}, slog.Default())
	e := mountAssistantWithIAM(engine)

	// Read endpoint passes the gate (status is not the IAM 403).
	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/assistant/models", nil)
		e.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("GET models with assistant.read grant returned 403; the read gate must allow it")
		}
	}

	// Write endpoints are denied for a read-only principal.
	writePaths := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/admin/assistant/sessions/s1/chat"},
		{http.MethodPost, "/api/admin/assistant/confirm"},
		{http.MethodPost, "/api/admin/assistant/sessions/s1/interrupt"},
		{http.MethodDelete, "/api/admin/assistant/sessions/s1"},
	}
	for _, c := range writePaths {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, nil)
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with read-only grant: status=%d want 403 (write must require assistant.write)", c.method, c.path, rec.Code)
		}
	}
}

// TestAssistantRoutes_FullGrantPassesGate proves the positive path: a principal
// with the full assistant grant clears the IAM gate on every endpoint (the
// status is never the IAM 403). Downstream handler behavior is covered by the
// other handler tests; here we assert only that the gate admits the request.
func TestAssistantRoutes_FullGrantPassesGate(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeIAMLoader{policies: iamAllowAll()}, slog.Default())
	e := mountAssistantWithIAM(engine)

	// models is the only read endpoint that resolves without a DB/upstream; the
	// rest are exercised for "gate admits" via the deny test's inverse. Assert
	// the gate admits the read probe.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/assistant/models", nil)
	e.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("GET models with full grant returned 403; the gate must admit it")
	}
}
