package login_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/idp"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/login"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// fakeUserLookup is a deterministic UserLookup that returns canned values
// without touching the database. Production code wires a *store.UserStore
// (which talks to PG); tests just need the contract.
type fakeUserLookup struct {
	userID     string
	pwdHash    string
	source     string // "local" | "oidc" | "scim"; empty defaults to "local"
	disabledAt *time.Time
	err        error
}

func (f *fakeUserLookup) GetByEmail(_ context.Context, _ string) (string, string, string, *time.Time, error) {
	source := f.source
	if source == "" {
		source = "local" // mirror the NexusUser.source DB default
	}
	return f.userID, f.pwdHash, source, f.disabledAt, f.err
}

// hashOrFail is a tiny helper so test setup doesn't keep paying the bcrypt
// cost in every single sub-test.
func hashOrFail(t *testing.T, plain string) string {
	t.Helper()
	h, err := auth.HashPassword(plain)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

// newPasswordMockDeps wires the Local IdP + Pending + AuthCodes stores
// around a fake UserLookup so the handler can be driven through every
// branch in isolation.
type passwordMockFixture struct {
	deps     login.PasswordDeps
	authctx  string
	state    string
	redirect string
	userID   string
	email    string
}

func newPasswordMockFixture(t *testing.T, lookup *fakeUserLookup) passwordMockFixture {
	t.Helper()
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	authCodes := store.NewAuthCodeStore(5 * time.Minute)
	t.Cleanup(authCodes.Close)

	authctx := "ctx-" + time.Now().Format("150405.000000000")
	state := "st-abc"
	redirect := "http://127.0.0.1:9999/cb"
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID:      "cli-1",
		RedirectURI:   redirect,
		Scope:         "openid",
		State:         state,
		Nonce:         "nonce-x",
		CodeChallenge: "cc-x",
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	})

	return passwordMockFixture{
		deps: login.PasswordDeps{
			Local:     idp.NewLocal(lookup, "idp-local-1"),
			Pending:   pending,
			AuthCodes: authCodes,
			Limiter:   login.NewLimiter(),
		},
		authctx:  authctx,
		state:    state,
		redirect: redirect,
		userID:   lookup.userID,
		email:    "alice@example.com",
	}
}

