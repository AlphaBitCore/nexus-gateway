package jwtverifier_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
)

// testIssuer and testAudience are the expected values wired into the
// Verifier in every test; evil values are used to assert rejection.
const (
	testIssuer   = "test-iss"
	testAudience = "test-aud"
	testKID      = "k1"
)

// verifierFixture bundles everything a test needs to mint a token and then
// hand it to Verify.
type verifierFixture struct {
	verifier *jwtverifier.Verifier
	priv     *rsa.PrivateKey
	srv      *httptest.Server
}

// newTestVerifier stands up a JWKS-serving httptest server with a single
// RS256 key (kid=k1), constructs a Verifier wired to it, and returns the
// bundle. Cleanup closes the server.
func newTestVerifier(t *testing.T) *verifierFixture {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nBytes := priv.N.Bytes()
		eBig := big.NewInt(int64(priv.E))
		doc := map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"alg": "RS256",
					"use": "sig",
					"kid": testKID,
					"n":   base64.RawURLEncoding.EncodeToString(nBytes),
					"e":   base64.RawURLEncoding.EncodeToString(eBig.Bytes()),
				},
			},
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)

	v := jwtverifier.New(jwtverifier.Config{
		Issuer:   testIssuer,
		JWKSURL:  srv.URL,
		Audience: testAudience,
		RevCheck: jwtverifier.AlwaysAllow{},
	})
	return &verifierFixture{verifier: v, priv: priv, srv: srv}
}

// signRS256 signs the given claims with the fixture key and kid=k1.
func (f *verifierFixture) signRS256(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = testKID
	raw, err := tok.SignedString(f.priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return raw
}

// baseClaims returns a fully-populated, currently-valid MapClaims for use as
// a template; individual tests override what they need.
func baseClaims() jwt.MapClaims {
	now := time.Now().Unix()
	return jwt.MapClaims{
		"iss":        testIssuer,
		"aud":        []string{testAudience},
		"sub":        "usr-1",
		"exp":        now + 3600,
		"iat":        now,
		"nbf":        now,
		"jti":        "jti-1",
		"client_id":  "foo",
		"scope":      "s1 s2",
		"device_id":  "dev-1",
		"session_id": "sid-1",
		"email":      "a@b",
		"idp":        "local",
		"auth_mode":  "interactive",
		"amr":        []string{"pwd"},
	}
}

func TestVerify_ValidToken(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)
	raw := f.signRS256(t, baseClaims())

	c, err := f.verifier.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c == nil {
		t.Fatal("Verify returned nil claims with nil error")
	}

	if c.Issuer != testIssuer { //nolint:staticcheck // SA5011: t.Fatal above terminates the test goroutine
		t.Errorf("Issuer = %q, want %q", c.Issuer, testIssuer)
	}
	if c.Subject != "usr-1" {
		t.Errorf("Subject = %q, want usr-1", c.Subject)
	}
	if len(c.Audience) != 1 || c.Audience[0] != testAudience {
		t.Errorf("Audience = %v, want [%q]", c.Audience, testAudience)
	}
	if c.ClientID != "foo" {
		t.Errorf("ClientID = %q, want foo", c.ClientID)
	}
	if c.Email != "a@b" {
		t.Errorf("Email = %q, want a@b", c.Email)
	}
	if c.Scope != "s1 s2" {
		t.Errorf("Scope = %q, want s1 s2", c.Scope)
	}
	if c.DeviceID != "dev-1" {
		t.Errorf("DeviceID = %q, want dev-1", c.DeviceID)
	}
	if c.SessionID != "sid-1" {
		t.Errorf("SessionID = %q, want sid-1", c.SessionID)
	}
	if c.JTI != "jti-1" {
		t.Errorf("JTI = %q, want jti-1", c.JTI)
	}
	if c.IDP != "local" {
		t.Errorf("IDP = %q, want local", c.IDP)
	}
	if c.AuthMode != "interactive" {
		t.Errorf("AuthMode = %q, want interactive", c.AuthMode)
	}
	if len(c.AMR) != 1 || c.AMR[0] != "pwd" {
		t.Errorf("AMR = %v, want [pwd]", c.AMR)
	}
	if c.ExpiresAt == 0 {
		t.Error("ExpiresAt = 0, want non-zero")
	}
	if c.IssuedAt == 0 {
		t.Error("IssuedAt = 0, want non-zero")
	}
	if c.NotBefore == 0 {
		t.Error("NotBefore = 0, want non-zero")
	}
}

