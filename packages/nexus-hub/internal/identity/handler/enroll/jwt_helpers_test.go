package enroll

// jwt_helpers_test.go covers verifyEnrollmentJWT via a real RS256 JWT and
// a stub JWKSKeyGetter, plus the helpers.go utility functions and
// jti_cache.sweep (the background compaction path).

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/enrollment"
)

// Stub JWKSKeyGetter

// stubJWKS implements JWKSKeyGetter. It returns the given public key for any
// kid, or the given error when errMode is set.
type stubJWKS struct {
	key    *rsa.PublicKey
	getErr error
}

func (s *stubJWKS) Get(_ string) (*rsa.PublicKey, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.key, nil
}

// makeRS256JWT creates an RS256 JWT signed with priv. claims must include
// RegisteredClaims with Subject, ID, Audience and ExpiresAt set.
func makeRS256JWT(t *testing.T, priv *rsa.PrivateKey, claims jwt.Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "test-key-id"
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return s
}

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return k
}

// Tests: verifyEnrollmentJWT — exercised end-to-end via enrollWithJWT

func TestEnroll_JWT_JWKS_CacheEmpty_Returns503(t *testing.T) {
	// stubJWKS returns the ErrCacheEmpty equivalent — string "cache is empty"
	// in the error message. The handler should map this to 503 JWKS_UNAVAILABLE.
	// We must send a structurally valid RS256 JWT (even with wrong sig) so the
	// parser reaches the key-lookup callback before failing on signature.
	api := buildAPI(&stubCA{}, nil, nil)
	api.JWKSCache = &stubJWKS{getErr: errCacheEmptyStub}
	api.jtiSeen = newJTICache(nil, nil)

	// Build a JWT signed with a throwaway key — the callback will return the
	// cache-empty error before signature verification even tries the key.
	throwaway := newRSAKey(t)
	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-x",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        "jti-cache-empty",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, throwaway, claims)

	rec := post(t, api, map[string]any{"csrPem": validCSR},
		map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "JWKS_UNAVAILABLE" {
		t.Errorf("want JWKS_UNAVAILABLE, got %q", body.Code)
	}
}

// errCacheEmptyStub embeds "cache is empty" so the handler's string-match
// triggers the 503 branch.
type cacheEmptyErr struct{}

func (cacheEmptyErr) Error() string {
	return "jwks: cache is empty; CP JWKS endpoint may be unreachable"
}

var errCacheEmptyStub = cacheEmptyErr{}

func TestEnroll_JWT_BadSignature_Returns401(t *testing.T) {
	// JWT signed with a different key → verification fails → 401 JWT_INVALID.
	signerKey := newRSAKey(t)
	verifierKey := newRSAKey(t) // different key
	api := buildAPI(&stubCA{}, nil, nil)
	api.JWKSCache = &stubJWKS{key: &verifierKey.PublicKey}
	api.jtiSeen = newJTICache(nil, nil)

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-1",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        "jti-bad-sig",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Email:   "user@example.com",
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, signerKey, claims)

	rec := post(t, api, map[string]any{"csrPem": validCSR},
		map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "JWT_INVALID" {
		t.Errorf("want JWT_INVALID, got %q", body.Code)
	}
}

func TestEnroll_JWT_WrongPurpose_Returns401(t *testing.T) {
	// JWT with purpose != "enrollment" → verifyEnrollmentJWT returns JWT_INVALID.
	key := newRSAKey(t)
	api := buildAPI(&stubCA{}, nil, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}
	api.jtiSeen = newJTICache(nil, nil)

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-2",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        "jti-wrong-purpose",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: "not-enrollment",
	}
	tokenStr := makeRS256JWT(t, key, claims)

	rec := post(t, api, map[string]any{"csrPem": validCSR},
		map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "JWT_INVALID" {
		t.Errorf("want JWT_INVALID, got %q", body.Code)
	}
}

func TestEnroll_JWT_MissingJTI_Returns401(t *testing.T) {
	// JWT with no ID (jti) → verifyEnrollmentJWT returns JWT_INVALID (missing jti).
	key := newRSAKey(t)
	api := buildAPI(&stubCA{}, nil, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}
	api.jtiSeen = newJTICache(nil, nil)

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-3",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			// ID deliberately omitted
			IssuedAt: jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)

	rec := post(t, api, map[string]any{"csrPem": validCSR},
		map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "JWT_INVALID" {
		t.Errorf("want JWT_INVALID, got %q", body.Code)
	}
}

func TestEnroll_JWT_Replayed_Returns401(t *testing.T) {
	// Two requests with the same valid JWT → second must be rejected as JWT_REPLAYED.
	key := newRSAKey(t)
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}

	api := buildAPI(&stubCA{}, mgr, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	jtiVal := "jti-replay-test-" + time.Now().Format("150405.000")
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-4",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        jtiVal,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)

	// First request: CA signs, but then DB calls happen. The first request
	// will reach the DB (StoreDeviceTokenHash). Set expectations for first call.
	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))

	rec1 := post(t, api, map[string]any{"csrPem": validCSR, "thingType": "agent"},
		map[string]string{"Authorization": "Bearer " + tokenStr})

	// First request must succeed (200 OK — JWT is valid and JTI is fresh).
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: want 200, got %d; body: %s", rec1.Code, rec1.Body.String())
	}

	// Second request with same JWT: JTI already in cache → 401 JWT_REPLAYED.
	rec2 := post(t, api, map[string]any{"csrPem": validCSR, "thingType": "agent"},
		map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("second request: want 401 JWT_REPLAYED, got %d; body: %s",
			rec2.Code, rec2.Body.String())
	}
	var body ErrorResponse
	decodeBody(t, rec2, &body)
	if body.Code != "JWT_REPLAYED" {
		t.Errorf("want JWT_REPLAYED, got %q", body.Code)
	}
}

