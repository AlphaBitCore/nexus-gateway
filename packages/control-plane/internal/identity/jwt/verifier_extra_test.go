package jwtverifier_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
)

// errorRevoker returns a sentinel error from IsRevoked. Used to assert that
// Verifier.Verify propagates a revocation-checker error verbatim — the wrapping
// behaviour of /oauth/introspect outages depends on this passthrough.
type errorRevoker struct{ err error }

func (r errorRevoker) IsRevoked(_ context.Context, _ *jwtverifier.Claims) (bool, error) {
	return false, r.err
}

// TestVerify_RevocationCheckerError_Propagates pins the contract that an error
// from the revocation checker surfaces to the caller as-is. This is the
// fail-closed introspect-outage branch — without it, an /oauth/introspect
// outage would either deny every token or silently allow revoked ones.
func TestVerify_RevocationCheckerError_Propagates(t *testing.T) {
	t.Parallel()

	base := newTestVerifier(t)
	sentinel := errors.New("introspect outage")
	v := jwtverifier.New(jwtverifier.Config{
		Issuer:   testIssuer,
		JWKSURL:  base.srv.URL,
		Audience: testAudience,
		RevCheck: errorRevoker{err: sentinel},
	})

	raw := base.signRS256(t, baseClaims())
	_, err := v.Verify(context.Background(), raw)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel %v (revocation-checker error must propagate)", err, sentinel)
	}
}

// TestVerify_ClaimInt64_HandlesJSONNumberAndInt64 pins the int64 + zero-fallback
// branches of claimInt64. encoding/json normally hands numeric JWT claims to
// jwt/v5 as float64, but the typed-Claims API accepts int64 too — and a
// completely missing claim must come back as the zero value, not panic.
//
// We cannot easily inject int64 through the jwt.Parse path (it always JSON-
// decodes into float64), but we CAN observe the missing-claim zero-fallback
// directly through Verify: a token minted without iat/nbf must verify and
// surface IssuedAt=0 / NotBefore=0 (instead of panicking on a missing key).
func TestVerify_ClaimInt64_MissingClaimsFallToZero(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)
	// Minimal claim set: omit iat and nbf entirely. exp + iss + aud + sub are
	// still required for a successful verify.
	now := time.Now().Unix()
	claims := jwt.MapClaims{
		"iss": testIssuer,
		"aud": []string{testAudience},
		"sub": "usr-only-exp",
		"exp": now + 3600,
	}
	raw := f.signRS256(t, claims)

	c, err := f.verifier.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.IssuedAt != 0 {
		t.Errorf("IssuedAt = %d, want 0 (missing iat claim should default to zero)", c.IssuedAt)
	}
	if c.NotBefore != 0 {
		t.Errorf("NotBefore = %d, want 0 (missing nbf claim should default to zero)", c.NotBefore)
	}
}

// TestVerify_AudienceAsBareString accepts the RFC 7519 tolerant form where
// `aud` is a single string rather than a string array. claimStringSlice must
// promote it to a single-element slice so the contains() audience check still
// matches.
func TestVerify_AudienceAsBareString(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)
	claims := baseClaims()
	claims["aud"] = testAudience // bare string, not []string
	raw := f.signRS256(t, claims)

	c, err := f.verifier.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify with bare-string aud: %v", err)
	}
	if len(c.Audience) != 1 || c.Audience[0] != testAudience {
		t.Errorf("Audience = %v, want [%q] (bare string must promote to single-element slice)", c.Audience, testAudience)
	}
}

// TestVerify_AudienceAsEmptyString rejects an empty-string aud — claimStringSlice
// returns nil for "", so the contains() audience check fails and the token is
// rejected as wrong-audience. Pins that an attacker-controlled minter cannot
// trick the verifier with "aud=\"\"".
func TestVerify_AudienceAsEmptyString(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)
	claims := baseClaims()
	claims["aud"] = "" // empty bare string
	raw := f.signRS256(t, claims)

	_, err := f.verifier.Verify(context.Background(), raw)
	if !errors.Is(err, jwtverifier.ErrWrongAudience) {
		t.Fatalf("err = %v, want ErrWrongAudience (empty-string aud must not satisfy match)", err)
	}
}

// TestVerify_AudienceArrayWithMixedTypes — claimStringSlice's []any branch must
// skip non-string entries (e.g. a number) rather than panic, so a tampered aud
// payload like [42, "test-aud"] still surfaces the legitimate audience.
func TestVerify_AudienceArrayWithMixedTypes(t *testing.T) {
	t.Parallel()

	f := newTestVerifier(t)
	claims := baseClaims()
	claims["aud"] = []any{42.0, testAudience, true}
	raw := f.signRS256(t, claims)

	c, err := f.verifier.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify with mixed-type aud array: %v", err)
	}
	// Only the string entry should survive the type filter.
	if len(c.Audience) != 1 || c.Audience[0] != testAudience {
		t.Errorf("Audience = %v, want [%q] (non-string entries must be dropped, not crash)", c.Audience, testAudience)
	}
}

// TestVerify_DefaultClockSkewApplied asserts the zero-value ClockSkew defaults
// to 5 minutes. A token with exp 4 minutes in the past must still verify when
// ClockSkew is left at its zero value.
func TestVerify_DefaultClockSkewApplied(t *testing.T) {
	t.Parallel()

	base := newTestVerifier(t)
	v := jwtverifier.New(jwtverifier.Config{
		Issuer:   testIssuer,
		JWKSURL:  base.srv.URL,
		Audience: testAudience,
		// ClockSkew left at zero — should default to 5 min.
	})
	claims := baseClaims()
	claims["exp"] = time.Now().Add(-4 * time.Minute).Unix()
	claims["iat"] = time.Now().Add(-1 * time.Hour).Unix()
	claims["nbf"] = time.Now().Add(-1 * time.Hour).Unix()
	raw := base.signRS256(t, claims)

	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("expected default 5-min skew to accept 4-min-expired token: %v", err)
	}
}

// TestNew_NilRevCheckFallsBackToAlwaysAllow pins the constructor default: a
// nil RevCheck must opt the caller into AlwaysAllow{}. Otherwise a valid token
// would NPE on every verify because the verifier dereferences cfg.RevCheck.
func TestNew_NilRevCheckFallsBackToAlwaysAllow(t *testing.T) {
	t.Parallel()

	base := newTestVerifier(t)
	v := jwtverifier.New(jwtverifier.Config{
		Issuer:   testIssuer,
		JWKSURL:  base.srv.URL,
		Audience: testAudience,
		// RevCheck nil — must NOT panic; must default to AlwaysAllow.
	})

	raw := base.signRS256(t, baseClaims())
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("Verify with nil RevCheck (default AlwaysAllow): %v", err)
	}
}
