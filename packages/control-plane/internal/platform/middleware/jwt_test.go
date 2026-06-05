package middleware

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// helper: encode big.Int to base64url (unpadded) as used in JWK.
func bigIntToB64(n *big.Int) string {
	return base64.RawURLEncoding.EncodeToString(n.Bytes())
}

// helper: encode int to base64url as used in JWK exponent.
func intToB64(e int) string {
	b := big.NewInt(int64(e)).Bytes()
	return base64.RawURLEncoding.EncodeToString(b)
}

// helper: build a signed JWT from parts.
func buildJWT(t *testing.T, header, payload map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	hJSON, _ := json.Marshal(header)
	pJSON, _ := json.Marshal(payload)
	hB64 := base64.RawURLEncoding.EncodeToString(hJSON)
	pB64 := base64.RawURLEncoding.EncodeToString(pJSON)
	sigInput := hB64 + "." + pB64
	hashed := sha256.Sum256([]byte(sigInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	sB64 := base64.RawURLEncoding.EncodeToString(sig)
	return hB64 + "." + pB64 + "." + sB64
}

// startJWKSServer creates an httptest server serving a JWKS with one RSA key.
func startJWKSServer(t *testing.T, kid string, pub *rsa.PublicKey) *httptest.Server {
	t.Helper()
	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": kid,
				"use": "sig",
				"alg": "RS256",
				"n":   bigIntToB64(pub.N),
				"e":   intToB64(pub.E),
			},
		},
	}
	body, _ := json.Marshal(jwks)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestValidateJWT_Valid(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kid := "test-kid-1"
	srv := startJWKSServer(t, kid, &priv.PublicKey)

	config := &OidcConfig{
		Enabled:    true,
		Issuer:     "https://idp.example.com",
		JwksUri:    srv.URL,
		Audience:   "nexus-gateway",
		EmailClaim: "email",
		GroupClaim: "groups",
		GroupRoleMap: map[string]string{
			"admins":  "super_admin",
			"editors": "editor",
		},
	}

	cache := NewJWKSCache(srv.URL, slog.Default())

	token := buildJWT(t,
		map[string]any{"alg": "RS256", "kid": kid},
		map[string]any{
			"sub":    "user-123",
			"iss":    "https://idp.example.com",
			"aud":    "nexus-gateway",
			"exp":    float64(time.Now().Add(time.Hour).Unix()),
			"email":  "alice@example.com",
			"groups": []string{"admins", "viewers"},
		},
		priv,
	)

	claims, err := ValidateJWT(token, config, cache)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.Subject != "user-123" {
		t.Errorf("subject = %q, want %q", claims.Subject, "user-123")
	}
	if claims.Email != "alice@example.com" {
		t.Errorf("email = %q, want %q", claims.Email, "alice@example.com")
	}
	if len(claims.Groups) != 2 || claims.Groups[0] != "admins" {
		t.Errorf("groups = %v, want [admins viewers]", claims.Groups)
	}
	if claims.Issuer != "https://idp.example.com" {
		t.Errorf("issuer = %q, want %q", claims.Issuer, "https://idp.example.com")
	}
}

// TestValidateJWT_IssuerTrailingSlash asserts a token whose `iss` differs from
// the configured issuer only by a trailing slash still validates — Auth0/Okta
// advertise "https://host/" while admins routinely configure "https://host".
func TestValidateJWT_IssuerTrailingSlash(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kid := "test-kid-slash"
	srv := startJWKSServer(t, kid, &priv.PublicKey)

	config := &OidcConfig{
		Enabled:    true,
		Issuer:     "https://idp.example.com", // configured without slash
		JwksUri:    srv.URL,
		Audience:   "nexus-gateway",
		EmailClaim: "email",
	}
	cache := NewJWKSCache(srv.URL, slog.Default())

	token := buildJWT(t,
		map[string]any{"alg": "RS256", "kid": kid},
		map[string]any{
			"sub": "user-1",
			"iss": "https://idp.example.com/", // token carries the slash
			"aud": "nexus-gateway",
			"exp": float64(time.Now().Add(time.Hour).Unix()),
		},
		priv,
	)

	if _, err := ValidateJWT(token, config, cache); err != nil {
		t.Fatalf("trailing-slash issuer must validate: %v", err)
	}
}

func TestValidateJWT_Expired(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-kid-2"
	srv := startJWKSServer(t, kid, &priv.PublicKey)
	config := &OidcConfig{
		Enabled:  true,
		Issuer:   "https://idp.example.com",
		JwksUri:  srv.URL,
		Audience: "nexus-gateway",
	}
	cache := NewJWKSCache(srv.URL, slog.Default())

	token := buildJWT(t,
		map[string]any{"alg": "RS256", "kid": kid},
		map[string]any{
			"sub": "user-123",
			"iss": "https://idp.example.com",
			"aud": "nexus-gateway",
			"exp": float64(time.Now().Add(-time.Hour).Unix()),
		},
		priv,
	)

	_, err := ValidateJWT(token, config, cache)
	if err == nil {
		t.Fatal("expected error for expired JWT")
	}
}

