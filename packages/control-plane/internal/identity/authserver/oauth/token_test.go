package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/oauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	cpstore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// fakeRefresh records NewChain/Rotate calls and returns configured outputs.
// Each method's behaviour is driven by the fake's exported fields so tests
// can describe desired behaviour in a single struct literal.
type fakeRefresh struct {
	// NewChain configuration.
	nextRefresh string
	nextSID     string
	nextJTI     string
	newErr      error

	// Rotate configuration. Rotate pops the first entry from rotateReturns;
	// when exhausted it falls back to rotateErr.
	rotateReturns []rotateResult
	rotateErr     error

	// Call recorders.
	newCalls    int32
	rotateCalls int32
	lastNewArgs struct {
		UserID, ClientID, DeviceID string
		TTL                        time.Duration
	}
	lastRotateInput string
}

type rotateResult struct {
	newToken string
	newJTI   string
	parent   *store.RefreshTokenRow
	err      error
}

func (f *fakeRefresh) NewChain(_ context.Context, userID, clientID, deviceID string, ttl time.Duration) (string, string, string, error) {
	atomic.AddInt32(&f.newCalls, 1)
	f.lastNewArgs.UserID = userID
	f.lastNewArgs.ClientID = clientID
	f.lastNewArgs.DeviceID = deviceID
	f.lastNewArgs.TTL = ttl
	if f.newErr != nil {
		return "", "", "", f.newErr
	}
	return f.nextRefresh, f.nextSID, f.nextJTI, nil
}

func (f *fakeRefresh) Rotate(_ context.Context, incoming string, _ time.Duration) (string, string, *store.RefreshTokenRow, error) {
	atomic.AddInt32(&f.rotateCalls, 1)
	f.lastRotateInput = incoming
	if len(f.rotateReturns) > 0 {
		r := f.rotateReturns[0]
		f.rotateReturns = f.rotateReturns[1:]
		return r.newToken, r.newJTI, r.parent, r.err
	}
	if f.rotateErr != nil {
		return "", "", nil, f.rotateErr
	}
	return "", "", nil, errors.New("fakeRefresh: no rotateReturns configured")
}

// fakeUsers returns a pre-built user by id; missing ids yield ErrUserNotFound.
type fakeUsers map[string]*store.User

func (f fakeUsers) TouchLastLogin(_ context.Context, _ string) error { return nil }

func (f fakeUsers) GetByID(_ context.Context, id string) (*store.User, error) {
	u, ok := f[id]
	if !ok {
		return nil, store.ErrUserNotFound
	}
	return u, nil
}

// defaultTokenTestClients is the shared fakeClients registration used by the
// token-handler test suite. Every client_id the suite invokes is registered
// here as a public client with PKCE; tests that need a confidential client
// replace the fixture's clients field before building the handler.
func defaultTokenTestClients() fakeClients {
	return fakeClients{
		"test-client":     {ID: "test-client", Type: "public"},
		"web-console":     {ID: "web-console", Type: "public"},
		"attacker-client": {ID: "attacker-client", Type: "public"},
		"agent-desktop":   {ID: "agent-desktop", Type: "public"},
		"other-client":    {ID: "other-client", Type: "public"},
		"cp-ui":           {ID: "cp-ui", Type: "public"},
		"any":             {ID: "any", Type: "public"},
		"refresh-test":    {ID: "refresh-test", Type: "public"},
		"confidential-c":  {ID: "confidential-c", Type: "confidential"},
	}
}

// tokFixture bundles the collaborators token-handler tests need. Each test
// builds its own fixture so AuthCodeStore state never leaks between cases.
type tokFixture struct {
	t        *testing.T
	codes    *store.AuthCodeStore
	refresh  *fakeRefresh
	signer   *token.Signer
	keystore *token.Keystore
	clients  fakeClients
	deps     oauth.TokenDeps
	echo     *echo.Echo
}

// newTokenFixture constructs the fixture with a fresh in-memory AuthCodeStore,
// a fake refresh helper, and a Signer backed by a single-key temp keystore.
func newTokenFixture(t *testing.T) *tokFixture {
	t.Helper()
	codes := store.NewAuthCodeStore(time.Minute)
	t.Cleanup(codes.Close)

	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	fr := &fakeRefresh{
		nextRefresh: "rt-new",
		nextSID:     "sid-new",
		nextJTI:     "rjti-new",
	}

	return &tokFixture{
		t:        t,
		codes:    codes,
		refresh:  fr,
		signer:   token.NewSigner(ks),
		keystore: ks,
	}
}