func TestVerify_WrongIssuer(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)
	claims := baseClaims()
	claims["iss"] = "evil-iss"
	raw := f.signRS256(t, claims)

	_, err := f.verifier.Verify(context.Background(), raw)
	if !errors.Is(err, jwtverifier.ErrWrongIssuer) {
		t.Fatalf("err = %v, want ErrWrongIssuer", err)
	}
}

func TestVerify_WrongAudience(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)
	claims := baseClaims()
	claims["aud"] = []string{"other"}
	raw := f.signRS256(t, claims)

	_, err := f.verifier.Verify(context.Background(), raw)
	if !errors.Is(err, jwtverifier.ErrWrongAudience) {
		t.Fatalf("err = %v, want ErrWrongAudience", err)
	}
}

func TestVerify_Expired(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)
	claims := baseClaims()
	// Exp 2 hours ago — comfortably past the 5-minute default skew.
	claims["exp"] = time.Now().Add(-2 * time.Hour).Unix()
	claims["iat"] = time.Now().Add(-3 * time.Hour).Unix()
	claims["nbf"] = time.Now().Add(-3 * time.Hour).Unix()
	raw := f.signRS256(t, claims)

	_, err := f.verifier.Verify(context.Background(), raw)
	if !errors.Is(err, jwtverifier.ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestVerify_NotYetValid(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)
	claims := baseClaims()
	// Nbf 2 hours in the future — comfortably past the 5-minute default skew.
	claims["nbf"] = time.Now().Add(2 * time.Hour).Unix()
	claims["iat"] = time.Now().Add(2 * time.Hour).Unix()
	claims["exp"] = time.Now().Add(4 * time.Hour).Unix()
	raw := f.signRS256(t, claims)

	_, err := f.verifier.Verify(context.Background(), raw)
	if !errors.Is(err, jwtverifier.ErrNotYetValid) {
		t.Fatalf("err = %v, want ErrNotYetValid", err)
	}
}

func TestVerify_ClockSkewTolerated(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)

	// Case A: token expired 4 minutes ago; default 5-minute skew lets it pass.
	claimsA := baseClaims()
	claimsA["exp"] = time.Now().Add(-4 * time.Minute).Unix()
	claimsA["iat"] = time.Now().Add(-1 * time.Hour).Unix()
	claimsA["nbf"] = time.Now().Add(-1 * time.Hour).Unix()
	rawA := f.signRS256(t, claimsA)

	if _, err := f.verifier.Verify(context.Background(), rawA); err != nil {
		t.Fatalf("4-min-expired within skew: err = %v, want nil", err)
	}

	// Case B: nbf 4 minutes in the future; default 5-minute skew lets it pass.
	claimsB := baseClaims()
	claimsB["nbf"] = time.Now().Add(4 * time.Minute).Unix()
	claimsB["iat"] = time.Now().Add(-1 * time.Minute).Unix()
	claimsB["exp"] = time.Now().Add(1 * time.Hour).Unix()
	rawB := f.signRS256(t, claimsB)

	if _, err := f.verifier.Verify(context.Background(), rawB); err != nil {
		t.Fatalf("nbf 4min future within skew: err = %v, want nil", err)
	}
}

func TestVerify_UnknownKid(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)

	// Generate a DIFFERENT key; sign with kid="other" not present in JWKS.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, baseClaims())
	tok.Header["kid"] = "other"
	raw, err := tok.SignedString(other)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	_, err = f.verifier.Verify(context.Background(), raw)
	if !errors.Is(err, jwtverifier.ErrJWKSUnavailable) {
		t.Fatalf("err = %v, want ErrJWKSUnavailable", err)
	}
}

