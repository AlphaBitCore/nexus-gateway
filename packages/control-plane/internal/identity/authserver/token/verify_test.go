package token_test

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// TestVerifyLocal_HappyPath proves a signed access token minted by our signer
// round-trips through VerifyLocal with every claim intact.
func TestVerifyLocal_HappyPath(t *testing.T) {
	signer, ks := newTestSigner(t)
	tok, jti, err := token.IssueAccess(signer, token.AccessInput{
		Issuer:   "https://cp.nexus.ai",
		Subject:  "usr-1",
		Audience: []string{token.AdminAudience},
		ClientID: "cp-ui",
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	claims, err := token.VerifyLocal(ks, "https://cp.nexus.ai", tok)
	if err != nil {
		t.Fatalf("VerifyLocal: %v", err)
	}
	if claims.ID != jti {
		t.Errorf("jti=%q, want %q", claims.ID, jti)
	}
	if claims.ClientID != "cp-ui" {
		t.Errorf("client_id=%q, want cp-ui", claims.ClientID)
	}
}

// TestVerifyLocal_WrongIssuer ensures the issuer is pinned: any mismatch has
// to surface as an error so the introspect / revoke handlers can reject the
// token without echoing foreign issuer data back.
func TestVerifyLocal_WrongIssuer(t *testing.T) {
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
	if _, err := token.VerifyLocal(ks, "https://evil.example", tok); err == nil {
		t.Fatalf("expected error for wrong issuer, got nil")
	}
}

// TestVerifyLocal_UnknownKid forges a well-formed RS256 JWT signed by a key
// the keystore never saw. VerifyLocal must refuse to resolve the kid.
func TestVerifyLocal_UnknownKid(t *testing.T) {
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
			ID:        "jti-forged",
		},
		ClientID: "cp-ui",
	}
	jt := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	jt.Header["kid"] = "bogus-kid"
	forged, err := jt.SignedString(priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := token.VerifyLocal(ks, "https://cp.nexus.ai", forged); err == nil {
		t.Fatalf("expected error for unknown kid, got nil")
	}
}

// TestVerifyLocal_HS256Rejected defends against the classic alg-confusion
// attack: a token signed with HS256 (even one whose kid points at a real
// keystore entry) must never verify via our RSA helper.
func TestVerifyLocal_HS256Rejected(t *testing.T) {
	_, ks := newTestSigner(t)
	claims := token.AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://cp.nexus.ai",
			Subject:   "usr-1",
			Audience:  jwt.ClaimStrings{token.AdminAudience},
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			ID:        "jti-hs",
		},
		ClientID: "cp-ui",
	}
	jt := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	jt.Header["kid"] = ks.ActiveKID()
	hsTok, err := jt.SignedString([]byte("shared-secret-does-not-matter"))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := token.VerifyLocal(ks, "https://cp.nexus.ai", hsTok); err == nil {
		t.Fatalf("expected error for HS256 token, got nil")
	}
}

// TestVerifyLocal_ExpiredRejected exercises the jwt library's built-in exp
// validator. A backdated exp must be rejected.
func TestVerifyLocal_ExpiredRejected(t *testing.T) {
	signer, ks := newTestSigner(t)
	tok, _, err := token.IssueAccess(signer, token.AccessInput{
		Issuer:   "https://cp.nexus.ai",
		Subject:  "usr-1",
		Audience: []string{token.AdminAudience},
		ClientID: "cp-ui",
		TTL:      -time.Minute, // exp in the past
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	if _, err := token.VerifyLocal(ks, "https://cp.nexus.ai", tok); err == nil {
		t.Fatalf("expected error for expired token, got nil")
	}
}

// TestVerifyLocal_MissingClientID rejects a signed token that lacks a
// client_id claim; IssueAccess always stamps one, so a missing value means
// the token was not minted by us.
func TestVerifyLocal_MissingClientID(t *testing.T) {
	signer, ks := newTestSigner(t)
	// Hand-build an RS256 token with no client_id claim by skipping the
	// AccessClaims field entirely.
	claims := jwt.RegisteredClaims{
		Issuer:    "https://cp.nexus.ai",
		Subject:   "usr-1",
		Audience:  jwt.ClaimStrings{token.AdminAudience},
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		ID:        "jti-noclient",
	}
	tok, err := signer.Sign(claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := token.VerifyLocal(ks, "https://cp.nexus.ai", tok); err == nil {
		t.Fatalf("expected error for missing client_id, got nil")
	}
}
