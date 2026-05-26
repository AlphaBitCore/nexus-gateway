package token_test

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// newTestSigner returns a fresh signer backed by a keystore with one key.
// t.TempDir scopes keystore PEM files to the test so parallel runs never
// collide.
func newTestSigner(t *testing.T) (*token.Signer, *token.Keystore) {
	t.Helper()
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return token.NewSigner(ks), ks
}

// parseAccess verifies sig + parses claims using the supplied keystore.
func parseAccess(t *testing.T, tok string, ks *token.Keystore) *token.AccessClaims {
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
		t.Fatalf("parse: err=%v valid=%v", err, parsed != nil && parsed.Valid)
	}
	return &claims
}

func TestIssueAccess_RoundTripsAllClaims(t *testing.T) {
	signer, ks := newTestSigner(t)

	in := token.AccessInput{
		Issuer:    "https://cp.nexus.ai",
		Subject:   "usr-1",
		Audience:  []string{"agent-desktop"},
		ClientID:  "agent-desktop",
		Scope:     "traffic:write",
		SessionID: "sid-abc",
		DeviceID:  "dev-42",
		Email:     "alice@nexus.ai",
		IdPID:     "local",
		AMR:       []string{"pwd"},
		TTL:       time.Hour,
	}

	before := time.Now()
	tok, jti, err := token.IssueAccess(signer, in)
	after := time.Now()
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	if tok == "" || jti == "" {
		t.Fatalf("empty return: tok=%q jti=%q", tok, jti)
	}

	claims := parseAccess(t, tok, ks)

	if claims.Issuer != in.Issuer {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, in.Issuer)
	}
	if claims.Subject != in.Subject {
		t.Errorf("Subject = %q, want %q", claims.Subject, in.Subject)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != in.Audience[0] {
		t.Errorf("Audience = %v, want %v", claims.Audience, in.Audience)
	}
	if claims.ClientID != in.ClientID {
		t.Errorf("ClientID = %q, want %q", claims.ClientID, in.ClientID)
	}
	if claims.Scope != in.Scope {
		t.Errorf("Scope = %q, want %q", claims.Scope, in.Scope)
	}
	if claims.SessionID != in.SessionID {
		t.Errorf("SessionID = %q, want %q", claims.SessionID, in.SessionID)
	}
	if claims.DeviceID != in.DeviceID {
		t.Errorf("DeviceID = %q, want %q", claims.DeviceID, in.DeviceID)
	}
	if claims.Email != in.Email {
		t.Errorf("Email = %q, want %q", claims.Email, in.Email)
	}
	if claims.IdPID != in.IdPID {
		t.Errorf("IdPID = %q, want %q", claims.IdPID, in.IdPID)
	}
	if len(claims.AMR) != 1 || claims.AMR[0] != "pwd" {
		t.Errorf("AMR = %v, want [pwd]", claims.AMR)
	}

	// jti must equal RegisteredClaims.ID so callers can correlate the token
	// with refresh rows without re-parsing.
	if claims.ID != jti {
		t.Errorf("claims.ID = %q, want jti=%q", claims.ID, jti)
	}

	if claims.IssuedAt == nil || claims.ExpiresAt == nil {
		t.Fatalf("iat or exp missing: iat=%v exp=%v", claims.IssuedAt, claims.ExpiresAt)
	}
	iat := claims.IssuedAt.Time
	exp := claims.ExpiresAt.Time
	// iat must be bracketed by the wall-clock samples around IssueAccess.
	// JWT numeric dates round to whole seconds so the lower bound is offset
	// by one second to tolerate truncation.
	if iat.Before(before.Add(-time.Second)) || iat.After(after.Add(time.Second)) {
		t.Errorf("iat = %v, want in [%v, %v]", iat, before, after)
	}
	// exp - iat must equal the configured TTL (seconds precision).
	if got := exp.Sub(iat); got < in.TTL-time.Second || got > in.TTL+time.Second {
		t.Errorf("exp-iat = %v, want ~%v", got, in.TTL)
	}
}

func TestIssueAccess_OptionalFieldsOmitted(t *testing.T) {
	signer, ks := newTestSigner(t)

	in := token.AccessInput{
		Issuer:   "https://cp.nexus.ai",
		Subject:  "usr-2",
		Audience: []string{"web-console"},
		ClientID: "web-console",
		TTL:      time.Minute,
	}

	tok, _, err := token.IssueAccess(signer, in)
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	claims := parseAccess(t, tok, ks)

	if claims.Scope != "" || claims.SessionID != "" || claims.DeviceID != "" {
		t.Errorf("unexpected optional fields: scope=%q sid=%q dev=%q",
			claims.Scope, claims.SessionID, claims.DeviceID)
	}
	if claims.Email != "" || claims.IdPID != "" {
		t.Errorf("unexpected optional fields: email=%q idp=%q", claims.Email, claims.IdPID)
	}
	if len(claims.AMR) != 0 {
		t.Errorf("slices should be empty: amr=%v", claims.AMR)
	}
}

func TestIssueAccess_JTIUniquePerCall(t *testing.T) {
	signer, _ := newTestSigner(t)

	const n = 16
	seen := make(map[string]struct{}, n)
	for i := range n {
		_, jti, err := token.IssueAccess(signer, token.AccessInput{
			Issuer: "iss", Subject: "u", Audience: []string{"c"},
			ClientID: "c", TTL: time.Minute,
		})
		if err != nil {
			t.Fatalf("IssueAccess: %v", err)
		}
		if _, dup := seen[jti]; dup {
			t.Fatalf("duplicate jti %q after %d calls", jti, i)
		}
		seen[jti] = struct{}{}
	}
}