func TestVerify_Malformed(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)

	_, err := f.verifier.Verify(context.Background(), "not.a.jwt")
	if !errors.Is(err, jwtverifier.ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

// jtiRevoker returns (true, nil) iff the token's jti matches the target.
type jtiRevoker struct{ target string }

func (r jtiRevoker) IsRevoked(_ context.Context, c *jwtverifier.Claims) (bool, error) {
	return c.JTI == r.target, nil
}

func TestVerify_RevokedByChecker(t *testing.T) {
	t.Parallel()

	// Use the standard fixture to spin up keys + JWKS, then swap in a custom
	// verifier pointing at the same JWKS URL but with a real revocation
	// checker.
	base := newTestVerifier(t)
	v := jwtverifier.New(jwtverifier.Config{
		Issuer:   testIssuer,
		JWKSURL:  base.srv.URL,
		Audience: testAudience,
		RevCheck: jtiRevoker{target: "revoked-jti"},
	})

	claims := baseClaims()
	claims["jti"] = "revoked-jti"
	raw := base.signRS256(t, claims)

	_, err := v.Verify(context.Background(), raw)
	if !errors.Is(err, jwtverifier.ErrRevoked) {
		t.Fatalf("err = %v, want ErrRevoked", err)
	}

	// Sanity check: a different jti passes.
	claims["jti"] = "other-jti"
	raw2 := base.signRS256(t, claims)
	if _, err := v.Verify(context.Background(), raw2); err != nil {
		t.Fatalf("non-revoked jti: err = %v, want nil", err)
	}
}

// TestVerify_RejectsEmptySubject locks the structural-completeness contract
// at the trust boundary: a signature-valid token whose `sub` is empty must be
// rejected as malformed, so no downstream consumer ever sees an empty
// principal id. The minter does not emit such tokens today, but the verifier
// is the only common choke point for every CP/Hub/AI-Gateway/Compliance-Proxy
// consumer of the same JWKS.
func TestVerify_RejectsEmptySubject(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)
	claims := baseClaims()
	claims["sub"] = ""
	raw := f.signRS256(t, claims)

	_, err := f.verifier.Verify(context.Background(), raw)
	if !errors.Is(err, jwtverifier.ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func TestVerify_WrongAlgRejected(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)

	// Sign with HS256 instead of RS256 — WithValidMethods should reject.
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, baseClaims())
	tok.Header["kid"] = testKID
	raw, err := tok.SignedString([]byte("shared-secret-no-good"))
	if err != nil {
		t.Fatalf("SignedString HS256: %v", err)
	}

	_, err = f.verifier.Verify(context.Background(), raw)
	// jwt/v5's WithValidMethods wraps the rejection in ErrTokenSignatureInvalid,
	// which our mapParseError falls through to ErrInvalidSignature.
	if !errors.Is(err, jwtverifier.ErrInvalidSignature) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}
}

// rawCapturingChecker is a RevocationChecker that records the Raw field of
// the Claims it sees at IsRevoked call time. Used to assert that Verifier
// pre-populates Claims.Raw BEFORE calling the checker.
type rawCapturingChecker struct {
	captured string
}

func (r *rawCapturingChecker) IsRevoked(_ context.Context, c *jwtverifier.Claims) (bool, error) {
	r.captured = c.Raw
	return false, nil
}

// TestVerify_RawPrePopulatedBeforeRevocationCheck asserts that Claims.Raw is
// set to the original token string before RevCheck.IsRevoked is called.
// Previously c.Raw = raw was placed AFTER the IsRevoked call, making
// introspect-based checkers see an empty Raw and short-circuit to allow.
func TestVerify_RawPrePopulatedBeforeRevocationCheck(t *testing.T) {
	t.Parallel()

	base := newTestVerifier(t)
	checker := &rawCapturingChecker{}
	v := jwtverifier.New(jwtverifier.Config{
		Issuer:   testIssuer,
		JWKSURL:  base.srv.URL,
		Audience: testAudience,
		RevCheck: checker,
	})

	raw := base.signRS256(t, baseClaims())
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if checker.captured == "" {
		t.Fatal("Claims.Raw was empty when IsRevoked was called; want the original token string")
	}
	if checker.captured != raw {
		t.Errorf("Claims.Raw = %q, want %q", checker.captured, raw)
	}
}
