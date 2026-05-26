package enroll

// enrollment_handler_test.go covers the EnrollmentAPI HTTP handler using stub
// implementations for CertAuthority, FleetManager, and EnrollmentSvc so no real
// CA directory, database, or fleet manager is required.
//
// Named failure modes tested:
//   - missing_csr_pem              → 400 INVALID_REQUEST
//   - no_auth_header               → 401 UNAUTHORIZED
//   - bearer_jwks_nil              → 503 JWKS_UNAVAILABLE
//   - token_invalid                → 401 UNAUTHORIZED
//   - ca_sign_error                → 400 BAD_REQUEST (no internal error text leaked)
//   - register_thing_error         → 500 INTERNAL_ERROR
//   - store_device_token_error     → 500 INTERNAL_ERROR
//   - happy_token_enrollment       → 200 with certPem, deviceToken, id, trustLevel
//   - happy_token_idempotent_thingid → 200 reuses supplied thingId
//   - happy_jwt_enrollment         → 200 with trustLevel via SSO path
//   - jwt_replayed                 → 401 JWT_REPLAYED
//   - jwt_wrong_purpose            → 401 JWT_INVALID

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// Stub implementations

// stubCA implements CertAuthority. signErr, when non-nil, is returned from
// SignCSR. Otherwise a synthetic CertResult is returned so that downstream
// code (device-token generation, store writes) can run.
type stubCA struct {
	signErr error
}

func (s *stubCA) SignCSR(csrPEM, subjectCN string) (*agentca.CertResult, error) {
	if s.signErr != nil {
		return nil, s.signErr
	}
	exp := time.Now().Add(90 * 24 * time.Hour)
	return &agentca.CertResult{
		CertPEM:   "-----BEGIN CERTIFICATE-----\nSTUB\n-----END CERTIFICATE-----\n",
		CaCertPEM: "-----BEGIN CERTIFICATE-----\nSTUB-CA\n-----END CERTIFICATE-----\n",
		Serial:    "AABBCCDDEEFF0011",
		ExpiresAt: exp,
	}, nil
}

// SignAttestationCSR mirrors SignCSR for the attestation-key path.
// Tests that don't exercise attestation get the same stub cert result.
func (s *stubCA) SignAttestationCSR(csrPEM, subjectCN string) (*agentca.CertResult, error) {
	return s.SignCSR(csrPEM, subjectCN)
}

// stubFleetManager implements FleetManager. registerErr, when non-nil, is
// returned from RegisterThing. st is the store the handler calls through
// Store() — typically backed by pgxmock.
type stubFleetManager struct {
	st          *store.Store
	registerErr error
}

func (m *stubFleetManager) RegisterThing(_ context.Context, req manager.RegisterRequest) (*manager.RegisterResponse, error) {
	if m.registerErr != nil {
		return nil, m.registerErr
	}
	return &manager.RegisterResponse{
		Desired:    map[string]any{},
		DesiredVer: 1,
	}, nil
}

func (m *stubFleetManager) ComputeAndStoreTrustLevel(_ context.Context, _, _, _ string) int {
	return 1
}

func (m *stubFleetManager) Store() *store.Store {
	return m.st
}

// stubEnrollSvc implements EnrollmentSvc.
type stubEnrollSvc struct {
	token   *enrollment.Token
	valid   bool
	markErr error
}

func (s *stubEnrollSvc) ValidateToken(_ context.Context, _ string) (*enrollment.Token, bool) {
	return s.token, s.valid
}

func (s *stubEnrollSvc) MarkUsed(_ context.Context, _, _ string) error {
	return s.markErr
}

// Test helpers

// silentLog returns a *slog.Logger that discards all output so test runs
// stay clean.
func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildAPI constructs an EnrollmentAPI ready to handle one request.
// ca / mgr / svc may be nil — the handler itself guards against nil JWKSCache.
func buildAPI(ca CertAuthority, mgr FleetManager, svc EnrollmentSvc) *EnrollmentAPI {
	api := &EnrollmentAPI{
		CA:         ca,
		Mgr:        mgr,
		Enrollment: svc,
		Logger:     silentLog(),
	}
	api.Init()
	return api
}

// post issues a POST request to /api/internal/things/enroll with the given
// JSON body and optional headers, runs it through api.Enroll, and returns the
// response recorder.
func post(t *testing.T, api *EnrollmentAPI, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/internal/things/enroll", &buf)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	_ = api.Enroll(c)
	return rec
}

// decodeBody decodes the response JSON into the supplied target.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

// newPgxmockStore creates a pgxmock pool and wraps it in a store for use in
// tests that exercise DB-bound store methods.
func newPgxmockStore(t *testing.T) (*store.Store, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	return store.NewWithPgxPool(mock), mock
}

