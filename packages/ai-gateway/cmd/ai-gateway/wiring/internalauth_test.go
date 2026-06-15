package wiring

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// testInternalToken is the internal-service token wired into the shared
// route handler (buildMinimalRouteDeps) so the /internal/* auth route tests
// can exercise the missing/wrong/correct bearer paths.
const testInternalToken = "test-internal-token"

// TestInternalAuth_require_emptyTokenFailsClosed verifies the fail-closed
// branch: an unconfigured token returns 503 and never reaches the handler,
// regardless of what bearer the caller presents.
func TestInternalAuth_require_emptyTokenFailsClosed(t *testing.T) {
	reached := false
	h := newInternalAuth("").require(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/internal/anything", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty token: expected 503, got %d", rr.Code)
	}
	if reached {
		t.Fatal("empty token must not reach the wrapped handler")
	}
}

// TestInternalAuth_require_missingBearer verifies a request with no
// Authorization header is rejected 401 without reaching the handler.
func TestInternalAuth_require_missingBearer(t *testing.T) {
	reached := false
	h := newInternalAuth(testInternalToken).require(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
	})

	req := httptest.NewRequest(http.MethodPost, "/internal/anything", nil)
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer: expected 401, got %d", rr.Code)
	}
	if reached {
		t.Fatal("missing bearer must not reach the wrapped handler")
	}
}

// TestInternalAuth_require_wrongBearer verifies a non-matching token is
// rejected 401.
func TestInternalAuth_require_wrongBearer(t *testing.T) {
	reached := false
	h := newInternalAuth(testInternalToken).require(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
	})

	req := httptest.NewRequest(http.MethodPost, "/internal/anything", nil)
	req.Header.Set("Authorization", "Bearer not-the-token")
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: expected 401, got %d", rr.Code)
	}
	if reached {
		t.Fatal("wrong bearer must not reach the wrapped handler")
	}
}

// TestInternalAuth_require_malformedHeader verifies an Authorization header
// that is not a Bearer scheme is treated as missing (401).
func TestInternalAuth_require_malformedHeader(t *testing.T) {
	h := newInternalAuth(testInternalToken).require(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/internal/anything", nil)
	req.Header.Set("Authorization", "Basic "+testInternalToken)
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("malformed header: expected 401, got %d", rr.Code)
	}
}

// TestInternalAuth_require_correctBearerReachesHandler verifies the happy
// path: a matching Bearer token reaches the wrapped handler.
func TestInternalAuth_require_correctBearerReachesHandler(t *testing.T) {
	reached := false
	h := newInternalAuth(testInternalToken).require(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusTeapot) // distinct sentinel
	})

	req := httptest.NewRequest(http.MethodPost, "/internal/anything", nil)
	req.Header.Set("Authorization", "Bearer "+testInternalToken)
	rr := httptest.NewRecorder()
	h(rr, req)

	if !reached {
		t.Fatal("correct bearer must reach the wrapped handler")
	}
	if rr.Code != http.StatusTeapot {
		t.Fatalf("correct bearer: expected handler sentinel 418, got %d", rr.Code)
	}
}

// internalRoutePaths enumerates the six operator endpoints that MountCoreRoutes
// gates behind the internal-service token.
var internalRoutePaths = []string{
	"/internal/provider-test",
	"/internal/routing-simulate",
	"/internal/v1/credentials/cred-123/probe",
	"/internal/hooks-test",
	"/internal/embedding-probe",
	"/internal/semantic-prewarm",
}

// TestMountCoreRoutes_internalRoutesRejectMissingBearer asserts every mounted
// /internal/* route returns 401 when no bearer is presented — proving the
// guard is actually wired onto each route, not just defined.
func TestMountCoreRoutes_internalRoutesRejectMissingBearer(t *testing.T) {
	h := getSharedCoreHandler(t)
	for _, p := range internalRoutePaths {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s without bearer: expected 401, got %d", p, rr.Code)
		}
	}
}

// TestMountCoreRoutes_internalRoutesRejectWrongBearer asserts every mounted
// /internal/* route returns 401 for a non-matching token.
func TestMountCoreRoutes_internalRoutesRejectWrongBearer(t *testing.T) {
	h := getSharedCoreHandler(t)
	for _, p := range internalRoutePaths {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s with wrong bearer: expected 401, got %d", p, rr.Code)
		}
	}
}

// TestMountCoreRoutes_internalRoutesPassWithCorrectBearer asserts that with
// the correct token the request clears auth and reaches the handler. Because
// the token is configured, the guard's only rejection is 401; a non-401
// response proves the request passed auth and entered the handler (which may
// return other statuses — e.g. semantic-prewarm 503 when its writer is nil).
func TestMountCoreRoutes_internalRoutesPassWithCorrectBearer(t *testing.T) {
	h := getSharedCoreHandler(t)
	for _, p := range internalRoutePaths {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		req.Header.Set("Authorization", "Bearer "+testInternalToken)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code == http.StatusUnauthorized {
			t.Errorf("%s with correct bearer: auth must pass, got 401", p)
		}
	}
}
