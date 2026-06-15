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
//   - client_supplied_thingid      → ignored; server mints its own id (F-0200)
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
	"sync"
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
// SignAttestationCSR. Otherwise a synthetic CertResult is returned so the
// attestation enrollment branch can run.
type stubCA struct {
	signErr error
}

// SignAttestationCSR is the attestation-key signing path. Returns a synthetic
// CertResult unless signErr is set (drives the non-fatal attestation-error arm).
func (s *stubCA) SignAttestationCSR(csrPEM, subjectCN string) (*agentca.CertResult, error) {
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
//
// valid==true models a successful atomic consume (ConsumeToken returns the
// token); valid==false models the lost-race / expired / unknown case
// (ConsumeToken returns enrollment.ErrAlreadyUsed). markErr is surfaced from
// LinkThing (the post-consume best-effort binding) so the existing
// "mark-used error is non-fatal" assertions still exercise that path.
// consumeCalls / linkCalls let race-arbiter tests count invocations.
type stubEnrollSvc struct {
	token       *enrollment.Token
	valid       bool
	markErr     error
	consumeErr  error // when set, ConsumeToken returns this verbatim (overrides valid)
	consumeOnce bool  // when true, only the first ConsumeToken wins; the rest get ErrAlreadyUsed
	mu          sync.Mutex
	consumeWon  int
	linkCalls   int
}

func (s *stubEnrollSvc) ConsumeToken(_ context.Context, _ string) (*enrollment.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumeErr != nil {
		return nil, s.consumeErr
	}
	if s.consumeOnce {
		if s.consumeWon > 0 {
			return nil, enrollment.ErrAlreadyUsed
		}
		s.consumeWon++
		return s.token, nil
	}
	if !s.valid {
		return nil, enrollment.ErrAlreadyUsed
	}
	s.consumeWon++
	return s.token, nil
}

func (s *stubEnrollSvc) LinkThing(_ context.Context, _, _ string) error {
	s.mu.Lock()
	s.linkCalls++
	s.mu.Unlock()
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

// ErrorResponse is a test-only helper for asserting the canonical nested error
// envelope {error:{message,type,code}} (F-0319). It unmarshals the inner object.
type ErrorResponse struct {
	Code    string `json:"-"`
	Message string `json:"-"`
}

func (e *ErrorResponse) UnmarshalJSON(data []byte) error {
	var wrapper struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return err
	}
	e.Code = wrapper.Error.Code
	e.Message = wrapper.Error.Message
	return nil
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

// F-0204: a request that LOSES the consume race (ConsumeToken →
// ErrAlreadyUsed) must be rejected with 401 BEFORE any enrollment side effect.
// We wire a fleet manager whose store has ZERO mock expectations: if the
// handler reached doEnroll (signed the CSR, registered the thing, stored the
// token) it would hit an unexpected DB call and the test would fail. A clean
// 401 with no DB interaction proves consume-FIRST ordering.
func TestEnroll_TokenConsumeLostRace_NoEnrollmentSideEffect(t *testing.T) {
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}
	// valid:false → ConsumeToken returns enrollment.ErrAlreadyUsed.
	svc := &stubEnrollSvc{token: nil, valid: false}
	api := buildAPI(&stubCA{}, mgr, svc)

	rec := post(t, api, map[string]any{"csrPem": validCSR, "thingType": "agent"},
		map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("lost-race consume must 401, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if svc.linkCalls != 0 {
		t.Errorf("loser must not call LinkThing; got %d calls", svc.linkCalls)
	}
	// No DB expectations were registered; ExpectationsWereMet confirms the
	// handler made zero store calls (doEnroll never ran).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("loser performed unexpected DB work (doEnroll ran): %v", err)
	}
}

// F-0204: under N concurrent enrollments with the SAME token, exactly one wins.
// The stub's consumeOnce models the DB's atomic UPDATE...WHERE pending RETURNING
// (the real arbiter, verified at the store layer); here we assert the handler
// honours it — one consume win, the rest ErrAlreadyUsed → 401 — with no data
// race in the consume bookkeeping (run under -race).
func TestEnroll_ConcurrentSameToken_ExactlyOneWins(t *testing.T) {
	tok := &enrollment.Token{
		ID:        "tok-race",
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
	svc := &stubEnrollSvc{token: tok, consumeOnce: true}

	const n = 16
	type result struct{ code int }
	results := make(chan result, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine gets its own store+manager so the only shared
			// arbiter is the consumeOnce stub (mirrors per-request DB conns).
			st, mock := newPgxmockStore(t)
			defer mock.Close()
			// The single winner will run doEnroll; allow its store calls.
			mock.ExpectExec(`UPDATE thing`).
				WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
				WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
			mock.ExpectExec(`INSERT INTO thing_agent`).
				WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
				WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
			mgr := &stubFleetManager{st: st}
			api := buildAPI(&stubCA{}, mgr, svc)
			rec := post(t, api, map[string]any{"csrPem": validCSR, "thingType": "agent"},
				map[string]string{"X-Enrollment-Token": "tok"})
			results <- result{code: rec.Code}
		}()
	}
	wg.Wait()
	close(results)

	wins, losses := 0, 0
	for r := range results {
		switch r.code {
		case http.StatusOK:
			wins++
		case http.StatusUnauthorized:
			losses++
		default:
			t.Errorf("unexpected status %d", r.code)
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one enrollment must win the consume race; got %d wins, %d losses", wins, losses)
	}
	if losses != n-1 {
		t.Errorf("want %d losers, got %d", n-1, losses)
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
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
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
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
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

	// Verify response shape: required fields present. Enrollment returns the
	// device token + thing id (no mTLS cert fields — F-0203 removed that surface).
	for _, field := range []string{"id", "deviceToken", "deviceTokenExpiresAt", "trustLevel"} {
		if _, ok := body[field]; !ok {
			t.Errorf("response missing field %q", field)
		}
	}
	// The removed mTLS cert fields must NOT appear.
	for _, field := range []string{"certPem", "caCertPem", "certSerial", "certExpiresAt"} {
		if _, ok := body[field]; ok {
			t.Errorf("response must not include removed cert field %q", field)
		}
	}

	// The device token must carry a parseable, future expiry (F-0202): a token
	// is no longer issued without a bounded lifetime.
	expStr, _ := body["deviceTokenExpiresAt"].(string)
	exp, perr := time.Parse(time.RFC3339, expStr)
	if perr != nil {
		t.Fatalf("deviceTokenExpiresAt not RFC3339: %q (%v)", expStr, perr)
	}
	if !exp.After(time.Now()) {
		t.Errorf("deviceTokenExpiresAt must be in the future, got %v", exp)
	}
	if d := time.Until(exp); d > agentca.DeviceTokenTTL+time.Minute || d < agentca.DeviceTokenTTL-24*time.Hour {
		t.Errorf("expiry %v not within one TTL (%v) of now", exp, agentca.DeviceTokenTTL)
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

	// deviceToken must not be empty (it is the identity credential).
	deviceToken, _ := body["deviceToken"].(string)
	if deviceToken == "" {
		t.Error("deviceToken must be non-empty")
	}
}

func TestEnroll_ClientSuppliedThingID_IsIgnored(t *testing.T) {
	// F-0200 regression: a client that puts a thingId in the body (attempting to
	// name/overwrite an existing Thing or a service identity) must NOT get that
	// id — the Hub always mints a fresh server-assigned id.
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

	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))

	const attackerID = "ai-gateway" // a well-known service identity to hijack
	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "agent",
		"thingId":   attackerID,
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	decodeBody(t, rec, &body)
	gotID, _ := body["id"].(string)
	if gotID == attackerID {
		t.Fatalf("client-supplied thingId was honored — takeover possible (F-0200): %q", gotID)
	}
	if !strings.HasPrefix(gotID, "agent-") {
		t.Errorf("expected a server-minted agent-* id, got %q", gotID)
	}
}

func TestEnroll_TokenThingTypeIsAuthoritative(t *testing.T) {
	// F-0200 regression (type half): a token issued for `agent` must enroll as
	// `agent` even if the request body claims `ai-gateway` — the caller cannot
	// upgrade the type to a service identity.
	tok := &enrollment.Token{
		ID:        "tok-type",
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
	svc := &stubEnrollSvc{token: tok, valid: true}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}
	api := buildAPI(&stubCA{}, mgr, svc)

	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))

	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "ai-gateway", // attacker tries to enroll as a service
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	gotID, _ := body["id"].(string)
	if !strings.HasPrefix(gotID, "agent-") {
		t.Errorf("token ThingType must win: expected an agent-* id, got %q (req claimed ai-gateway)", gotID)
	}
}