// buildHandler returns an Echo with the token handler mounted, using the
// fixture's concrete stores and an in-memory user loader. Callers mutate
// fixture fields before calling this.
//
// The default Clients fake registers every client_id used across the
// existing test suite as a public client with PKCE — that mirrors the
// pre-client-secret-auth contract these tests were written against. Tests
// that need a confidential client (e.g. covering the new verifyClientAuth
// paths) replace f.clients before calling buildHandler.
func (f *tokFixture) buildHandler(users fakeUsers) *echo.Echo {
	if f.clients == nil {
		f.clients = defaultTokenTestClients()
	}
	f.deps = oauth.TokenDeps{
		Issuer:    "https://cp.nexus.ai",
		Clients:   f.clients,
		AuthCodes: f.codes,
		Users:     users,
		Refresh:   f.refresh,
		Signer:    f.signer,
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e
	return e
}

// post issues an x-www-form-urlencoded POST to /oauth/token. When device is
// non-nil it is stashed in the Echo context under the key the mTLS middleware
// writes, so agent-desktop paths see a peer cert without a real TLS listener.
func (f *tokFixture) post(form url.Values, device *cpstore.ThingNodeInfo) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	c := f.echo.NewContext(req, rec)
	setDevice(c, device)
	// Locate the handler by matching the request against the registered routes.
	f.echo.Router().Find(http.MethodPost, "/oauth/token", c)
	h := c.Handler()
	if h == nil {
		f.t.Fatalf("no handler matched /oauth/token")
	}
	if err := h(c); err != nil {
		f.echo.HTTPErrorHandler(err, c)
	}
	return rec
}

// testVerifier is the PKCE verifier every test uses. At 46 chars it satisfies
// the 43-char minimum enforced by VerifyPKCE.
const testVerifier = "the-verifier-a-a-a-a-a-a-a-a-a-a-a-a-a-a-a-a"

// s256 returns BASE64URL(SHA256(v)) — the exact encoding VerifyPKCE checks.
// We compute challenges at test-time instead of hardcoding so a future change
// to the encoding convention surfaces here, not via mysteriously-failing tests.
func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// success body round-trip helper.
type tokenBody struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func decodeTokenBody(t *testing.T, body []byte) tokenBody {
	t.Helper()
	var b tokenBody
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("decode body: %v body=%s", err, string(body))
	}
	return b
}

// putAuthCode seeds an AuthCodeEntry and returns the code string.
func (f *tokFixture) putAuthCode(e store.AuthCodeEntry) string {
	f.t.Helper()
	code := "code-" + time.Now().Format("150405.000000000")
	if e.ExpiresAt.IsZero() {
		e.ExpiresAt = time.Now().Add(2 * time.Minute)
	}
	if e.PKCEChallenge == "" {
		e.PKCEChallenge = s256(testVerifier)
	}
	f.codes.Put(code, e)
	return code
}

func TestToken_UnsupportedGrantType(t *testing.T) {
	f := newTokenFixture(t)
	f.buildHandler(fakeUsers{})

	rec := f.post(url.Values{"grant_type": {"password"}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrUnsupportedGrantType {
		t.Errorf("error=%q want %q", code, oauth.ErrUnsupportedGrantType)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control=%q, want no-store", got)
	}
}

func TestToken_AuthCode_HappyPathNonAgent(t *testing.T) {
	f := newTokenFixture(t)
	f.buildHandler(fakeUsers{})

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "web-console",
		UserID:      "usr-1",
		RedirectURI: "https://cp.nexus.ai/callback",
		Scope:       "openid profile",
		IdPID:       "local",
		Email:       "alice@nexus.ai",
		AMR:         []string{"pwd"},
	})

	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"web-console"},
		"redirect_uri":  {"https://cp.nexus.ai/callback"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control=%q, want no-store", got)
	}

	body := decodeTokenBody(t, rec.Body.Bytes())
	if body.TokenType != "Bearer" {
		t.Errorf("token_type=%q, want Bearer", body.TokenType)
	}
	if body.ExpiresIn != 3600 {
		t.Errorf("expires_in=%d, want 3600", body.ExpiresIn)
	}
	if body.RefreshToken != "rt-new" {
		t.Errorf("refresh_token=%q, want rt-new", body.RefreshToken)
	}
	if body.Scope != "openid profile" {
		t.Errorf("scope=%q, want round-trip", body.Scope)
	}
	if body.AccessToken == "" {
		t.Fatal("access_token empty")
	}

	// NewChain must have been called with the right shape.
	if n := atomic.LoadInt32(&f.refresh.newCalls); n != 1 {
		t.Fatalf("NewChain called %d times, want 1", n)
	}
	if got := f.refresh.lastNewArgs; got.UserID != "usr-1" || got.ClientID != "web-console" || got.DeviceID != "" {
		t.Errorf("NewChain args = %+v", got)
	}

	// Verify claims round-trip.
	claims := parseAccessClaims(t, body.AccessToken, f.keystore)
	if claims.Subject != "usr-1" || claims.SessionID != "sid-new" || claims.Email != "alice@nexus.ai" {
		t.Errorf("unexpected claims: %+v", claims)
	}
	if len(claims.AMR) != 1 || claims.AMR[0] != "pwd" {
		t.Errorf("amr=%v, want [pwd]", claims.AMR)
	}

	// Second attempt with the same code must fail — AuthCodeStore is single-use.
	rec2 := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"web-console"},
		"redirect_uri":  {"https://cp.nexus.ai/callback"},
	}, nil)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("replay status=%d, want 400", rec2.Code)
	}
	code2, _ := decodeError(t, rec2.Body.Bytes())
	if code2 != oauth.ErrInvalidGrant {
		t.Errorf("replay error=%q, want invalid_grant", code2)
	}
}