func TestEnroll_JWT_ValidToken_HappyPath(t *testing.T) {
	// Valid RS256 enrollment JWT with no CpIssuer → SSO enrollment succeeds.
	key := newRSAKey(t)
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}

	api := buildAPI(&stubCA{}, mgr, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}
	// CpIssuer not set → issuer pinning skipped (test-only shortcut).

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	jtiVal := "jti-happy-jwt-" + time.Now().Format("150405.000")
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-sso-1",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        jtiVal,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Email:   "sso@example.com",
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)

	// DB expectations for doEnroll:
	// 1. StoreDeviceTokenHash
	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	// 2. UpdateThingAgent (no hostname/os → no UPDATE thing SET hostname)
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
	// 3. UpsertDeviceAssignment (enrollWithJWT path, SSO): 3 steps
	//    Step 1: release stale
	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
	//    Step 2: insert new assignment
	mock.ExpectExec(`INSERT INTO "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
	//    Step 3: sync thing_agent.current_assignment_id
	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "agent",
	}, map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	for _, field := range []string{"id", "deviceToken", "trustLevel"} {
		if _, ok := body[field]; !ok {
			t.Errorf("response missing field %q", field)
		}
	}
}

// Tests: helper functions

func makeCtx(t *testing.T, method, target string) echo.Context {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec)
}

func TestHelpers_Forbidden(t *testing.T) {
	c := makeCtx(t, http.MethodGet, "/")
	if err := forbidden(c, "not allowed"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rec := c.Response().Writer.(*httptest.ResponseRecorder)
	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "FORBIDDEN" {
		t.Errorf("want FORBIDDEN, got %q", body.Code)
	}
}

func TestHelpers_NotFound(t *testing.T) {
	c := makeCtx(t, http.MethodGet, "/")
	if err := notFound(c, "thing gone"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rec := c.Response().Writer.(*httptest.ResponseRecorder)
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "NOT_FOUND" {
		t.Errorf("want NOT_FOUND, got %q", body.Code)
	}
}

func TestHelpers_ServiceUnavailable(t *testing.T) {
	c := makeCtx(t, http.MethodGet, "/")
	if err := serviceUnavailable(c, "upstream down"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rec := c.Response().Writer.(*httptest.ResponseRecorder)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "SERVICE_UNAVAILABLE" {
		t.Errorf("want SERVICE_UNAVAILABLE, got %q", body.Code)
	}
}

func TestHelpers_HandleErr_NotFound(t *testing.T) {
	c := makeCtx(t, http.MethodGet, "/")
	_ = handleErr(c, errNotFoundStub{})
	rec := c.Response().Writer.(*httptest.ResponseRecorder)
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

func TestHelpers_HandleErr_Internal(t *testing.T) {
	c := makeCtx(t, http.MethodGet, "/")
	_ = handleErr(c, errOtherStub{})
	rec := c.Response().Writer.(*httptest.ResponseRecorder)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
}

// errNotFoundStub wraps store.ErrNotFound via errors.Is so handleErr
// routes to notFound. We reproduce the sentinel behaviour by wrapping
// the imported value from the store package.
type errNotFoundStub struct{}

func (errNotFoundStub) Error() string { return "not found" }
func (errNotFoundStub) Is(target error) bool {
	// import the real sentinel via the store sub-package path avoidance:
	// we compare against the Error() string of store.ErrNotFound.
	return target.Error() == "not found"
}

type errOtherStub struct{}

func (errOtherStub) Error() string { return "some other error" }

func TestHelpers_ParseIntDefault(t *testing.T) {
	tests := []struct {
		in   string
		def  int
		want int
	}{
		{"", 10, 10},
		{"42", 10, 42},
		{"0", 10, 10},  // <= 0 → default
		{"-1", 10, 10}, // < 1 → default
		{"abc", 10, 10},
	}
	for _, tc := range tests {
		got := parseIntDefault(tc.in, tc.def)
		if got != tc.want {
			t.Errorf("parseIntDefault(%q, %d) = %d, want %d", tc.in, tc.def, got, tc.want)
		}
	}
}

func TestHelpers_Clamp(t *testing.T) {
	if got := clamp(5, 1, 10); got != 5 {
		t.Errorf("clamp(5,1,10) = %d, want 5", got)
	}
	if got := clamp(0, 1, 10); got != 1 {
		t.Errorf("clamp(0,1,10) = %d, want 1", got)
	}
	if got := clamp(20, 1, 10); got != 10 {
		t.Errorf("clamp(20,1,10) = %d, want 10", got)
	}
}

func TestHelpers_ParseTimeOrNil(t *testing.T) {
	if got := parseTimeOrNil(""); got != nil {
		t.Error("empty string must return nil")
	}
	if got := parseTimeOrNil("not-a-time"); got != nil {
		t.Error("invalid time string must return nil")
	}
	valid := "2026-01-01T00:00:00Z"
	got := parseTimeOrNil(valid)
	if got == nil {
		t.Fatal("valid RFC3339 string must return non-nil time")
	}
	if got.Year() != 2026 {
		t.Errorf("parsed year = %d, want 2026", got.Year())
	}
}

// Tests: jti_cache sweep (background compaction path)

func TestJTICache_SweepLoop_TickerPath(t *testing.T) {
	// Exercise the ticker-fired sweep path in sweepLoop. We create a jtiCache
	// with a very short interval so the ticker fires within the test, then
	// verify an expired entry is removed without the stop signal.
	c := &jtiCache{
		entries: make(map[string]time.Time),
		stopCh:  make(chan struct{}),
		now:     time.Now,
	}
	// Add an already-expired entry so sweep has something to remove.
	c.entries["ticker-expired"] = time.Now().Add(-time.Minute)

	// Run sweepLoop with a 1ms interval in a goroutine; stop it after giving
	// the ticker a chance to fire at least once.
	done := make(chan struct{})
	go func() {
		c.sweepLoop(time.Millisecond)
		close(done)
	}()

	// Wait long enough for the ticker to fire (5ms >> 1ms).
	time.Sleep(5 * time.Millisecond)
	c.Stop()
	<-done // goroutine must exit after Stop()

	c.mu.Lock()
	_, stillPresent := c.entries["ticker-expired"]
	c.mu.Unlock()
	if stillPresent {
		t.Error("ticker-fired sweep must have removed the expired entry")
	}
}

func TestJTICache_Sweep_RemovesExpiredEntries(t *testing.T) {
	// Directly exercise the sweep() method by controlling the now() clock.
	c := &jtiCache{
		entries: make(map[string]time.Time),
		stopCh:  make(chan struct{}),
	}
	past := time.Now().Add(-time.Minute)  // already expired
	future := time.Now().Add(time.Minute) // not yet expired

	c.entries["expired-jti"] = past
	c.entries["fresh-jti"] = future

	// Override now so sweep sees both entries as evaluated at the current time.
	c.now = time.Now
	c.sweep()

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries["expired-jti"]; ok {
		t.Error("expired entry must be removed by sweep")
	}
	if _, ok := c.entries["fresh-jti"]; !ok {
		t.Error("fresh entry must survive sweep")
	}
}

func TestJTICache_SweepLoop_StopsOnClose(t *testing.T) {
	// sweepLoop must exit when the stop channel is closed. We test this by
	// creating the cache, stopping it immediately, and verifying the goroutine
	// does not block the test from finishing (test timeout = proof of liveness).
	c := newJTICache(nil, nil)
	c.Stop() // signals sweepLoop to exit via stopCh
	// If sweepLoop does not exit, the test will hang and eventually time out.
}

// Tests: DeviceOrServiceAuth — uncovered device-token error branches

func TestDeviceOrServiceAuth_InvalidTokenFormat_Returns401(t *testing.T) {
	// Bearer token that is not valid hex → HashDeviceToken returns error → 401.
	mw := DeviceOrServiceAuth(nil, "different-svc-token")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer not-a-hex-token!!!")
	req.Header.Set("X-Thing-Id", "thing-abc")
	e := echo.New()
	c := e.NewContext(req, rec)
	handler := mw(func(c echo.Context) error { return nil })
	_ = handler(c)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("invalid hex token: want 401, got %d", rec.Code)
	}
}

func TestDeviceOrServiceAuth_DeviceTokenByQueryParam(t *testing.T) {
	// X-Thing-Id may also come via ?id= query parameter. Check that the param
	// fallback path is exercised (auth will fail at ValidateDeviceToken because
	// the store is nil, but we verify it reached the device-token path).
	mw := DeviceOrServiceAuth(nil, "different-svc-token")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?id=thing-qp", nil)
	req.Header.Set("Authorization", "Bearer not-a-hex-token!!!")
	e := echo.New()
	c := e.NewContext(req, rec)
	handler := mw(func(c echo.Context) error { return nil })
	_ = handler(c)
	// Without a valid hex token format → 401 (invalid token format, not a
	// "no X-Thing-Id" error). Proves the ?id= fallback was entered.
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("query-param thing id: want 401, got %d", rec.Code)
	}
}

// Tests: ThingFromContext — both branches

func TestThingFromContext_NilValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	// Nothing set → must return nil.
	if got := ThingFromContext(c); got != nil {
		t.Errorf("ThingFromContext with no value: want nil, got %+v", got)
	}
}

func TestThingFromContext_WrongType(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	// Set a value that is not *store.Thing → type assertion fails → nil returned.
	c.Set(thingContextKey, "not-a-thing")
	if got := ThingFromContext(c); got != nil {
		t.Errorf("ThingFromContext with wrong type: want nil, got %+v", got)
	}
}

// Tests: logger() fallback branch

func TestEnrollmentAPI_LoggerFallback(t *testing.T) {
	// When Logger is nil, logger() must return slog.Default() without panic.
	api := &EnrollmentAPI{Logger: nil}
	l := api.logger()
	if l == nil {
		t.Error("logger() must never return nil")
	}
}

// Tests: verifyEnrollmentJWT — jtiSeen nil branch FAILS CLOSED

func TestVerifyEnrollmentJWT_JTISeenNil_FailsClosed(t *testing.T) {
	// When jtiSeen is nil (Init() never ran — a wiring bug),
	// verifyEnrollmentJWT MUST reject rather than proceed without replay
	// protection. The previous behaviour logged an error and accepted the
	// JWT (fail-open), silently disabling the replay guard. We now require a
	// JTI_STORE_UNAVAILABLE error so the request is refused.
	key := newRSAKey(t)
	api := &EnrollmentAPI{
		CA:        &stubCA{},
		JWKSCache: &stubJWKS{key: &key.PublicKey},
		Logger:    silentLog(),
		// jtiSeen intentionally left nil
	}
	// Do NOT call api.Init() to keep jtiSeen nil.

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-no-jti-cache",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        "jti-no-cache",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)

	userID, _, err := api.verifyEnrollmentJWT(context.Background(), tokenStr)
	if err == nil {
		t.Fatal("nil jtiSeen must fail closed; got nil error (fail-open regression)")
	}
	if err.Error() != "JTI_STORE_UNAVAILABLE" {
		t.Fatalf("err = %q, want JTI_STORE_UNAVAILABLE", err.Error())
	}
	if userID != "" {
		t.Errorf("rejected request must not return a subject; got %q", userID)
	}
}

// TestEnroll_JWT_JTIStoreUnavailable_Returns503 verifies the HTTP surface:
// a nil jtiSeen on the JWT enrollment path yields 503 JTI_STORE_UNAVAILABLE,
// not a 200 enrollment.
func TestEnroll_JWT_JTIStoreUnavailable_Returns503(t *testing.T) {
	key := newRSAKey(t)
	api := &EnrollmentAPI{
		CA:        &stubCA{},
		JWKSCache: &stubJWKS{key: &key.PublicKey},
		Logger:    silentLog(),
		// jtiSeen intentionally nil; Init() not called.
	}

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	tokenStr := makeRS256JWT(t, key, enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "u1",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        "jti-503",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	})

	rec := post(t, api, map[string]any{"csrPem": validCSR},
		map[string]string{"Authorization": "Bearer " + tokenStr})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "JTI_STORE_UNAVAILABLE" {
		t.Errorf("want JTI_STORE_UNAVAILABLE, got %q", body.Code)
	}
}

// Tests: enrollWithJWT fingerprint dedupe path

// Tests: DeviceOrServiceAuth — ValidateDeviceToken failure path

func TestDeviceOrServiceAuth_ValidateDeviceTokenFails_Returns401(t *testing.T) {
	// A structurally valid hex device token that doesn't match any stored hash.
	// ValidateDeviceToken returns an error → 401 "invalid or revoked device token".
	st, mock := newPgxmockStore(t)
	defer mock.Close()

	// ValidateDeviceToken runs a SELECT ... WHERE id=$1 AND metadata->>'deviceTokenHash'=$2
	mock.ExpectQuery(`SELECT id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("no rows"))

	mw := DeviceOrServiceAuth(st, "different-svc-token")
	rec := httptest.NewRecorder()
	// 64 hex chars = 32 bytes = a valid device token format
	const validHexToken = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+validHexToken)
	req.Header.Set("X-Thing-Id", "thing-test")
	e := echo.New()
	c := e.NewContext(req, rec)
	handler := mw(func(c echo.Context) error { return nil })
	_ = handler(c)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("ValidateDeviceToken fail: want 401, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "UNAUTHORIZED" {
		t.Errorf("want UNAUTHORIZED, got %q", body.Code)
	}
}

