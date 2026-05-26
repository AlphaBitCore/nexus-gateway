package agentca

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

const DeviceTokenLen = 32

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