func TestToken_AuthCode_ClientIDMismatch(t *testing.T) {
	f := newTokenFixture(t)
	f.buildHandler(fakeUsers{})

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "web-console",
		UserID:      "usr-1",
		RedirectURI: "https://cp.nexus.ai/callback",
	})

	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"attacker-client"},
		"redirect_uri":  {"https://cp.nexus.ai/callback"},
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrInvalidGrant {
		t.Errorf("error=%q want invalid_grant", c)
	}
}

func TestToken_AuthCode_RedirectURIMismatch(t *testing.T) {
	f := newTokenFixture(t)
	f.buildHandler(fakeUsers{})

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "web-console",
		UserID:      "usr-1",
		RedirectURI: "https://cp.nexus.ai/callback",
	})
	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"web-console"},
		"redirect_uri":  {"https://attacker.example/cb"},
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrInvalidGrant {
		t.Errorf("error=%q want invalid_grant", c)
	}
}

func TestToken_AuthCode_PKCEMismatch(t *testing.T) {
	f := newTokenFixture(t)
	f.buildHandler(fakeUsers{})

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:      "web-console",
		UserID:        "usr-1",
		RedirectURI:   "https://cp.nexus.ai/callback",
		PKCEChallenge: s256("some-other-verifier-xxxxxxxxxxxxxxxxxxxxxxxx"),
	})

	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"web-console"},
		"redirect_uri":  {"https://cp.nexus.ai/callback"},
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrInvalidGrant {
		t.Errorf("error=%q want invalid_grant", c)
	}
}

func TestToken_AuthCode_AgentDesktopWithoutDeviceContext(t *testing.T) {
	f := newTokenFixture(t)
	f.buildHandler(fakeUsers{})

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "agent-desktop",
		UserID:      "usr-1",
		RedirectURI: "http://127.0.0.1:54321/callback",
		DeviceID:    "dev-42",
	})

	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"agent-desktop"},
		"redirect_uri":  {"http://127.0.0.1:54321/callback"},
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrInvalidGrant {
		t.Errorf("error=%q want invalid_grant", c)
	}
}

func TestToken_AuthCode_AgentDesktopDeviceMismatch(t *testing.T) {
	f := newTokenFixture(t)
	f.buildHandler(fakeUsers{})

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "agent-desktop",
		UserID:      "usr-1",
		RedirectURI: "http://127.0.0.1:54321/callback",
		DeviceID:    "dev-42",
	})

	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"agent-desktop"},
		"redirect_uri":  {"http://127.0.0.1:54321/callback"},
	}, &cpstore.ThingNodeInfo{ID: "dev-different", Status: "active"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrInvalidGrant {
		t.Errorf("error=%q want invalid_grant", c)
	}
}

func TestToken_AuthCode_AgentDesktopDeviceMatch(t *testing.T) {
	f := newTokenFixture(t)
	f.buildHandler(fakeUsers{})

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "agent-desktop",
		UserID:      "usr-7",
		RedirectURI: "http://127.0.0.1:54321/callback",
		DeviceID:    "dev-42",
		Scope:       "traffic:write",
	})

	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"agent-desktop"},
		"redirect_uri":  {"http://127.0.0.1:54321/callback"},
	}, &cpstore.ThingNodeInfo{ID: "dev-42", Status: "active"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeTokenBody(t, rec.Body.Bytes())
	if body.AccessToken == "" || body.RefreshToken == "" {
		t.Fatalf("missing tokens in body=%+v", body)
	}
	claims := parseAccessClaims(t, body.AccessToken, f.keystore)
	if claims.DeviceID != "dev-42" {
		t.Errorf("device_id claim=%q, want dev-42", claims.DeviceID)
	}
}