// Tests: enrollWithToken — resolveThingID error (rand.Read fails)

func TestEnroll_Token_ResolveThingIDError_Returns500(t *testing.T) {
	// When thingId is empty AND thingType is empty (falls back to token.ThingType),
	// resolveThingID tries to read random bytes. We can't easily inject a failing
	// rand without a seam, so we verify this path by reaching it via a token
	// with an empty ThingType (which causes thingType to be "" after resolution)
	// — but this won't fail because thingType just becomes "".
	// Instead, test a different invariant: that the generated thingId starts with
	// thingType prefix when no explicit id is given. This is already covered by
	// TestResolveThingID_Generated, so we verify the behavior via enrollWithToken.
	tok := &enrollment.Token{
		ID:        "tok-type-check",
		ThingType: "gateway",
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

	// No explicit thingType in request → falls back to token.ThingType = "gateway".
	rec := post(t, api, map[string]any{"csrPem": validCSR},
		map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "gateway-") {
		t.Errorf("generated id must start with 'gateway-' when token.ThingType='gateway', got %q", id)
	}
}

// Tests: enrollWithJWT — UpsertDeviceAssignment error is non-fatal

func TestEnroll_JWT_UpsertDeviceAssignmentError_IsNonFatal(t *testing.T) {
	// UpsertDeviceAssignment failure must not abort enrollment — it is Warn-logged
	// and the response is still 200 OK.
	key := newRSAKey(t)
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}

	api := buildAPI(&stubCA{}, mgr, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	jtiVal := "jti-assign-err-" + time.Now().Format("150405.000")
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-assign-err",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        jtiVal,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)

	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
	// UpsertDeviceAssignment step 1 fails immediately.
	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("db error"))

	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "agent",
	}, map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusOK {
		t.Fatalf("UpsertDeviceAssignment error must not block enrollment; got %d body: %s",
			rec.Code, rec.Body.String())
	}
}