func TestValidateJWT_WrongIssuer(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-kid-3"
	srv := startJWKSServer(t, kid, &priv.PublicKey)
	config := &OidcConfig{
		Enabled:  true,
		Issuer:   "https://idp.example.com",
		JwksUri:  srv.URL,
		Audience: "nexus-gateway",
	}
	cache := NewJWKSCache(srv.URL, slog.Default())

	token := buildJWT(t,
		map[string]any{"alg": "RS256", "kid": kid},
		map[string]any{
			"sub": "user-123",
			"iss": "https://wrong-issuer.com",
			"aud": "nexus-gateway",
			"exp": float64(time.Now().Add(time.Hour).Unix()),
		},
		priv,
	)

	_, err := ValidateJWT(token, config, cache)
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
}

func TestValidateJWT_WrongAudience(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-kid-4"
	srv := startJWKSServer(t, kid, &priv.PublicKey)
	config := &OidcConfig{
		Enabled:  true,
		Issuer:   "https://idp.example.com",
		JwksUri:  srv.URL,
		Audience: "nexus-gateway",
	}
	cache := NewJWKSCache(srv.URL, slog.Default())

	token := buildJWT(t,
		map[string]any{"alg": "RS256", "kid": kid},
		map[string]any{
			"sub": "user-123",
			"iss": "https://idp.example.com",
			"aud": "wrong-audience",
			"exp": float64(time.Now().Add(time.Hour).Unix()),
		},
		priv,
	)

	_, err := ValidateJWT(token, config, cache)
	if err == nil {
		t.Fatal("expected error for wrong audience")
	}
}

func TestValidateJWT_TamperedPayload(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-kid-5"
	srv := startJWKSServer(t, kid, &priv.PublicKey)
	config := &OidcConfig{
		Enabled:  true,
		Issuer:   "https://idp.example.com",
		JwksUri:  srv.URL,
		Audience: "nexus-gateway",
	}
	cache := NewJWKSCache(srv.URL, slog.Default())

	// Build valid token then tamper with the payload.
	token := buildJWT(t,
		map[string]any{"alg": "RS256", "kid": kid},
		map[string]any{
			"sub": "user-123",
			"iss": "https://idp.example.com",
			"aud": "nexus-gateway",
			"exp": float64(time.Now().Add(time.Hour).Unix()),
		},
		priv,
	)

	// Replace sub in the payload.
	tamperedPayload, _ := json.Marshal(map[string]any{
		"sub": "evil-user",
		"iss": "https://idp.example.com",
		"aud": "nexus-gateway",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	parts := splitJWT(token)
	parts[1] = base64.RawURLEncoding.EncodeToString(tamperedPayload)
	tampered := parts[0] + "." + parts[1] + "." + parts[2]

	_, err := ValidateJWT(tampered, config, cache)
	if err == nil {
		t.Fatal("expected error for tampered JWT")
	}
}

func TestValidateJWT_AudArray(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-kid-6"
	srv := startJWKSServer(t, kid, &priv.PublicKey)
	config := &OidcConfig{
		Enabled:  true,
		Issuer:   "https://idp.example.com",
		JwksUri:  srv.URL,
		Audience: "nexus-gateway",
	}
	cache := NewJWKSCache(srv.URL, slog.Default())

	token := buildJWT(t,
		map[string]any{"alg": "RS256", "kid": kid},
		map[string]any{
			"sub": "user-123",
			"iss": "https://idp.example.com",
			"aud": []string{"other-app", "nexus-gateway"},
			"exp": float64(time.Now().Add(time.Hour).Unix()),
		},
		priv,
	)

	claims, err := ValidateJWT(token, config, cache)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.Subject != "user-123" {
		t.Errorf("subject = %q, want %q", claims.Subject, "user-123")
	}
}

func TestParseRSAPublicKey(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub := &priv.PublicKey

	nB64 := bigIntToB64(pub.N)
	eB64 := intToB64(pub.E)

	parsed, err := parseRSAPublicKey(nB64, eB64)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.N.Cmp(pub.N) != 0 {
		t.Error("N mismatch")
	}
	if parsed.E != pub.E {
		t.Errorf("E = %d, want %d", parsed.E, pub.E)
	}
}

func TestAudienceContains(t *testing.T) {
	tests := []struct {
		aud    any
		target string
		want   bool
	}{
		{"foo", "foo", true},
		{"foo", "bar", false},
		{[]any{"a", "b"}, "b", true},
		{[]any{"a", "b"}, "c", false},
		{nil, "foo", false},
		{42, "foo", false},
	}
	for i, tt := range tests {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			got := audienceContains(tt.aud, tt.target)
			if got != tt.want {
				t.Errorf("audienceContains(%v, %q) = %v, want %v", tt.aud, tt.target, got, tt.want)
			}
		})
	}
}

func splitJWT(token string) [3]string {
	var parts [3]string
	i := 0
	for j := 0; j < len(token) && i < 3; j++ {
		if token[j] == '.' {
			i++
			continue
		}
		parts[i] += string(token[j])
	}
	return parts
}
