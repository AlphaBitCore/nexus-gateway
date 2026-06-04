package sso

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	authcodestore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	cpiam "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	sharediam "github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/pkce"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildSigner generates a temporary in-memory keystore and signer for tests.
func buildSigner(t *testing.T) *token.Signer {
	t.Helper()
	dir := t.TempDir()
	ks, err := token.OpenKeystore(dir)
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return token.NewSigner(ks)
}

// buildSigner that always fails to sign (no keys).
func buildSignerNoKeys(t *testing.T) *token.Signer {
	t.Helper()
	dir := t.TempDir()
	ks, err := token.OpenKeystore(dir)
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	return token.NewSigner(ks)
}

// buildCodes returns an AuthCodeStore pre-seeded with one entry.
func buildCodes(t *testing.T, code string, entry authcodestore.AuthCodeEntry) *authcodestore.AuthCodeStore {
	t.Helper()
	s := authcodestore.NewAuthCodeStore(time.Minute)
	t.Cleanup(func() { s.Close() })
	s.Put(code, entry)
	return s
}

// postJSON fires a POST JSON request through the handler and returns the recorder.
func postJSON(t *testing.T, h *AgentEnrollHandler, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/agent/sso-enroll", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	_ = h.SSOEnroll(c)
	return rec
}

// validPKCE returns a (verifier, challenge) pair for S256.
func validPKCE(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = "test-verifier-abcdefghijklmnopqrstuvwxyz1234567"
	challenge = pkce.ChallengeS256(verifier)
	return
}

func buildHandler(t *testing.T, codes *authcodestore.AuthCodeStore, signer *token.Signer) *AgentEnrollHandler {
	t.Helper()
	h := &AgentEnrollHandler{
		AuthCodes: codes,
		Signer:    signer,
		Issuer:    "https://nexus.test",
		Logger:    silentLogger(),
	}
	return h
}

// allow / rate-limiter

func TestAllow_PermitsFirstN(t *testing.T) {
	h := &AgentEnrollHandler{Logger: silentLogger()}
	for i := range enrollMaxPerMin {
		ok, _ := h.allow("127.0.0.1")
		if !ok {
			t.Fatalf("allow[%d] returned false before limit", i)
		}
	}
}

func TestAllow_BlocksAfterLimit(t *testing.T) {
	h := &AgentEnrollHandler{Logger: silentLogger()}
	for range enrollMaxPerMin {
		h.allow("10.0.0.1") //nolint:errcheck
	}
	ok, retryAfter := h.allow("10.0.0.1")
	if ok {
		t.Fatal("allow returned true after limit")
	}
	if retryAfter < 1 {
		t.Errorf("retryAfter=%d want ≥1", retryAfter)
	}
}

func TestAllow_SeparateIPsBucketsAreIndependent(t *testing.T) {
	h := &AgentEnrollHandler{Logger: silentLogger()}
	for range enrollMaxPerMin {
		h.allow("192.168.0.1") //nolint:errcheck
	}
	ok, _ := h.allow("192.168.0.2")
	if !ok {
		t.Fatal("different IP should not be rate-limited by another IP's bucket")
	}
}

// SSOEnroll happy path

func TestSSOEnroll_HappyPath(t *testing.T) {
	verifier, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "code-1", authcodestore.AuthCodeEntry{
		UserID:        "usr-1",
		Email:         "alice@example.com",
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSigner(t))

	b, _ := json.Marshal(map[string]any{
		"code":          "code-1",
		"code_verifier": verifier,
		"redirect_uri":  redirectURI,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/agent/sso-enroll", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.SSOEnroll(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := resp["enrollment_jwt"]; !ok {
		t.Error("response missing enrollment_jwt")
	}
	if resp["user_email"] != "alice@example.com" {
		t.Errorf("user_email=%v want alice@example.com", resp["user_email"])
	}
}

// SSOEnroll failure modes

func TestSSOEnroll_RateLimited(t *testing.T) {
	codes := buildCodes(t, "x", authcodestore.AuthCodeEntry{ExpiresAt: time.Now().Add(time.Minute)})
	h := buildHandler(t, codes, buildSigner(t))
	// exhaust the bucket for this IP
	for range enrollMaxPerMin {
		h.allow("1.2.3.4") //nolint:errcheck
	}
	b, _ := json.Marshal(map[string]any{"code": "x", "code_verifier": "v", "redirect_uri": "u"})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "1.2.3.4:1234"
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	_ = h.SSOEnroll(c)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status=%d want 429", rec.Code)
	}
}