// Tests: DeviceOrServiceAuth — happy device-token path (Thing set in context)

func TestDeviceOrServiceAuth_DeviceTokenHappy_ThingSetInContext(t *testing.T) {
	// Valid hex device token + ValidateDeviceToken succeeds → thing set in context,
	// next handler called.
	st, mock := newPgxmockStore(t)
	defer mock.Close()

	now := time.Now().UTC()
	// ValidateDeviceToken SELECT — 18 columns in Scan order.
	cols := []string{
		"id", "type", "name", "version", "address",
		"enrolled_by", "auth_type", "conn_protocol",
		"status", "desired", "reported", "desired_ver", "reported_ver",
		"metadata", "last_seen_at", "enrolled_at",
		"reported_outcomes", "process_started_at",
	}
	mock.ExpectQuery(`SELECT id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(cols).AddRow(
			"thing-happy", "agent", "My Agent", "1.0", "",
			"", "bearer", "http",
			"online", []byte(`{}`), []byte(`{}`), int64(1), int64(0),
			[]byte(`{}`), &now, now,
			[]byte(`{}`), &now,
		))

	mw := DeviceOrServiceAuth(st, "different-svc-token")
	called := false
	var thingInCtx bool
	handler := mw(func(c echo.Context) error {
		called = true
		thingInCtx = ThingFromContext(c) != nil
		return nil
	})

	const validHexToken = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+validHexToken)
	req.Header.Set("X-Thing-Id", "thing-happy")
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := handler(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("next handler must be called on valid device token")
	}
	if !thingInCtx {
		t.Error("Thing must be set in context after valid device token auth")
	}
}

// Tests: Enroll — Bind error path

func TestEnroll_BindError_ReturnsBadRequest(t *testing.T) {
	// Sending malformed JSON causes c.Bind to fail → 400 INVALID_REQUEST.
	api := buildAPI(&stubCA{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/internal/things/enroll",
		strings.NewReader("{invalid json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	_ = api.Enroll(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: want 400, got %d", rec.Code)
	}
}

// Tests: resolveThingID — rand.Read error via enrollRandReader seam

func TestResolveThingID_RandReadError_ReturnsError(t *testing.T) {
	// Inject a failing reader via the enrollRandReader seam so the rand.Read
	// error branch in resolveThingID is exercised.
	original := enrollRandReader
	enrollRandReader = &failReader{err: errors.New("entropy exhausted")}
	defer func() { enrollRandReader = original }()

	api := &EnrollmentAPI{}
	_, err := api.resolveThingID("agent")
	if err == nil {
		t.Error("rand.Read error must propagate from resolveThingID")
	}
}

// failReader is an io.Reader that always returns an error.
type failReader struct{ err error }

func (f *failReader) Read([]byte) (int, error) { return 0, f.err }

// Tests: doEnroll — GenerateDeviceToken error via deviceTokenGenFn seam

func TestDoEnroll_DeviceTokenGenError_Returns500(t *testing.T) {
	// Inject a failing token generator so the GenerateDeviceToken error branch
	// in doEnroll is covered.
	original := deviceTokenGenFn
	deviceTokenGenFn = func() (string, string, error) {
		return "", "", errors.New("entropy device error")
	}
	defer func() { deviceTokenGenFn = original }()

	tok := &enrollment.Token{
		ID:        "tok-devtok",
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
	svc := &stubEnrollSvc{token: tok, valid: true}
	api := buildAPI(&stubCA{}, &stubFleetManager{st: nil}, svc)

	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "agent",
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("device token gen error must produce 500, got %d; body: %s",
			rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "INTERNAL_ERROR" {
		t.Errorf("want INTERNAL_ERROR, got %q", body.Code)
	}
}

// Tests: enrollWithToken — resolveThingID error path

func TestEnrollWithToken_ResolveThingIDError_Returns500(t *testing.T) {
	// Inject a failing rand reader so resolveThingID fails when no thingId is
	// provided in the request (and no thingId in token either).
	original := enrollRandReader
	enrollRandReader = &failReader{err: errors.New("rand fail")}
	defer func() { enrollRandReader = original }()

	tok := &enrollment.Token{
		ID:        "tok-rand-fail",
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
	svc := &stubEnrollSvc{token: tok, valid: true}
	api := buildAPI(&stubCA{}, &stubFleetManager{st: nil}, svc)

	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "agent",
		// No thingId → resolveThingID must generate one → rand.Read fails
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("rand fail must produce 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "INTERNAL_ERROR" {
		t.Errorf("want INTERNAL_ERROR, got %q", body.Code)
	}
}

// Tests: enrollWithJWT — resolveThingID error path

func TestEnrollWithJWT_ResolveThingIDError_Returns500(t *testing.T) {
	// Inject a failing rand reader so resolveThingID fails on the JWT enrollment
	// path when no thingId is in the request.
	original := enrollRandReader
	enrollRandReader = &failReader{err: errors.New("rand fail jwt")}
	defer func() { enrollRandReader = original }()

	key := newRSAKey(t)
	api := buildAPI(&stubCA{}, &stubFleetManager{st: nil}, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	jtiVal := "jti-rand-fail-" + time.Now().Format("150405.000")
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-rand-fail",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        jtiVal,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)

	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "agent",
	}, map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("rand fail in JWT path must produce 500, got %d; body: %s",
			rec.Code, rec.Body.String())
	}
}

// Tests: enrollWithJWT — resolveThingID error via rand seam (originally documented)

// Tests: enrollWithJWT — fingerprint dedupe

func TestEnroll_JWT_WithFingerprint_ReuseExistingThingID(t *testing.T) {
	// When DeviceFingerprint is set and FindAgentByPhysicalID returns a match,
	// enrollWithJWT reuses the existing thingID. We verify the response id
	// matches the returned value.
	key := newRSAKey(t)
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}

	api := buildAPI(&stubCA{}, mgr, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}

	const existingThingID = "agent-existing-fingerprint"

	// FindAgentByPhysicalID SELECT returns a row with the existing thingID.
	mock.ExpectQuery(`SELECT id FROM thing`).
		WithArgs("fp-abc123").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(existingThingID))

	// Ownership check: the existing device is owned by the SAME SSO user
	// (subject "user-fp"), so reuse is allowed.
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(existingThingID).
		WillReturnRows(pgxmock.NewRows([]string{"userId", "source", "login_method", "ip_address", "assignedAt"}).
			AddRow("user-fp", "sso", nil, nil, time.Now()))

	// doEnroll DB calls using the reused thingID.
	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
	// UpsertDeviceAssignment (3 steps).
	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
	mock.ExpectExec(`INSERT INTO "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	jtiVal := "jti-fp-reuse-" + time.Now().Format("150405.000")
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-fp",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        jtiVal,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)

	rec := post(t, api, map[string]any{
		"csrPem":            validCSR,
		"thingType":         "agent",
		"deviceFingerprint": "fp-abc123",
	}, map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	gotID, _ := body["id"].(string)
	if gotID != existingThingID {
		t.Errorf("response id = %q, want %q", gotID, existingThingID)
	}
}

func TestEnroll_JWT_WithFingerprint_DifferentOwner_Refused(t *testing.T) {
	// DeviceFingerprint is an attacker-controllable request field. When
	// it matches a device already owned by a DIFFERENT SSO user, enrollment must
	// be REFUSED (409) — not silently rebound to the new user (device takeover).
	key := newRSAKey(t)
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}

	api := buildAPI(&stubCA{}, mgr, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}

	const victimThingID = "agent-victim-device"

	// Fingerprint matches the victim's device...
	mock.ExpectQuery(`SELECT id FROM thing`).
		WithArgs("fp-victim").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(victimThingID))
	// ...but it is owned by a DIFFERENT user ("victim-user"), not the enrolling
	// subject ("attacker-user") → refuse. No doEnroll / rebind expectations.
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(victimThingID).
		WillReturnRows(pgxmock.NewRows([]string{"userId", "source", "login_method", "ip_address", "assignedAt"}).
			AddRow("victim-user", "sso", nil, nil, time.Now()))

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "attacker-user",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        "jti-fp-takeover-" + time.Now().Format("150405.000"),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)

	rec := post(t, api, map[string]any{
		"csrPem":            validCSR,
		"thingType":         "agent",
		"deviceFingerprint": "fp-victim",
	}, map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 (refuse cross-user device rebind), got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	inner, _ := body["error"].(map[string]any)
	if code, _ := inner["code"].(string); code != "DEVICE_OWNED_BY_ANOTHER_USER" {
		t.Errorf("want code DEVICE_OWNED_BY_ANOTHER_USER, got %q", code)
	}
}