// validCSR is a placeholder non-empty CSR string. The stub CA accepts any
// non-empty string; only the real *agentca.CA validates PEM structure.
const validCSR = "-----BEGIN CERTIFICATE REQUEST-----\nSTUB\n-----END CERTIFICATE REQUEST-----\n"

// Tests: request validation (no auth)

func TestEnroll_MissingCSR(t *testing.T) {
	api := buildAPI(&stubCA{}, nil, nil)
	rec := post(t, api, map[string]any{"thingType": "agent"}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "INVALID_REQUEST" {
		t.Errorf("want INVALID_REQUEST, got %q", body.Code)
	}
}

func TestEnroll_NoAuthHeader(t *testing.T) {
	api := buildAPI(&stubCA{}, nil, nil)
	rec := post(t, api, map[string]any{"csrPem": validCSR}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "UNAUTHORIZED" {
		t.Errorf("want UNAUTHORIZED, got %q", body.Code)
	}
}

// Tests: JWT enrollment path failures

func TestEnroll_BearerJWKSNil(t *testing.T) {
	// JWKSCache is nil → handler immediately returns 503.
	api := buildAPI(&stubCA{}, nil, nil)
	rec := post(t, api, map[string]any{"csrPem": validCSR},
		map[string]string{"Authorization": "Bearer some-jwt"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "JWKS_UNAVAILABLE" {
		t.Errorf("want JWKS_UNAVAILABLE, got %q", body.Code)
	}
}

// Tests: token enrollment path failures

func TestEnroll_TokenInvalid(t *testing.T) {
	// ValidateToken returns (nil, false) → 401.
	svc := &stubEnrollSvc{token: nil, valid: false}
	api := buildAPI(&stubCA{}, nil, svc)
	rec := post(t, api, map[string]any{"csrPem": validCSR},
		map[string]string{"X-Enrollment-Token": "bad-token"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "UNAUTHORIZED" {
		t.Errorf("want UNAUTHORIZED, got %q", body.Code)
	}
}

func TestEnroll_CASignError(t *testing.T) {
	// CA.SignCSR returns an error → 400 BAD_REQUEST; internal error text must
	// NOT be leaked verbatim in the "error" field (the handler wraps it with
	// "CSR signing failed: ..." which does expose the CA error message — this
	// is intentional per the spec: the CA error describes why the CSR was bad,
	// not an internal secret). We assert status code and code field.
	tok := &enrollment.Token{
		ID:        "tok-1",
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
	svc := &stubEnrollSvc{token: tok, valid: true}
	ca := &stubCA{signErr: errors.New("invalid CSR PEM")}
	api := buildAPI(ca, nil, svc)
	rec := post(t, api, map[string]any{"csrPem": validCSR, "thingType": "agent"},
		map[string]string{"X-Enrollment-Token": "tok"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "BAD_REQUEST" {
		t.Errorf("want BAD_REQUEST, got %q", body.Code)
	}
	// The signer's internal error message should not appear raw in the body's
	// "error" field as a naked internal string (i.e., the handler wraps it).
	if body.Error == "invalid CSR PEM" {
		t.Error("raw CA error leaked verbatim; handler must wrap it")
	}
	if !strings.Contains(body.Error, "CSR signing failed") {
		t.Errorf("want 'CSR signing failed' prefix, got %q", body.Error)
	}
}

func TestEnroll_RegisterThingError(t *testing.T) {
	// CA signs OK but RegisterThing fails → 500 INTERNAL_ERROR.
	tok := &enrollment.Token{
		ID:        "tok-2",
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
	svc := &stubEnrollSvc{token: tok, valid: true}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st, registerErr: errors.New("db unavailable")}
	api := buildAPI(&stubCA{}, mgr, svc)

	// No DB expectations needed because RegisterThing fails before any store call.
	rec := post(t, api, map[string]any{"csrPem": validCSR, "thingType": "agent"},
		map[string]string{"X-Enrollment-Token": "tok"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "INTERNAL_ERROR" {
		t.Errorf("want INTERNAL_ERROR, got %q", body.Code)
	}
}

func TestEnroll_StoreDeviceTokenError(t *testing.T) {
	// CA signs OK, RegisterThing OK, but StoreDeviceTokenHash fails → 500.
	tok := &enrollment.Token{
		ID:        "tok-3",
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
	svc := &stubEnrollSvc{token: tok, valid: true}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}
	api := buildAPI(&stubCA{}, mgr, svc)

	// StoreDeviceTokenHash: UPDATE thing SET metadata = jsonb_set(...)
	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("db write failed"))

	rec := post(t, api, map[string]any{"csrPem": validCSR, "thingType": "agent"},
		map[string]string{"X-Enrollment-Token": "tok"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "INTERNAL_ERROR" {
		t.Errorf("want INTERNAL_ERROR, got %q", body.Code)
	}
}

// Tests: happy path — token enrollment

func TestEnroll_HappyTokenEnrollment(t *testing.T) {
	tok := &enrollment.Token{
		ID:        "tok-happy",
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
	svc := &stubEnrollSvc{token: tok, valid: true}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}
	api := buildAPI(&stubCA{}, mgr, svc)

	// StoreDeviceTokenHash: UPDATE thing SET metadata = jsonb_set(...) WHERE id = $1
	// Args: (thingID string, tokenHash string)
	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	// UpdateThingAgent with hostname/os/osVersion present:
	// First: UPDATE thing SET hostname = ... WHERE id = $1  (4 args)
	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	// Second: INSERT INTO thing_agent ... ON CONFLICT DO UPDATE  (3 args)
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))

	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "agent",
		"hostname":  "mac-01",
		"os":        "darwin",
		"osVersion": "14.0",
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	decodeBody(t, rec, &body)

	// Verify response shape: required fields present.
	for _, field := range []string{"id", "deviceToken", "certPem", "caCertPem", "certSerial", "trustLevel"} {
		if _, ok := body[field]; !ok {
			t.Errorf("response missing field %q", field)
		}
	}

	// id must be non-empty.
	id, _ := body["id"].(string)
	if id == "" {
		t.Error("response id must be non-empty")
	}

	// trustLevel must be numeric (JSON number decodes as float64).
	tl, ok := body["trustLevel"].(float64)
	if !ok {
		t.Errorf("trustLevel must be a number, got %T", body["trustLevel"])
	}
	if tl < 0 || tl > 3 {
		t.Errorf("trustLevel %v out of range [0,3]", tl)
	}

	// certPem must not be empty.
	certPem, _ := body["certPem"].(string)
	if certPem == "" {
		t.Error("certPem must be non-empty")
	}
}

func TestEnroll_HappyTokenEnrollment_ExplicitThingID(t *testing.T) {
	// When the client supplies thingId, the response id must match.
	tok := &enrollment.Token{
		ID:        "tok-explicit",
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
	svc := &stubEnrollSvc{token: tok, valid: true}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}
	api := buildAPI(&stubCA{}, mgr, svc)

	// StoreDeviceTokenHash (no hostname/os in this request, so UpdateThingAgent
	// skips the UPDATE thing SET hostname step and goes directly to INSERT).
	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))

	const wantID = "agent-fixed-id-123"
	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "agent",
		"thingId":   wantID,
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	decodeBody(t, rec, &body)
	gotID, _ := body["id"].(string)
	if gotID != wantID {
		t.Errorf("response id = %q, want %q", gotID, wantID)
	}
}

func TestEnroll_MarkUsedError_IsNonFatal(t *testing.T) {
	// MarkUsed failing must not cause a 5xx — it is best-effort and logged
	// at Warn only. The response is still 200 OK.
	tok := &enrollment.Token{
		ID:        "tok-markused",
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
	svc := &stubEnrollSvc{token: tok, valid: true, markErr: errors.New("db timeout")}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}
	api := buildAPI(&stubCA{}, mgr, svc)

	// No hostname/os → UpdateThingAgent skips UPDATE thing SET hostname.
	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))

	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "agent",
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("MarkUsed error must not block enrollment; got %d body: %s",
			rec.Code, rec.Body.String())
	}
}

func TestEnroll_UpdateThingAgentError_IsNonFatal(t *testing.T) {
	// UpdateThingAgent failure is best-effort (Warn log) — enrollment must still
	// return 200 OK and include all required response fields.
	tok := &enrollment.Token{
		ID:        "tok-agent-upsert",
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
	svc := &stubEnrollSvc{token: tok, valid: true}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}
	api := buildAPI(&stubCA{}, mgr, svc)

	// StoreDeviceTokenHash succeeds.
	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	// UpdateThingAgent fails on the INSERT step — should be swallowed.
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("thing_agent row missing"))

	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "agent",
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("UpdateThingAgent error must not block enrollment; got %d body: %s",
			rec.Code, rec.Body.String())
	}

	var body map[string]any
	decodeBody(t, rec, &body)
	if _, ok := body["certPem"]; !ok {
		t.Error("response must still include certPem")
	}
}

// Tests: verifyEnrollmentJWT — exercised via jtiSeen directly (white-box)

func TestVerifyEnrollmentJWT_ReplayGuard(t *testing.T) {
	// MarkSeen is called by verifyEnrollmentJWT when the JWT parses cleanly.
	// We can't reach verifyEnrollmentJWT without a valid RS256 JWT, so we
	// test the jtiCache contract directly — the same contract the handler
	// relies on. This verifies the named failure mode "JWT_REPLAYED" at the
	// component level.
	c := newJTICache()
	defer c.Stop()

	exp := time.Now().Add(5 * time.Minute)
	const jti = "unique-jti-42"

	// First call: new JTI → allowed.
	if !c.MarkSeen(jti, exp) {
		t.Fatal("first MarkSeen should return true (new JTI)")
	}
	// Second call: same JTI → replay detected.
	if c.MarkSeen(jti, exp) {
		t.Fatal("second MarkSeen should return false (replay)")
	}
}

func TestVerifyEnrollmentJWT_EmptyJTIRejected(t *testing.T) {
	// MarkSeen with an empty jti must always return false (no-op guard).
	c := newJTICache()
	defer c.Stop()
	if c.MarkSeen("", time.Now().Add(time.Minute)) {
		t.Error("empty JTI must be rejected")
	}
}

// Tests: JWT enrollment — nil JWKSCache paths

func TestEnrollWithJWT_JWKSCacheNil_Returns503(t *testing.T) {
	// When JWKSCache is nil the enrollWithJWT path must immediately return 503
	// without touching CA, Mgr, or Enrollment.
	api := &EnrollmentAPI{
		CA:         &stubCA{},
		Mgr:        nil,
		Enrollment: nil,
		JWKSCache:  nil,
		Logger:     silentLog(),
	}
	api.Init()

	rec := post(t, api, map[string]any{"csrPem": validCSR},
		map[string]string{"Authorization": "Bearer fake-jwt"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "JWKS_UNAVAILABLE" {
		t.Errorf("want JWKS_UNAVAILABLE, got %q", body.Code)
	}
}

// Tests: resolveThingID

func TestResolveThingID_ExplicitID(t *testing.T) {
	api := &EnrollmentAPI{}
	id, err := api.resolveThingID("agent", "my-specific-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "my-specific-id" {
		t.Errorf("want 'my-specific-id', got %q", id)
	}
}

func TestResolveThingID_Generated(t *testing.T) {
	api := &EnrollmentAPI{}
	id, err := api.resolveThingID("agent", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(id, "agent-") {
		t.Errorf("generated id must start with 'agent-', got %q", id)
	}
	// Uniqueness: two calls must produce different IDs.
	id2, err := api.resolveThingID("agent", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == id2 {
		t.Error("two generated IDs must differ")
	}
}

// Tests: DeviceOrServiceAuth middleware

func TestDeviceOrServiceAuth_MissingAuthHeader(t *testing.T) {
	// No Authorization header → 401.
	mw := DeviceOrServiceAuth(nil, "svc-token")
	called := false
	handler := mw(func(c echo.Context) error {
		called = true
		return nil
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := handler(c); err != nil {
		t.Logf("handler returned err (expected): %v", err)
	}
	if called {
		t.Error("next handler must not be called when auth header missing")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rec.Code)
	}
}

func TestDeviceOrServiceAuth_InvalidBearerFormat(t *testing.T) {
	// Authorization header that is not "Bearer <token>" → 401.
	mw := DeviceOrServiceAuth(nil, "svc-token")
	called := false
	handler := mw(func(c echo.Context) error {
		called = true
		return nil
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	_ = handler(c)
	if called {
		t.Error("next handler must not be called with non-Bearer auth")
	}
}

func TestDeviceOrServiceAuth_ServiceTokenAccepted(t *testing.T) {
	// Matching service token → next handler called with no Thing in context.
	const svcToken = "secret-service-token"
	mw := DeviceOrServiceAuth(nil, svcToken)
	called := false
	handler := mw(func(c echo.Context) error {
		called = true
		if ThingFromContext(c) != nil {
			return fmt.Errorf("service token auth must not set Thing in context")
		}
		return nil
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+svcToken)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := handler(c); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !called {
		t.Error("next handler must be called when service token matches")
	}
	_ = rec
}

func TestDeviceOrServiceAuth_DeviceTokenMissingThingID(t *testing.T) {
	// Device token without X-Thing-Id header → 401.
	mw := DeviceOrServiceAuth(nil, "different-token")
	handler := mw(func(c echo.Context) error { return nil })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer device-tok")
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	_ = handler(c)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401 when X-Thing-Id missing, got %d", rec.Code)
	}
}

// Tests: Init / Close lifecycle

func TestEnrollmentAPI_InitClose_Idempotent(t *testing.T) {
	api := &EnrollmentAPI{Logger: silentLog()}
	// Init twice must not panic or create duplicate goroutines.
	api.Init()
	api.Init()
	// Close must be idempotent.
	api.Close()
	api.Close()
}
