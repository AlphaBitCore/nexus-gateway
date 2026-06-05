// Package middleware — OIDC JWT validation with JWKS caching.
package middleware

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// OidcConfig holds the OIDC settings for one external IdP — issuer, JWKS
// endpoint, audience, and claim mappings. It is built per request from the
// IdentityProvider row (see store.DecodeOIDCConfig) and passed to ValidateJWT
// to verify an ID token.
type OidcConfig struct {
	Enabled      bool              `json:"enabled"`
	Issuer       string            `json:"issuer"`
	JwksUri      string            `json:"jwksUri"`
	ClientID     string            `json:"clientId"`
	ClientSecret string            `json:"clientSecret"`
	RedirectURI  string            `json:"redirectUri"`
	AuthorizeURL string            `json:"authorizeUrl"`
	TokenURL     string            `json:"tokenUrl"`
	Audience     string            `json:"audience"`
	EmailClaim   string            `json:"emailClaim"`
	GroupClaim   string            `json:"groupClaim"`
	GroupRoleMap map[string]string `json:"groupRoleMap"`
}

// JWTClaims holds the validated claims extracted from a JWT.
type JWTClaims struct {
	Subject string
	Email   string
	Groups  []string
	Issuer  string
	// Nonce is the raw `nonce` claim, surfaced (not validated here) so the OIDC
	// callback can bind it to the nonce it sent at SSO start.
	Nonce string
}

// JWKSCache fetches and caches RSA public keys from a JWKS endpoint.
type JWKSCache struct {
	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey // kid → public key
	fetchedAt time.Time
	ttl       time.Duration
	jwksURI   string
	client    *http.Client
	logger    *slog.Logger
}

// NewJWKSCache creates a cache that fetches keys from the given JWKS URI.
func NewJWKSCache(jwksURI string, logger *slog.Logger) *JWKSCache {
	return &JWKSCache{
		keys:    make(map[string]*rsa.PublicKey),
		ttl:     10 * time.Minute,
		jwksURI: jwksURI,
		client: nexushttp.New(nexushttp.Config{
			Timeout:        10 * time.Second,
			Caller:         "cp-jwt-middleware",
			PropagateReqID: true,
		}),
		logger: logger,
	}
}

// GetKey returns the RSA public key for the given kid, refreshing the cache
// if it is stale or the kid is not found.
func (c *JWKSCache) GetKey(kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	key, ok := c.keys[kid]
	stale := time.Since(c.fetchedAt) > c.ttl
	c.mu.RUnlock()

	if ok && !stale {
		return key, nil
	}

	// Refresh and retry.
	if err := c.refresh(); err != nil {
		return nil, fmt.Errorf("jwks refresh: %w", err)
	}

	c.mu.RLock()
	key, ok = c.keys[kid]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("kid %q not found in JWKS", kid)
	}
	return key, nil
}

// jwksResponse is the JSON shape of a JWKS endpoint response.
type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (c *JWKSCache) refresh() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.jwksURI, nil)
	if err != nil {
		return fmt.Errorf("build JWKS request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS endpoint returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB max
	if err != nil {
		return fmt.Errorf("read JWKS body: %w", err)
	}

	var jwks jwksResponse
	if err := json.Unmarshal(body, &jwks); err != nil {
		return fmt.Errorf("parse JWKS: %w", err)
	}

	newKeys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := parseRSAPublicKey(k.N, k.E)
		if err != nil {
			c.logger.Warn("skipping JWKS key", "kid", k.Kid, "error", err)
			continue
		}
		newKeys[k.Kid] = pub
	}

	c.mu.Lock()
	c.keys = newKeys
	c.fetchedAt = time.Now()
	c.mu.Unlock()

	c.logger.Debug("refreshed JWKS cache", "keys", len(newKeys), "uri", c.jwksURI)
	return nil
}