func TestToken_AuthCode_MissingRequiredParam(t *testing.T) {
	f := newTokenFixture(t)
	f.buildHandler(fakeUsers{})

	rec := f.post(url.Values{
		"grant_type": {"authorization_code"},
		// no code / verifier / client_id / redirect_uri
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrInvalidRequest {
		t.Errorf("error=%q want invalid_request", c)
	}
}

func TestToken_Refresh_HappyPath(t *testing.T) {
	f := newTokenFixture(t)
	email := "bob@nexus.ai"
	users := fakeUsers{"usr-1": {
		ID:    "usr-1",
		Email: &email,
	}}
	f.refresh.rotateReturns = []rotateResult{{
		newToken: "rt-rotated",
		newJTI:   "rjti-rotated",
		parent: &store.RefreshTokenRow{
			JTI:       "rjti-old",
			SessionID: "sid-persist",
			UserID:    "usr-1",
			ClientID:  "agent-desktop",
			DeviceID:  strPtr("dev-42"),
		},
	}}
	f.buildHandler(users)

	rec := f.post(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"rt-old"},
		"client_id":     {"agent-desktop"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeTokenBody(t, rec.Body.Bytes())
	if body.RefreshToken != "rt-rotated" {
		t.Errorf("refresh_token=%q, want rt-rotated", body.RefreshToken)
	}
	if body.ExpiresIn != 3600 {
		t.Errorf("expires_in=%d, want 3600", body.ExpiresIn)
	}
	claims := parseAccessClaims(t, body.AccessToken, f.keystore)
	if claims.Subject != "usr-1" {
		t.Errorf("sub=%q, want usr-1", claims.Subject)
	}
	if claims.SessionID != "sid-persist" {
		t.Errorf("sid=%q, want sid-persist (persisted across refresh)", claims.SessionID)
	}
	if claims.DeviceID != "dev-42" {
		t.Errorf("device_id=%q, want dev-42", claims.DeviceID)
	}
	if claims.Email != email {
		t.Errorf("email=%q, want %q", claims.Email, email)
	}

	if f.refresh.lastRotateInput != "rt-old" {
		t.Errorf("Rotate called with %q, want rt-old", f.refresh.lastRotateInput)
	}
}

func TestToken_Refresh_ReplayRejected(t *testing.T) {
	f := newTokenFixture(t)
	f.refresh.rotateErr = token.ErrReplay
	f.buildHandler(fakeUsers{})

	rec := f.post(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"rt-compromised"},
		"client_id":     {"web-console"},
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrInvalidGrant {
		t.Errorf("error=%q want invalid_grant", c)
	}
}

func TestToken_Refresh_ExpiredRejected(t *testing.T) {
	f := newTokenFixture(t)
	f.refresh.rotateErr = token.ErrExpired
	f.buildHandler(fakeUsers{})

	rec := f.post(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"rt-expired"},
		"client_id":     {"web-console"},
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrInvalidGrant {
		t.Errorf("error=%q want invalid_grant", c)
	}
}

func TestToken_Refresh_DisabledUser(t *testing.T) {
	f := newTokenFixture(t)
	disabled := time.Now().Add(-time.Hour)
	users := fakeUsers{"usr-1": {
		ID:         "usr-1",
		DisabledAt: &disabled,
	}}
	f.refresh.rotateReturns = []rotateResult{{
		newToken: "rt-rotated",
		newJTI:   "rjti-rotated",
		parent: &store.RefreshTokenRow{
			JTI:       "rjti-old",
			SessionID: "sid-x",
			UserID:    "usr-1",
			ClientID:  "web-console",
		},
	}}
	f.buildHandler(users)

	rec := f.post(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"rt-any"},
		"client_id":     {"web-console"},
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	c, desc := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrInvalidGrant {
		t.Errorf("error=%q want invalid_grant", c)
	}
	if !strings.Contains(desc, "disabled") {
		t.Errorf("description=%q, want contains 'disabled'", desc)
	}
}

func TestToken_Refresh_MissingToken(t *testing.T) {
	f := newTokenFixture(t)
	f.buildHandler(fakeUsers{})

	rec := f.post(url.Values{"grant_type": {"refresh_token"}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrInvalidRequest {
		t.Errorf("error=%q want invalid_request", c)
	}
}

// parseAccessClaims verifies + parses an access token against the test keystore.
// Shared helper so each test can assert on claim fields without repeating the
// kid-resolution dance.
func parseAccessClaims(t *testing.T, tok string, ks *token.Keystore) *token.AccessClaims {
	t.Helper()
	var claims token.AccessClaims
	parsed, err := jwt.ParseWithClaims(tok, &claims, func(jt *jwt.Token) (any, error) {
		kid, _ := jt.Header["kid"].(string)
		k, ok := ks.ByKID(kid)
		if !ok {
			t.Fatalf("unknown kid: %q", kid)
		}
		return &k.Priv.PublicKey, nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("parse access token: err=%v valid=%v", err, parsed != nil && parsed.Valid)
	}
	return &claims
}

func strPtr(s string) *string { return &s }
