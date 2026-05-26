package token_test

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// TestVerifyLocal_NilKeystoreRejected covers the early-guard branch — a
// nil keystore must surface ErrInvalidAccessToken rather than dereference
// and panic. The error text intentionally does not leak that the keystore
// was the cause (anti-enumeration).
func TestVerifyLocal_NilKeystoreRejected(t *testing.T) {
	_, err := token.VerifyLocal(nil, "https://cp.nexus.ai", "anything")
	if !errors.Is(err, token.ErrInvalidAccessToken) {
		t.Fatalf("err = %v, want ErrInvalidAccessToken", err)
	}
}

// TestVerifyLocal_EmptyRawRejected covers the empty-token guard — an empty
// Bearer header must never reach jwt.ParseWithClaims (parser internals
// allocate before checking length and panic on some forks).
func TestVerifyLocal_EmptyRawRejected(t *testing.T) {
	_, ks := newTestSigner(t)
	_, err := token.VerifyLocal(ks, "https://cp.nexus.ai", "")
	if !errors.Is(err, token.ErrInvalidAccessToken) {
		t.Fatalf("err = %v, want ErrInvalidAccessToken", err)
	}
}

// TestVerifyLocal_RS384Rejected exercises the second alg-confusion branch:
// the token IS signed with an RSA family alg, so the first type-switch
// passes, but the alg string is not RS256. VerifyLocal must still reject
// — accepting any RSA alg would let attackers downgrade to weaker hashes.
func TestVerifyLocal_RS384Rejected(t *testing.T) {
	_, ks := newTestSigner(t)
	// Find the active key so we can sign with it under a different alg.
	kid := ks.ActiveKID()
	key, ok := ks.ByKID(kid)
	if !ok {
		t.Fatalf("active key not found")
	}

	claims := token.AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://cp.nexus.ai",
			Subject:   "usr-1",
			Audience:  jwt.ClaimStrings{token.AdminAudience},
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			ID:        "jti-rs384",
		},
		ClientID: "cp-ui",
	}
	jt := jwt.NewWithClaims(jwt.SigningMethodRS384, claims)
	jt.Header["kid"] = kid
	tok, err := jt.SignedString(key.Priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := token.VerifyLocal(ks, "https://cp.nexus.ai", tok); !errors.Is(err, token.ErrInvalidAccessToken) {
		t.Fatalf("RS384 must be rejected, got: %v", err)
	}
}

// TestVerifyLocal_EmptyKidRejected covers the kid-presence guard. A JWT
// missing kid is unverifiable in our model — every key in the keystore
// has a kid and is selected via that header alone.
func TestVerifyLocal_EmptyKidRejected(t *testing.T) {
	_, ks := newTestSigner(t)
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	claims := token.AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://cp.nexus.ai",
			Subject:   "usr-1",
			Audience:  jwt.ClaimStrings{token.AdminAudience},
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			ID:        "jti-nokid",
		},
		ClientID: "cp-ui",
	}
	jt := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	// Deliberately do NOT set jt.Header["kid"].
	tok, err := jt.SignedString(priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := token.VerifyLocal(ks, "https://cp.nexus.ai", tok); !errors.Is(err, token.ErrInvalidAccessToken) {
		t.Fatalf("empty kid must be rejected, got: %v", err)
	}
}

// TestVerifyLocal_SignatureTamperingRejected mints a real token then
// flips the last byte of its signature. The library must reject the
// modified compact form — defense in depth for the "RSA key compromise
// is the only way past a signed token" property.
func TestVerifyLocal_SignatureTamperingRejected(t *testing.T) {
	signer, ks := newTestSigner(t)
	tok, _, err := token.IssueAccess(signer, token.AccessInput{
		Issuer:   "https://cp.nexus.ai",
		Subject:  "usr-1",
		Audience: []string{token.AdminAudience},
		ClientID: "cp-ui",
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	// Flip a character in the signature portion.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed compact JWT: %d parts", len(parts))
	}
	sig := []byte(parts[2])
	if len(sig) < 8 {
		t.Fatalf("signature too short to tamper")
	}
	// Replace a character at a stable mid-point so the decoded-byte change
	// is unambiguous and away from base64url's last-group quirks. Earlier
	// "swap last two chars" was flaky because base64url signatures can
	// end in repeated identical chars, making the swap a no-op (VerifyLocal
	// then accepted the unchanged token and the test passed by "succeeded"
	// rather than "tampered-rejected"). Picking mid-point + replace-with-
	// different-char guarantees the decoded byte string differs.
	mid := len(sig) / 2
	if sig[mid] == 'A' {
		sig[mid] = 'B'
	} else {
		sig[mid] = 'A'
	}
	tampered := parts[0] + "." + parts[1] + "." + string(sig)

	if _, err := token.VerifyLocal(ks, "https://cp.nexus.ai", tampered); !errors.Is(err, token.ErrInvalidAccessToken) {
		t.Fatalf("tampered sig must be rejected, got: %v", err)
	}
}

// TestVerifyLocal_IssuerEmptyAllowedSkipsPinning documents that when the
// caller passes issuer="" the helper does NOT pin (so callers wiring this
// helper for shared-tenancy scenarios can opt out). A pinned issuer
// elsewhere in the codebase is the safe default; this branch only exists
// so tests / dev tools can bypass it without forking the helper.
func TestVerifyLocal_IssuerEmptyAllowedSkipsPinning(t *testing.T) {
	signer, ks := newTestSigner(t)
	tok, _, err := token.IssueAccess(signer, token.AccessInput{
		Issuer:   "https://whatever.example",
		Subject:  "usr-1",
		Audience: []string{token.AdminAudience},
		ClientID: "cp-ui",
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	claims, err := token.VerifyLocal(ks, "", tok)
	if err != nil {
		t.Fatalf("issuer='' must skip pinning: %v", err)
	}
	if claims.Issuer != "https://whatever.example" {
		t.Errorf("issuer round-trip mismatch: %q", claims.Issuer)
	}
}

// TestVerifyLocal_MalformedJWTRejected exercises the jwt.ParseWithClaims
// failure branch on a syntactically invalid string. Same anti-enumeration
// surface as every other VerifyLocal failure.
func TestVerifyLocal_MalformedJWTRejected(t *testing.T) {
	_, ks := newTestSigner(t)
	if _, err := token.VerifyLocal(ks, "https://cp.nexus.ai", "not-a-jwt"); !errors.Is(err, token.ErrInvalidAccessToken) {
		t.Fatalf("malformed JWT must be rejected, got: %v", err)
	}
}