func mockPostJSON(body any) (echo.Context, *httptest.ResponseRecorder) {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/authserver/password", strings.NewReader(string(raw)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

// TestPasswordHandler_Mock_Success asserts: 200 OK; redirect URI keeps the
// scheme/host/path of the registered callback (no open-redirect injection
// path); state echoed back verbatim; an auth code is minted and stored;
// pending entry is consumed (single-use); AMR=pwd is stamped onto the
// auth code so the token endpoint can include it in the ID token.
func TestPasswordHandler_Mock_Success(t *testing.T) {
	hash := hashOrFail(t, "hunter2")
	lookup := &fakeUserLookup{userID: "user-1", pwdHash: hash}
	fx := newPasswordMockFixture(t, lookup)

	c, rec := mockPostJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    "Alice@Example.com", // case insensitivity should still resolve
		Password: "hunter2",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	var resp login.PasswordSubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !strings.HasPrefix(resp.RedirectURI, fx.redirect+"?") {
		t.Fatalf("redirect prefix mutated: got %q, want prefix %q?", resp.RedirectURI, fx.redirect)
	}
	if !strings.Contains(resp.RedirectURI, "state="+fx.state) {
		t.Fatalf("state missing from redirect: %q", resp.RedirectURI)
	}
	if !strings.Contains(resp.RedirectURI, "code=") {
		t.Fatalf("code missing from redirect: %q", resp.RedirectURI)
	}
	// Extract code, assert auth code entry is single-use.
	idx := strings.Index(resp.RedirectURI, "code=")
	codePart := resp.RedirectURI[idx+5:]
	if amp := strings.Index(codePart, "&"); amp >= 0 {
		codePart = codePart[:amp]
	}
	entry, ok := fx.deps.AuthCodes.Get(codePart)
	if !ok {
		t.Fatal("auth code not stored")
	}
	if entry.UserID != "user-1" {
		t.Fatalf("entry user: got %q, want user-1", entry.UserID)
	}
	if entry.IdPID != "idp-local-1" {
		t.Fatalf("entry idp: got %q, want idp-local-1", entry.IdPID)
	}
	if len(entry.AMR) != 1 || entry.AMR[0] != "pwd" {
		t.Fatalf("AMR: got %v, want [pwd]", entry.AMR)
	}
	if entry.Nonce != "nonce-x" {
		t.Fatalf("nonce: got %q, want nonce-x", entry.Nonce)
	}
	if entry.PKCEChallenge != "cc-x" {
		t.Fatalf("pkce: got %q, want cc-x", entry.PKCEChallenge)
	}

	// Pending must have been consumed (single-use).
	if _, ok := fx.deps.Pending.Take(fx.authctx); ok {
		t.Fatal("pending entry should be consumed after success")
	}
	// Auth code is single-use: a second Get returns false.
	if _, ok := fx.deps.AuthCodes.Get(codePart); ok {
		t.Fatal("auth code returned twice (single-use invariant broken)")
	}
}

// TestPasswordHandler_Mock_BindFailure feeds a malformed JSON body and
// confirms the handler rejects with 400/authctx_expired and never calls
// the limiter or Local.Authenticate.
func TestPasswordHandler_Mock_BindFailure(t *testing.T) {
	fx := newPasswordMockFixture(t, &fakeUserLookup{userID: "user-1"})
	lim := &countingLimiter{}
	fx.deps.Limiter = lim

	req := httptest.NewRequest(http.MethodPost, "/authserver/password",
		strings.NewReader(`{"authctx": 12345 not-json}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "authctx_expired" {
		t.Fatalf("error: got %q, want authctx_expired", body["error"])
	}
	if lim.calls != 0 {
		t.Fatalf("limiter must not run before bind succeeds (calls=%d)", lim.calls)
	}
}

type countingLimiter struct {
	calls int
	allow bool
}

func (c *countingLimiter) Allow(_, _ string) bool {
	c.calls++
	return c.allow
}

// TestPasswordHandler_Mock_RateLimitedBeforeAuth verifies the limiter is
// the FIRST gate: even with correct credentials, a denied limiter call
// returns 429 and Local.Authenticate is never invoked. Authenticate
// running would burn scrypt time the attacker doesn't need to pay for —
// the limit MUST kick in before the password check.
func TestPasswordHandler_Mock_RateLimitedBeforeAuth(t *testing.T) {
	lookup := &fakeUserLookup{userID: "user-1", pwdHash: hashOrFail(t, "hunter2")}
	fx := newPasswordMockFixture(t, lookup)
	fx.deps.Limiter = &countingLimiter{allow: false}

	c, rec := mockPostJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    "alice@example.com",
		Password: "hunter2",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status: got %d, want 429", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "rate_limited" {
		t.Fatalf("error: got %q, want rate_limited", body["error"])
	}
	// Pending entry must NOT be consumed by a rate-limited attempt.
	if _, ok := fx.deps.Pending.Take(fx.authctx); !ok {
		t.Fatal("pending entry consumed by rate-limited attempt — would let attacker burn one valid authctx per IP")
	}
}

// TestPasswordHandler_Mock_InvalidPassword asserts: a wrong password
// → 401 invalid_credentials, no auth code is stored, the pending entry
// survives for retry.
func TestPasswordHandler_Mock_InvalidPassword(t *testing.T) {
	hash := hashOrFail(t, "real-pw")
	lookup := &fakeUserLookup{userID: "user-1", pwdHash: hash}
	fx := newPasswordMockFixture(t, lookup)

	c, rec := mockPostJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    "alice@example.com",
		Password: "totally-wrong",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "invalid_credentials" {
		t.Fatalf("error: got %q, want invalid_credentials", body["error"])
	}
	// Pending survives — user may have mis-typed.
	if _, ok := fx.deps.Pending.Take(fx.authctx); !ok {
		t.Fatal("pending entry consumed by failed login — would break retry UX")
	}
}

// TestPasswordHandler_Mock_UserMissingMapsToInvalidCreds asserts the
// timing-safety invariant: an unknown email returns the SAME 401
// invalid_credentials code as a wrong password. If the response
// distinguished them, attackers could enumerate registered emails.
func TestPasswordHandler_Mock_UserMissingMapsToInvalidCreds(t *testing.T) {
	lookup := &fakeUserLookup{err: errors.New("user not found")}
	fx := newPasswordMockFixture(t, lookup)

	c, rec := mockPostJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    "no-such-user@example.com",
		Password: "hunter2",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "invalid_credentials" {
		t.Fatalf("missing-user must map to invalid_credentials; got %q", body["error"])
	}
}

// TestPasswordHandler_Mock_UserDisabledMapsToUserDisabled distinguishes a
// disabled-account response from invalid_credentials so support staff
// can triage failures, but only AFTER the user already passed the
// timing-uniform path inside idp.Local.
func TestPasswordHandler_Mock_UserDisabledMapsToUserDisabled(t *testing.T) {
	hash := hashOrFail(t, "hunter2")
	disabled := time.Now().Add(-time.Hour)
	lookup := &fakeUserLookup{userID: "user-disabled", pwdHash: hash, disabledAt: &disabled}
	fx := newPasswordMockFixture(t, lookup)

	c, rec := mockPostJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    "alice@example.com",
		Password: "hunter2",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "user_disabled" {
		t.Fatalf("error: got %q, want user_disabled", body["error"])
	}
}

// TestPasswordHandler_Mock_ExpiredAuthctx covers the case where the
// password verifies but Pending.Take fails (the authctx expired between
// the time the SPA loaded the login page and the time the user typed
// the password). 400/authctx_expired forces the SPA to restart the
// flow.
func TestPasswordHandler_Mock_ExpiredAuthctx(t *testing.T) {
	hash := hashOrFail(t, "hunter2")
	lookup := &fakeUserLookup{userID: "user-1", pwdHash: hash}
	fx := newPasswordMockFixture(t, lookup)

	c, rec := mockPostJSON(login.PasswordSubmitRequest{
		AuthCtx:  "never-existed",
		Email:    "alice@example.com",
		Password: "hunter2",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "authctx_expired" {
		t.Fatalf("error: got %q, want authctx_expired", body["error"])
	}
}

// TestPasswordHandler_Mock_RedirectURIInvalid covers the
// `buildRedirect → url.Parse error` path. Production code never seeds an
// invalid registered URI (validated at OAuth-client registration), but
// the handler must still fail closed rather than crashing or returning
// a garbled redirect.
func TestPasswordHandler_Mock_RedirectURIInvalid(t *testing.T) {
	hash := hashOrFail(t, "hunter2")
	lookup := &fakeUserLookup{userID: "user-1", pwdHash: hash}
	fx := newPasswordMockFixture(t, lookup)

	// Overwrite pending with a registered URI that url.Parse rejects.
	bad := "://not-a-url"
	fx.deps.Pending.Put(fx.authctx, store.PendingAuthzEntry{
		ClientID:    "cli-1",
		RedirectURI: bad,
		Scope:       "openid",
		State:       fx.state,
		ExpiresAt:   time.Now().Add(5 * time.Minute),
	})

	c, rec := mockPostJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    "alice@example.com",
		Password: "hunter2",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500 (got body=%q)", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "internal_error" {
		t.Fatalf("error: got %q, want internal_error", body["error"])
	}
}

// TestPasswordHandler_Mock_AuditNilWriter_NoPanic verifies the
// `d.Audit == nil` branch in both failure and success paths: nothing
// should panic, no audit row should be emitted, the response must still
// be correct.
func TestPasswordHandler_Mock_AuditNilWriter_NoPanic(t *testing.T) {
	hash := hashOrFail(t, "hunter2")
	lookup := &fakeUserLookup{userID: "user-1", pwdHash: hash}
	fx := newPasswordMockFixture(t, lookup)
	fx.deps.Audit = nil // explicit

	c, rec := mockPostJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    "alice@example.com",
		Password: "wrong",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// capturingProducer collects every Enqueue payload so audit-emission
// assertions can inspect the marshaled AdminAuditMessage. Publish is
// unused by audit.Writer but defined to satisfy mq.Producer.
type capturingProducer struct {
	mu   sync.Mutex
	msgs [][]byte
}

func (p *capturingProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (p *capturingProducer) Enqueue(_ context.Context, _ string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	p.msgs = append(p.msgs, cp)
	return nil
}
func (p *capturingProducer) Close() error { return nil }
func (p *capturingProducer) snapshot() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.msgs))
	copy(out, p.msgs)
	return out
}

// waitForAuditMsg polls the producer's queue until it sees at least n
// messages or the deadline expires. audit.Writer.Log is invoked
// synchronously from the handler, but the test must still avoid a race
// window between handler return and ResponseWriter close.
func waitForAuditMsg(t *testing.T, p *capturingProducer, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs := p.snapshot()
		if len(msgs) >= n {
			return msgs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("audit producer never saw %d messages (got %d)", n, len(p.snapshot()))
	return nil
}

// TestPasswordHandler_Mock_AuditOnFailure asserts an `admin.login.failed`
// row is published — this is the row brute-force alerting fires on. The
// ActorLabel MUST be the email as typed (NOT canonicalized) so attackers
// can't bypass alerts by changing case across attempts; binding via the
// auth.login_failure_rate alert rule.
func TestPasswordHandler_Mock_AuditOnFailure(t *testing.T) {
	prod := &capturingProducer{}
	w := audit.NewWriter(prod, "admin-audit", nil)

	lookup := &fakeUserLookup{err: errors.New("nope")} // unknown user → ErrInvalidCredentials
	fx := newPasswordMockFixture(t, lookup)
	fx.deps.Audit = w

	c, rec := mockPostJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    "Attacker@Corp.com",
		Password: "wrong",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}

	msgs := waitForAuditMsg(t, prod, 1)
	var ev map[string]any
	if err := json.Unmarshal(msgs[0], &ev); err != nil {
		t.Fatalf("decode audit msg: %v", err)
	}
	if ev["action"] != "admin.login.failed" {
		t.Fatalf("action: got %v, want admin.login.failed", ev["action"])
	}
	// ActorLabel preserves the as-typed casing — verified by inspecting
	// the marshaled message rather than asserting on logger output.
	if ev["actorLabel"] != "Attacker@Corp.com" {
		t.Fatalf("actorLabel: got %v, want Attacker@Corp.com (case-preserving)", ev["actorLabel"])
	}
}

// TestPasswordHandler_Mock_AuditOnSuccess asserts the success-side row
// is published with the resolved (canonicalized) email and the user id
// stamped as both ActorID and EntityID. The success row is mandatory
// for symmetry — a failure-only audit stream would lose the audit
// reference point for a future SLO ("successful logins per hour").
func TestPasswordHandler_Mock_AuditOnSuccess(t *testing.T) {
	prod := &capturingProducer{}
	w := audit.NewWriter(prod, "admin-audit", nil)
	hash := hashOrFail(t, "hunter2")
	lookup := &fakeUserLookup{userID: "user-real", pwdHash: hash}
	fx := newPasswordMockFixture(t, lookup)
	fx.deps.Audit = w

	c, rec := mockPostJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    "alice@example.com",
		Password: "hunter2",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	msgs := waitForAuditMsg(t, prod, 1)
	var ev map[string]any
	if err := json.Unmarshal(msgs[0], &ev); err != nil {
		t.Fatalf("decode audit msg: %v", err)
	}
	if ev["action"] != "admin.login.succeeded" {
		t.Fatalf("action: got %v, want admin.login.succeeded", ev["action"])
	}
	if ev["actorId"] != "user-real" {
		t.Fatalf("actorId: got %v, want user-real", ev["actorId"])
	}
	if ev["entityId"] != "user-real" {
		t.Fatalf("entityId: got %v, want user-real", ev["entityId"])
	}
	if ev["entityType"] != "user" {
		t.Fatalf("entityType: got %v, want user", ev["entityType"])
	}
}