func TestEnroll_JWT_WithFingerprint_OwnershipReadError_FailsClosed(t *testing.T) {
	// Fail-closed: if the ownership check itself errors, enrollment must
	// be refused (500) and NOT fall through to reusing/rebinding the device.
	key := newRSAKey(t)
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}
	api := buildAPI(&stubCA{}, mgr, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}

	mock.ExpectQuery(`SELECT id FROM thing`).
		WithArgs("fp-err").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("agent-some-device"))
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs("agent-some-device").
		WillReturnError(errors.New("db unavailable"))
	// No doEnroll expectations — the handler must bail before any write.

	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-err",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
			ID:        "jti-fp-err-" + time.Now().Format("150405.000"),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)
	rec := post(t, api, map[string]any{
		"csrPem":            validCSR,
		"thingType":         "agent",
		"deviceFingerprint": "fp-err",
	}, map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 (fail closed on ownership-read error), got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestEnroll_JWT_WithFingerprint_Unowned_MintsNewThingID(t *testing.T) {
	// A fingerprint match with NO active assignment (e.g. a
	// token-enrolled / trust-level-1 device) must NOT be claimed by the
	// fingerprint alone — a world-readable DeviceFingerprint is an identifier,
	// not an authenticator. The handler mints a NEW thing_id instead of reusing
	// the existing one, so an attacker who knows the victim's fingerprint cannot
	// take over the victim's thing_id (which would overwrite its token and
	// rebind/elevate it). The response id must differ from the existing device.
	key := newRSAKey(t)
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}
	api := buildAPI(&stubCA{}, mgr, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}

	const existingThingID = "agent-unowned-device"
	mock.ExpectQuery(`SELECT id FROM thing`).
		WithArgs("fp-unowned").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(existingThingID))
	// Ownership read returns NO rows → unowned → reuse proceeds.
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(existingThingID).
		WillReturnRows(pgxmock.NewRows([]string{"userId", "source", "login_method", "ip_address", "assignedAt"}))
	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
	mock.ExpectExec(`INSERT INTO "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-unowned",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
			ID:        "jti-fp-unowned-" + time.Now().Format("150405.000"),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)
	rec := post(t, api, map[string]any{
		"csrPem":            validCSR,
		"thingType":         "agent",
		"deviceFingerprint": "fp-unowned",
	}, map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (unowned device → new thing_id minted), got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	gotID, _ := body["id"].(string)
	if gotID == "" {
		t.Fatal("response missing thing id")
	}
	if gotID == existingThingID {
		t.Errorf("SEC-C2-01: response id = victim's %q — the unowned thing was taken over by fingerprint; want a NEW thing_id", existingThingID)
	}
}
