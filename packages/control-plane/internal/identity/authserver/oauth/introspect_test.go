package oauth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/oauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// introFixture bundles the collaborators every introspect test needs so each
// case can focus on the scenario it is exercising instead of boilerplate.
type introFixture struct {
	t        *testing.T
	keystore *token.Keystore
	signer   *token.Signer
	deps     oauth.IntrospectDeps
	echo     *echo.Echo
}

// newIntrospectFixture wires a fresh signer + keystore and mounts the handler
// on an Echo instance. Each test gets its own temp-dir keystore so parallel
// runs cannot leak signing keys across cases.
func newIntrospectFixture(t *testing.T) *introFixture {
	t.Helper()
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	f := &introFixture{
		t:        t,
		keystore: ks,
		signer:   token.NewSigner(ks),
	}
	f.deps = oauth.IntrospectDeps{
		Issuer:   "https://cp.nexus.ai",
		Keystore: ks,
	}
	e := echo.New()
	e.POST("/oauth/introspect", oauth.IntrospectHandler(f.deps))
	f.echo = e
	return f
}

// post issues an x-www-form-urlencoded request to /oauth/introspect through
// the fixture's Echo instance via ServeHTTP so the full middleware chain runs
// exactly as it would in production.
func (f *introFixture) post(form url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	f.echo.ServeHTTP(rec, req)
	return rec
}

type introspectBody struct {
	Active    bool     `json:"active"`
	Issuer    string   `json:"iss,omitempty"`
	Subject   string   `json:"sub,omitempty"`
	Audience  []string `json:"aud,omitempty"`
	Expiry    int64    `json:"exp,omitempty"`
	IssuedAt  int64    `json:"iat,omitempty"`
	JTI       string   `json:"jti,omitempty"`
	ClientID  string   `json:"client_id,omitempty"`
	Scope     string   `json:"scope,omitempty"`
	TokenType string   `json:"token_type,omitempty"`
	Username  string   `json:"username,omitempty"`
}

// decodeIntrospect unmarshals the handler's JSON body.
func decodeIntrospect(t *testing.T, raw []byte) introspectBody {
	t.Helper()
	var b introspectBody
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("decode: %v body=%s", err, string(raw))
	}
	return b
}

// rawMap decodes the response body as a generic map so tests can assert on
// the presence/absence of keys (e.g. username MUST be omitted when Email is
// zero-valued — a round-trip to introspectBody would erase that distinction).
func rawMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode map: %v body=%s", err, string(raw))
	}
	return m
}