// parseRSAPublicKey builds an *rsa.PublicKey from base64url-encoded n and e
// values as found in a JWK.
func parseRSAPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() {
		return nil, fmt.Errorf("exponent too large")
	}

	return &rsa.PublicKey{
		N: n,
		E: int(e.Int64()),
	}, nil
}

// ValidateJWT parses and validates a JWT string against the given OIDC config
// and JWKS cache. It verifies the RS256 signature, expiration, issuer, and
// audience, then extracts email and group claims.
func ValidateJWT(tokenString string, config *OidcConfig, cache *JWKSCache) (*JWTClaims, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT: expected 3 parts, got %d", len(parts))
	}

	// Decode header.
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode JWT header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("parse JWT header: %w", err)
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("unsupported JWT algorithm: %s", header.Alg)
	}

	// Fetch public key by kid.
	pubKey, err := cache.GetKey(header.Kid)
	if err != nil {
		return nil, fmt.Errorf("get signing key: %w", err)
	}

	// Verify signature: SHA-256 hash of "header.payload", verified with RSA PKCS1v15.
	sigInput := parts[0] + "." + parts[1]
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode JWT signature: %w", err)
	}
	hashed := sha256.Sum256([]byte(sigInput))
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hashed[:], sigBytes); err != nil {
		return nil, fmt.Errorf("JWT signature verification failed: %w", err)
	}

	// Decode payload.
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT payload: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("parse JWT payload: %w", err)
	}

	// Check expiration.
	expVal, ok := payload["exp"]
	if !ok {
		return nil, fmt.Errorf("JWT missing exp claim")
	}
	expFloat, ok := expVal.(float64)
	if !ok {
		return nil, fmt.Errorf("JWT exp claim is not a number")
	}
	if time.Now().Unix() > int64(expFloat) {
		return nil, fmt.Errorf("JWT expired")
	}

	// Check issuer. Compare with a trailing slash trimmed from both sides:
	// many IdPs (Auth0, Okta) advertise an issuer ending in "/" in the token's
	// `iss`, while admins routinely enter it without — a same-host difference
	// that should not fail validation.
	iss, _ := payload["iss"].(string)
	if strings.TrimRight(iss, "/") != strings.TrimRight(config.Issuer, "/") {
		return nil, fmt.Errorf("JWT issuer mismatch: got %q, want %q", iss, config.Issuer)
	}

	// Check audience — aud can be a string or array of strings.
	if !audienceContains(payload["aud"], config.Audience) {
		return nil, fmt.Errorf("JWT audience mismatch: want %q", config.Audience)
	}

	// Extract subject.
	sub, _ := payload["sub"].(string)
	if sub == "" {
		return nil, fmt.Errorf("JWT missing sub claim")
	}

	// Extract email.
	emailClaim := config.EmailClaim
	if emailClaim == "" {
		emailClaim = "email"
	}
	email, _ := payload[emailClaim].(string)

	// Extract groups.
	groupClaim := config.GroupClaim
	if groupClaim == "" {
		groupClaim = "groups"
	}
	var groups []string
	if rawGroups, ok := payload[groupClaim]; ok {
		switch g := rawGroups.(type) {
		case []any:
			for _, v := range g {
				if s, ok := v.(string); ok {
					groups = append(groups, s)
				}
			}
		case string:
			groups = []string{g}
		}
	}

	nonce, _ := payload["nonce"].(string)

	return &JWTClaims{
		Subject: sub,
		Email:   email,
		Groups:  groups,
		Issuer:  iss,
		Nonce:   nonce,
	}, nil
}

// audienceContains checks whether the target audience is present in the aud
// claim, which may be a string or an array of strings per the JWT spec.
func audienceContains(aud any, target string) bool {
	// An empty expected audience can never be satisfied — without a known
	// audience (client_id) there is nothing to bind the token to, so fail
	// closed rather than accept any (or an empty) aud.
	if target == "" {
		return false
	}
	switch v := aud.(type) {
	case string:
		return v == target
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == target {
				return true
			}
		}
	}
	return false
}
