package agentca

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

const DeviceTokenLen = 32

// DeviceTokenTTL is the lifetime of a device bearer token from the moment it is
// issued (at enrollment) or rotated (POST /api/internal/things/renew-token).
// Bounded so a stolen plaintext token — read from the agent's on-disk
// `device-token` file — is replayable for at most this window rather than
// forever. The agent renews well before expiry (see
// DeviceTokenRenewWindow on the agent side), so a healthy device never lets its
// token lapse; an attacker who exfiltrates a token and goes quiet loses it when
// the legitimate agent's next rotation overwrites the stored hash.
//
// 30 days balances replay-window minimisation against renewal churn: the token
// rotates roughly monthly, and the agent's multi-day renewal window leaves ample
// retries before a transient Hub outage could let it expire.
const DeviceTokenTTL = 30 * 24 * time.Hour

// DeviceTokenExpiry returns the absolute expiry for a token issued at `now`.
// Callers stamp the result onto thing.device_token_expires_at.
func DeviceTokenExpiry(now time.Time) time.Time {
	return now.Add(DeviceTokenTTL)
}

// tokenRandReader is the entropy source used by GenerateDeviceToken. It is a
// package-level variable solely so tests can substitute a failing reader and
// exercise the entropy-error branch; production code never reassigns it.
// Matches the same seam pattern used by packages/shared/identity/pkce.
var tokenRandReader io.Reader = rand.Reader

// GenerateDeviceToken creates a cryptographically random device token.
// Returns (plaintext hex, SHA-256 hash hex, error).
func GenerateDeviceToken() (plaintext string, hashed string, err error) {
	buf := make([]byte, DeviceTokenLen)
	if _, err = io.ReadFull(tokenRandReader, buf); err != nil {
		return "", "", fmt.Errorf("generate device token: %w", err)
	}
	plaintext = hex.EncodeToString(buf)
	sum := sha256.Sum256(buf)
	hashed = hex.EncodeToString(sum[:])
	return plaintext, hashed, nil
}

// HashDeviceToken computes the SHA-256 hash of a plaintext device token.
func HashDeviceToken(plaintext string) (string, error) {
	buf, err := hex.DecodeString(plaintext)
	if err != nil {
		return "", fmt.Errorf("hash device token: invalid hex: %w", err)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}