func TestSSOEnroll_MissingCode(t *testing.T) {
	h := buildHandler(t, buildCodes(t, "c1", authcodestore.AuthCodeEntry{ExpiresAt: time.Now().Add(time.Minute)}), buildSigner(t))
	rec := postJSON(t, h, map[string]any{"code": "", "code_verifier": "v", "redirect_uri": "u"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestSSOEnroll_InvalidCode(t *testing.T) {
	h := buildHandler(t, buildCodes(t, "real-code", authcodestore.AuthCodeEntry{ExpiresAt: time.Now().Add(time.Minute), PKCEChallenge: "x", RedirectURI: "u"}), buildSigner(t))
	rec := postJSON(t, h, map[string]any{"code": "wrong-code", "code_verifier": "v", "redirect_uri": "u"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400; body=%s", rec.Code, rec.Body)
	}
}

func TestSSOEnroll_RedirectURIMismatch(t *testing.T) {
	verifier, challenge := validPKCE(t)
	codes := buildCodes(t, "c2", authcodestore.AuthCodeEntry{
		PKCEChallenge: challenge,
		RedirectURI:   "nexus://expected",
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSigner(t))
	rec := postJSON(t, h, map[string]any{
		"code": "c2", "code_verifier": verifier, "redirect_uri": "nexus://wrong",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestSSOEnroll_PKCEVerifierMismatch(t *testing.T) {
	_, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "c3", authcodestore.AuthCodeEntry{
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSigner(t))
	rec := postJSON(t, h, map[string]any{
		"code": "c3", "code_verifier": "bad-verifier", "redirect_uri": redirectURI,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestSSOEnroll_ExpiredCode(t *testing.T) {
	verifier, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "c4", authcodestore.AuthCodeEntry{
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(-time.Second), // already expired
	})
	h := buildHandler(t, codes, buildSigner(t))
	rec := postJSON(t, h, map[string]any{
		"code": "c4", "code_verifier": verifier, "redirect_uri": redirectURI,
	})
	// expired entries are removed by Get() → treated as missing → 400
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (expired code)", rec.Code)
	}
}

func TestSSOEnroll_SignerNoKeys_Returns500(t *testing.T) {
	verifier, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "c5", authcodestore.AuthCodeEntry{
		UserID:        "usr-5",
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSignerNoKeys(t))
	rec := postJSON(t, h, map[string]any{
		"code": "c5", "code_verifier": verifier, "redirect_uri": redirectURI,
	})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 (no signing keys)", rec.Code)
	}
}

// IAM gate

// stubPolicyLoader implements cpiam.PolicyLoader for tests.
type stubPolicyLoader struct {
	policies []cpiam.LoadedPolicy
}

func (s *stubPolicyLoader) LoadPolicies(_ context.Context, _, _ string) ([]cpiam.LoadedPolicy, error) {
	return s.policies, nil
}

func buildIAMEngine(t *testing.T, allow bool) *cpiam.Engine {
	t.Helper()
	// Build a real IAM engine with a stub loader that either allows or denies.
	var policies []cpiam.LoadedPolicy
	if allow {
		policies = []cpiam.LoadedPolicy{{
			ID:   "pol-1",
			Name: "allow-enroll",
			Document: cpiam.PolicyDocument{
				Version: cpiam.PolicyVersion,
				Statement: []cpiam.Statement{{
					Effect:   "Allow",
					Action:   []string{sharediam.ResourceDeviceEnrollment.Action(sharediam.VerbEnroll)},
					Resource: []string{"nrn:nexus:*:*:*/*"},
				}},
			},
			Source: "direct",
		}}
	}
	loader := &stubPolicyLoader{policies: policies}
	return cpiam.NewEngine(loader, silentLogger())
}

func TestSSOEnroll_IAMDenied_Returns403(t *testing.T) {
	verifier, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "c6", authcodestore.AuthCodeEntry{
		UserID:        "usr-6",
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSigner(t))
	h.IAM = buildIAMEngine(t, false) // deny-all engine

	rec := postJSON(t, h, map[string]any{
		"code": "c6", "code_verifier": verifier, "redirect_uri": redirectURI,
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403 (IAM denied)", rec.Code)
	}
}

func TestSSOEnroll_IAMAllowed_Returns200(t *testing.T) {
	verifier, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "c7", authcodestore.AuthCodeEntry{
		UserID:        "usr-7",
		Email:         "bob@example.com",
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSigner(t))
	h.IAM = buildIAMEngine(t, true)

	rec := postJSON(t, h, map[string]any{
		"code": "c7", "code_verifier": verifier, "redirect_uri": redirectURI,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// mtls-only enrollment mode

// stubMeta satisfies systemMetaReader for testing the mtls-only mode check.
type stubMeta struct {
	values map[string]json.RawMessage
}

func (s *stubMeta) GetSystemMetadata(_ context.Context, key string) (json.RawMessage, error) {
	if v, ok := s.values[key]; ok {
		return v, nil
	}
	return nil, nil
}

func TestSSOEnroll_MtlsOnlyMode_Returns400(t *testing.T) {
	verifier, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "c-mtls", authcodestore.AuthCodeEntry{
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSigner(t))
	modeBytes, _ := json.Marshal("mtls-only")
	h.metaReader = &stubMeta{values: map[string]json.RawMessage{"device.auth.mode": modeBytes}}

	rec := postJSON(t, h, map[string]any{
		"code": "c-mtls", "code_verifier": verifier, "redirect_uri": redirectURI,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (mtls-only mode)", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "enrollment_mode_mtls" {
		t.Errorf("error=%q want enrollment_mode_mtls", body["error"])
	}
}

func TestSSOEnroll_MetaMode_OtherValue_Allows(t *testing.T) {
	verifier, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "c-meta-other", authcodestore.AuthCodeEntry{
		UserID:        "usr-meta",
		Email:         "meta@example.com",
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSigner(t))
	modeBytes, _ := json.Marshal("sso-login")
	h.metaReader = &stubMeta{values: map[string]json.RawMessage{"device.auth.mode": modeBytes}}

	rec := postJSON(t, h, map[string]any{
		"code": "c-meta-other", "code_verifier": verifier, "redirect_uri": redirectURI,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200 (non-mtls mode should allow); body=%s", rec.Code, rec.Body)
	}
}

// stubUserChecker satisfies enrollUserChecker for testing the user-active check.
type stubUserChecker struct {
	err        error
	disabledAt *time.Time
}

func (s *stubUserChecker) GetByID(_ context.Context, _ string) (*authcodestore.User, error) {
	if s.err != nil {
		return nil, s.err
	}
	u := &authcodestore.User{DisabledAt: s.disabledAt}
	return u, nil
}

func TestSSOEnroll_UserLookupError_Returns400(t *testing.T) {
	verifier, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "c-user-err", authcodestore.AuthCodeEntry{
		UserID:        "usr-err",
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSigner(t))
	h.userChecker = &stubUserChecker{err: context.DeadlineExceeded}

	rec := postJSON(t, h, map[string]any{
		"code": "c-user-err", "code_verifier": verifier, "redirect_uri": redirectURI,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (user lookup error)", rec.Code)
	}
}

func TestSSOEnroll_DisabledUser_Returns403(t *testing.T) {
	verifier, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "c-disabled", authcodestore.AuthCodeEntry{
		UserID:        "usr-disabled",
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSigner(t))
	disabledAt := time.Now().Add(-time.Hour)
	h.userChecker = &stubUserChecker{disabledAt: &disabledAt}

	rec := postJSON(t, h, map[string]any{
		"code": "c-disabled", "code_verifier": verifier, "redirect_uri": redirectURI,
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403 (user disabled)", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "user_disabled" {
		t.Errorf("error=%q want user_disabled", body["error"])
	}
}

func TestSSOEnroll_ActiveUser_Returns200(t *testing.T) {
	verifier, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "c-active", authcodestore.AuthCodeEntry{
		UserID:        "usr-active",
		Email:         "active@example.com",
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSigner(t))
	h.userChecker = &stubUserChecker{} // nil DisabledAt means active

	rec := postJSON(t, h, map[string]any{
		"code": "c-active", "code_verifier": verifier, "redirect_uri": redirectURI,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200 (active user); body=%s", rec.Code, rec.Body)
	}
}

// gc / Init / Close lifecycle

func TestInitAndClose_Idempotent(t *testing.T) {
	h := &AgentEnrollHandler{Logger: silentLogger()}
	h.Init()
	h.Init() // double-init must be no-op
	h.Close()
	h.Close() // double-close must be no-op
}

func TestGC_RemovesExpiredBuckets(t *testing.T) {
	h := &AgentEnrollHandler{Logger: silentLogger()}
	// Inject a bucket that is already expired.
	h.rateMu.Lock()
	h.rateBkt = map[string]*rateBucket{
		"expired-ip": {count: 5, resetAt: time.Now().Add(-time.Minute)},
		"live-ip":    {count: 5, resetAt: time.Now().Add(time.Minute)},
	}
	h.rateMu.Unlock()

	h.gc()

	h.rateMu.Lock()
	_, expiredStillPresent := h.rateBkt["expired-ip"]
	_, livePresent := h.rateBkt["live-ip"]
	h.rateMu.Unlock()

	if expiredStillPresent {
		t.Error("gc did not remove expired bucket")
	}
	if !livePresent {
		t.Error("gc removed a live bucket")
	}
}

func TestClose_WhenNotInitialized_IsNoOp(t *testing.T) {
	// Close on an uninitialized handler (stopCh == nil) must not panic.
	h := &AgentEnrollHandler{Logger: silentLogger()}
	h.Close() // must not panic
}

func TestGCLoop_ExitsOnClose(t *testing.T) {
	h := &AgentEnrollHandler{Logger: silentLogger()}
	h.Init()
	// Let the gc goroutine run briefly then stop.
	time.Sleep(10 * time.Millisecond)
	h.Close()
}

// SSOEnroll — IAM engine error path

// failingPolicyLoader always returns an error so the IAM engine propagates it.
type failingPolicyLoader struct{}

func (f *failingPolicyLoader) LoadPolicies(_ context.Context, _, _ string) ([]cpiam.LoadedPolicy, error) {
	return nil, context.DeadlineExceeded
}

func TestSSOEnroll_IAMEngineError_Returns500(t *testing.T) {
	verifier, challenge := validPKCE(t)
	redirectURI := "nexus://enroll"
	codes := buildCodes(t, "c-iam-err", authcodestore.AuthCodeEntry{
		UserID:        "usr-iam-err",
		PKCEChallenge: challenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	h := buildHandler(t, codes, buildSigner(t))
	h.IAM = cpiam.NewEngine(&failingPolicyLoader{}, silentLogger())

	rec := postJSON(t, h, map[string]any{
		"code": "c-iam-err", "code_verifier": verifier, "redirect_uri": redirectURI,
	})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 (IAM engine error)", rec.Code)
	}
}

func TestNewEnrollJTI_Unique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := range 100 {
		j := newEnrollJTI()
		if j == "" {
			t.Fatal("newEnrollJTI returned empty string")
		}
		if seen[j] {
			t.Fatalf("duplicate JTI %q at iteration %d", j, i)
		}
		seen[j] = true
	}
}

// RSA key generation sanity

func TestSignerRejectsEmptyKeystore(t *testing.T) {
	dir := t.TempDir()
	ks, _ := token.OpenKeystore(dir)
	signer := token.NewSigner(ks)
	// fabricate minimal claims
	claims := enrollmentClaims{Purpose: "test"}
	_, err := signer.Sign(claims)
	if err == nil {
		t.Fatal("expected error when keystore has no keys")
	}
}

func TestSignerWithKey_ReturnsNonEmptyJWT(t *testing.T) {
	dir := t.TempDir()
	ks, _ := token.OpenKeystore(dir)
	_, _ = ks.Generate()
	signer := token.NewSigner(ks)
	claims := enrollmentClaims{Purpose: enrollPurpose}
	jwt, err := signer.Sign(claims)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if jwt == "" {
		t.Fatal("expected non-empty JWT")
	}
}

// Ensure the test file itself compiles even without the rsa import.
var _ = rsa.GenerateKey
var _ = os.TempDir
var _ = rand.Read
