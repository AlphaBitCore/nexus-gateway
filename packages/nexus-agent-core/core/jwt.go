package core

import (
	"encoding/base64"
	"fmt"
	"github.com/goccy/go-json"
	"strings"
	"time"
)

// jwtExpiry parses the `exp` claim from a JWT WITHOUT verifying its signature.
// The toolkit never trusts the token's contents — the gateway verifies it on
// every call. We read `exp` only to decide when to refresh proactively, so an
// unverified read is correct and avoids shipping the auth-server's JWKS.
func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("not a JWT (want 3 segments, got %d)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("parse JWT claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, fmt.Errorf("JWT has no exp claim")
	}
	return time.Unix(claims.Exp, 0), nil
}