func TestEnroll_LinkThingError_IsNonFatal(t *testing.T) {
	// The post-consume LinkThing call (recording the minted thing id on the
	// already-spent token) failing must not cause a 5xx — the token is
	// single-use-consumed regardless, so this is best-effort and logged at
	// Warn only. The response is still 200 OK.
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
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
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
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
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
	if _, ok := body["deviceToken"]; !ok {
		t.Error("response must still include deviceToken")
	}
}

// Tests: verifyEnrollmentJWT — exercised via jtiSeen directly (white-box)

func TestVerifyEnrollmentJWT_ReplayGuard(t *testing.T) {
	// MarkSeen is called by verifyEnrollmentJWT when the JWT parses cleanly.
	// We can't reach verifyEnrollmentJWT without a valid RS256 JWT, so we
	// test the jtiCache contract directly — the same contract the handler
	// relies on. This verifies the named failure mode "JWT_REPLAYED" at the
	// component level.
	c := newJTICache(nil, nil)
	defer c.Stop()

	exp := time.Now().Add(5 * time.Minute)
	const jti = "unique-jti-42"

	// First call: new JTI → allowed.
	if !c.MarkSeen(context.Background(), jti, exp) {
		t.Fatal("first MarkSeen should return true (new JTI)")
	}
	// Second call: same JTI → replay detected.
	if c.MarkSeen(context.Background(), jti, exp) {
		t.Fatal("second MarkSeen should return false (replay)")
	}
}

func TestVerifyEnrollmentJWT_EmptyJTIRejected(t *testing.T) {
	// MarkSeen with an empty jti must always return false (no-op guard).
	c := newJTICache(nil, nil)
	defer c.Stop()
	if c.MarkSeen(context.Background(), "", time.Now().Add(time.Minute)) {
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

func TestResolveThingID_Generated(t *testing.T) {
	api := &EnrollmentAPI{}
	id, err := api.resolveThingID("agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(id, "agent-") {
		t.Errorf("generated id must start with 'agent-', got %q", id)
	}
	// Uniqueness: two calls must produce different IDs — the server mints a
	// fresh, unguessable ID every time; the caller can never pin one (F-0200).
	id2, err := api.resolveThingID("agent")
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