func TestIntrospect_ActiveForValidToken(t *testing.T) {
	f := newIntrospectFixture(t)

	// Mint a fully-populated access token, including Email so we can assert
	// username is exposed when present.
	accessTok, jti, err := token.IssueAccess(f.signer, token.AccessInput{
		Issuer:    "https://cp.nexus.ai",
		Subject:   "usr-1",
		Audience:  []string{"web-console"},
		ClientID:  "web-console",
		Scope:     "openid profile",
		SessionID: "sid-1",
		DeviceID:  "dev-42",
		Email:     "alice@nexus.ai",
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	rec := f.post(url.Values{"token": {accessTok}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type=%q, want application/json", got)
	}

	body := decodeIntrospect(t, rec.Body.Bytes())
	if !body.Active {
		t.Fatalf("active=false, want true: %+v", body)
	}
	if body.Issuer != "https://cp.nexus.ai" {
		t.Errorf("iss=%q, want https://cp.nexus.ai", body.Issuer)
	}
	if body.Subject != "usr-1" {
		t.Errorf("sub=%q, want usr-1", body.Subject)
	}
	if len(body.Audience) != 1 || body.Audience[0] != "web-console" {
		t.Errorf("aud=%v, want [web-console]", body.Audience)
	}
	if body.ClientID != "web-console" {
		t.Errorf("client_id=%q, want web-console", body.ClientID)
	}
	if body.Scope != "openid profile" {
		t.Errorf("scope=%q, want openid profile", body.Scope)
	}
	if body.JTI != jti {
		t.Errorf("jti=%q, want %q", body.JTI, jti)
	}
	if body.TokenType != "Bearer" {
		t.Errorf("token_type=%q, want Bearer", body.TokenType)
	}
	if body.Username != "alice@nexus.ai" {
		t.Errorf("username=%q, want alice@nexus.ai", body.Username)
	}
	if body.Expiry == 0 {
		t.Error("exp not set")
	}
	if body.IssuedAt == 0 {
		t.Error("iat not set")
	}

	// Second call: same setup but without Email → username must be omitted from
	// the JSON (not merely an empty string).
	accessNoEmail, _, err := token.IssueAccess(f.signer, token.AccessInput{
		Issuer:   "https://cp.nexus.ai",
		Subject:  "usr-2",
		Audience: []string{"web-console"},
		ClientID: "web-console",
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	rec2 := f.post(url.Values{"token": {accessNoEmail}})
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	m := rawMap(t, rec2.Body.Bytes())
	if _, ok := m["username"]; ok {
		t.Errorf("username key present with no Email: %v", m)
	}
	if active, _ := m["active"].(bool); !active {
		t.Errorf("active=%v, want true", m["active"])
	}
}

func TestIntrospect_InactiveForExpiredToken(t *testing.T) {
	f := newIntrospectFixture(t)

	// A negative TTL backdates exp before now, so jwt's validator rejects it.
	expired, _, err := token.IssueAccess(f.signer, token.AccessInput{
		Issuer:   "https://cp.nexus.ai",
		Subject:  "usr-1",
		Audience: []string{"web-console"},
		ClientID: "web-console",
		TTL:      -time.Minute,
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	rec := f.post(url.Values{"token": {expired}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeIntrospect(t, rec.Body.Bytes())
	if body.Active {
		t.Fatalf("expected active=false, got %+v", body)
	}
	// §2.2 requires no extra claims leak on a negative response.
	m := rawMap(t, rec.Body.Bytes())
	for k := range m {
		if k != "active" {
			t.Errorf("leaked key %q on inactive response: %v", k, m)
		}
	}
}

func TestIntrospect_InactiveForUnknownKid(t *testing.T) {
	f := newIntrospectFixture(t)

	// Forge a JWT signed by a brand-new RSA key that was never added to the
	// fixture keystore. The handler must refuse to resolve the kid → inactive.
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	claims := token.AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://cp.nexus.ai",
			Subject:   "usr-1",
			Audience:  jwt.ClaimStrings{"web-console"},
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			ID:        "jti-forged",
		},
		ClientID: "web-console",
	}
	jt := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	jt.Header["kid"] = "bogus-kid"
	forged, err := jt.SignedString(priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	rec := f.post(url.Values{"token": {forged}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeIntrospect(t, rec.Body.Bytes())
	if body.Active {
		t.Fatalf("expected active=false, got %+v", body)
	}
}

func TestIntrospect_InactiveForMissingToken(t *testing.T) {
	f := newIntrospectFixture(t)

	rec := f.post(url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeIntrospect(t, rec.Body.Bytes())
	if body.Active {
		t.Fatalf("expected active=false, got %+v", body)
	}
}

func TestIntrospect_InactiveForMalformedToken(t *testing.T) {
	f := newIntrospectFixture(t)

	rec := f.post(url.Values{"token": {"not.a.jwt"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeIntrospect(t, rec.Body.Bytes())
	if body.Active {
		t.Fatalf("expected active=false, got %+v", body)
	}
}

func TestIntrospect_RejectsNonRS256(t *testing.T) {
	f := newIntrospectFixture(t)

	// Sign the same claim shape with HS256. Even if the handler somehow
	// resolved a key, the alg check must short-circuit to inactive. This
	// defends against the classic alg-confusion attack.
	claims := token.AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://cp.nexus.ai",
			Subject:   "usr-1",
			Audience:  jwt.ClaimStrings{"web-console"},
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			ID:        "jti-hs",
		},
		ClientID: "web-console",
	}
	jt := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	jt.Header["kid"] = f.keystore.ActiveKID()
	hsTok, err := jt.SignedString([]byte("shared-secret-does-not-matter"))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	rec := f.post(url.Values{"token": {hsTok}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeIntrospect(t, rec.Body.Bytes())
	if body.Active {
		t.Fatalf("expected active=false, got %+v", body)
	}
}

func TestIntrospect_CacheControlNoStore(t *testing.T) {
	// §2.2 forbids clients from caching introspection responses on either
	// path — positive replies leak token lifetime, negative replies would
	// allow an attacker to memoise revocation state. Cover both explicitly
	// so a refactor that moves the header into only one branch is caught.
	t.Run("inactive", func(t *testing.T) {
		f := newIntrospectFixture(t)
		rec := f.post(url.Values{"token": {"anything"}})
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control=%q, want no-store", got)
		}
	})

	t.Run("active", func(t *testing.T) {
		f := newIntrospectFixture(t)
		accessTok, _, err := token.IssueAccess(f.signer, token.AccessInput{
			Issuer:   "https://cp.nexus.ai",
			Subject:  "usr-1",
			Audience: []string{"web-console"},
			ClientID: "web-console",
			TTL:      time.Hour,
		})
		if err != nil {
			t.Fatalf("IssueAccess: %v", err)
		}
		rec := f.post(url.Values{"token": {accessTok}})
		body := decodeIntrospect(t, rec.Body.Bytes())
		if !body.Active {
			t.Fatalf("expected active=true, got %+v", body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control=%q, want no-store", got)
		}
	})
}
