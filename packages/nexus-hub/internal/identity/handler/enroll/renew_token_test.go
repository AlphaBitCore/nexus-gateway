package enroll

// renew_token_test.go covers POST /api/internal/things/renew-token — the
// device-token rotation handler (F-0202).
//
// Named failure modes / behaviours tested:
//   - no_thing_in_context (service token) → 401 UNAUTHORIZED
//   - token_generation_error             → 500 INTERNAL_ERROR
//   - store_error                        → 500 INTERNAL_ERROR
//   - happy_rotation                     → 200, fresh token + future expiry,
//                                          old hash overwritten with new expiry

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// renewTokenCtx builds an echo context for the renew-token handler with the
// given Thing already attached (as DeviceOrServiceAuth would have done). A nil
// thing models the internal-service-token path that has no device identity.
func renewTokenCtx(thing *store.Thing) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodPost, "/api/internal/things/renew-token", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	if thing != nil {
		c.Set("thing", thing)
	}
	return c, rec
}

func TestRenewToken_NoThing_Rejected(t *testing.T) {
	// Reached via the internal service token: no Thing in context → 401.
	api := buildAPI(&stubCA{}, &stubFleetManager{}, nil)
	c, rec := renewTokenCtx(nil)
	if err := api.RenewToken(c); err != nil {
		t.Fatalf("RenewToken returned err: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "UNAUTHORIZED" {
		t.Errorf("want UNAUTHORIZED, got %q", body.Code)
	}
}

func TestRenewToken_GenError_Returns500(t *testing.T) {
	orig := deviceTokenGenFn
	deviceTokenGenFn = func() (string, string, error) { return "", "", errors.New("entropy fail") }
	defer func() { deviceTokenGenFn = orig }()

	api := buildAPI(&stubCA{}, &stubFleetManager{}, nil)
	c, rec := renewTokenCtx(&store.Thing{ID: "agent-1"})
	_ = api.RenewToken(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "INTERNAL_ERROR" {
		t.Errorf("want INTERNAL_ERROR, got %q", body.Code)
	}
}

func TestRenewToken_StoreError_Returns500(t *testing.T) {
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mock.ExpectExec(`UPDATE thing.*deviceTokenHash.*device_token_expires_at`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("db down"))

	api := buildAPI(&stubCA{}, &stubFleetManager{st: st}, nil)
	c, rec := renewTokenCtx(&store.Thing{ID: "agent-1"})
	_ = api.RenewToken(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestRenewToken_HappyRotation(t *testing.T) {
	// Pin the clock so the stamped expiry is deterministic.
	fixedNow := time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC)
	origTime := timeNow
	timeNow = func() time.Time { return fixedNow }
	defer func() { timeNow = origTime }()

	wantExpiry := agentca.DeviceTokenExpiry(fixedNow)

	st, mock := newPgxmockStore(t)
	defer mock.Close()
	// The rotation overwrites the stored hash AND re-stamps the expiry: the old
	// token is invalidated and the new one is bounded (F-0202). Asserting the
	// expiry arg ($3) equals NOW()+TTL proves the rotation re-stamps expiry.
	mock.ExpectExec(`UPDATE thing.*deviceTokenHash.*device_token_expires_at = \$3`).
		WithArgs("agent-1", pgxmock.AnyArg(), wantExpiry).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	api := buildAPI(&stubCA{}, &stubFleetManager{st: st}, nil)
	c, rec := renewTokenCtx(&store.Thing{ID: "agent-1"})
	if err := api.RenewToken(c); err != nil {
		t.Fatalf("RenewToken: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	decodeBody(t, rec, &body)

	tok, _ := body["deviceToken"].(string)
	if len(tok) != 2*agentca.DeviceTokenLen {
		t.Errorf("rotated token must be a fresh %d-hex token, got len %d", 2*agentca.DeviceTokenLen, len(tok))
	}
	gotExpStr, _ := body["deviceTokenExpiresAt"].(string)
	gotExp, perr := time.Parse(time.RFC3339, gotExpStr)
	if perr != nil {
		t.Fatalf("deviceTokenExpiresAt not RFC3339: %q", gotExpStr)
	}
	if !gotExp.Equal(wantExpiry) {
		t.Errorf("expiry = %v, want NOW()+TTL = %v", gotExp, wantExpiry)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("rotation did not re-stamp expiry as expected: %v", err)
	}
}
