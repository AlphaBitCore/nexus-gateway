package siem

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// echoWithAuth builds an Echo context with AdminAuth injected, mirroring the
// technique in killswitch/handler/killswitch_test.go.
func echoWithAuth(method, path string, body []byte, keyID, keyName string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             keyID,
		KeyName:           keyName,
		AuthPrincipalType: "api_key",
	})
	return c, rec
}

// TestActorFromContext_WithAuth proves the non-nil AdminAuth branch of
// actorFromContext: when AdminAuth is set in the context the function must
// return an Actor populated with KeyID and KeyName.
func TestActorFromContext_WithAuth(t *testing.T) {
	c, _ := echoWithAuth(http.MethodGet, "/", nil, "key-99", "alice")
	a := actorFromContext(c)
	if a.UserID != "key-99" {
		t.Errorf("UserID = %q; want key-99", a.UserID)
	}
	if a.Name != "alice" {
		t.Errorf("Name = %q; want alice", a.Name)
	}
}

// TestRedactSecretHeaders_Nil proves the nil-guard: redactSecretHeaders(nil)
// must return nil — not an empty map — so callers can distinguish "no headers
// configured" from "headers configured but all redacted".
func TestRedactSecretHeaders_Nil(t *testing.T) {
	if got := redactSecretHeaders(nil); got != nil {
		t.Errorf("redactSecretHeaders(nil) = %v; want nil", got)
	}
}

// TestPreserveSecretHeaders_Nil proves the nil-guard: preserveSecretHeaders
// with a nil inbound map returns nil immediately (no panic, no mutation).
func TestPreserveSecretHeaders_Nil(t *testing.T) {
	stored := map[string]string{"Authorization": "Bearer real-token"}
	if got := preserveSecretHeaders(nil, stored); got != nil {
		t.Errorf("preserveSecretHeaders(nil, stored) = %v; want nil", got)
	}
}

// TestPreserveSecretHeaders_SentinelWithNoStoredValue covers the
// `delete(inbound, k)` branch: when inbound carries the redacted sentinel for
// a secret header but stored has NO prior value for that key, the placeholder
// must be deleted rather than persisted as a literal credential value.
func TestPreserveSecretHeaders_SentinelWithNoStoredValue(t *testing.T) {
	// Inbound replays the sentinel, but no prior token exists in stored.
	inbound := map[string]string{
		"Authorization": redactedSecretSentinel, // sentinel with no stored counterpart
		"x-custom":      "plain",                // non-secret passes through unchanged
	}
	stored := map[string]string{} // empty — no prior Authorization value

	result := preserveSecretHeaders(inbound, stored)

	// The sentinel must be deleted (no stored value to preserve).
	if _, ok := result["Authorization"]; ok {
		t.Errorf("Authorization = %q; want key deleted (no stored value to preserve)", result["Authorization"])
	}
	// Non-secret header must pass through unchanged.
	if result["x-custom"] != "plain" {
		t.Errorf("x-custom = %q; want plain", result["x-custom"])
	}
}

// TestUpdateSIEMConfig_WithActorID covers the `updatedBy = aa.KeyID` branch
// inside UpdateSIEMConfig: when an authenticated AdminAuth is in the context,
// the updatedBy field fed to SetSystemMetadata must be the KeyID, not "unknown".
// We verify this by checking the DB call succeeds (the query arg is AnyArg so
// the test just confirms the path executes without error).
func TestUpdateSIEMConfig_WithActorID(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "key-admin-42").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	body, _ := json.Marshal(SIEMConfig{Format: "json", URL: "https://example.com/siem"})
	c, rec := echoWithAuth(http.MethodPut, "/api/admin/settings/siem", body, "key-admin-42", "admin-user")

	if err := h.UpdateSIEMConfig(c); err != nil {
		t.Fatalf("UpdateSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body = %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
