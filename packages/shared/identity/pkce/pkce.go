// Package pkce implements the PKCE (RFC 7636) S256 code-challenge
// transformation shared across the Control Plane authorization server,
// the agent SSO enrollment endpoint, and the agent-side enrollment
// flow. The SHA-256 → base64url-no-pad → constant-time-compare sequence
// is consolidated here into a single audited implementation.
package pkce

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
)

// VerifierMinLen / VerifierMaxLen are the bounds RFC 7636 §4.1 imposes
// on the code_verifier: 43–128 base64url-safe characters.
const (
	VerifierMinLen = 43
	VerifierMaxLen = 128
)

// randReader is the entropy source used by Generate. It is a package-level
// variable solely so tests can substitute a failing reader and exercise the
// entropy-error branch; production code never reassigns it. Default:
// crypto/rand.Reader.
var randReader io.Reader = rand.Reader

// Generate creates a fresh (verifier, S256 challenge) pair using 64
// bytes of CSPRNG entropy, yielding an 86-character base64url-no-pad
// verifier (within the RFC's 43–128 range).
func Generate() (verifier, challenge string, err error) {
	buf := make([]byte, 64)
	if _, err := io.ReadFull(randReader, buf); err != nil {
		return "", "", fmt.Errorf("pkce: read entropy: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	challenge = ChallengeS256(verifier)
	return verifier, challenge, nil
}

// ChallengeS256 computes BASE64URL(SHA256(verifier)) with no padding,
// per RFC 7636 §4.2 ("S256" code_challenge_method).
func ChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// VerifyS256 returns true when `verifier` matches the previously-stored
// S256 `challenge`. The comparison is constant-time to avoid leaking
// timing information about partial matches.
//
// The verifier itself must satisfy RFC 7636's length bounds; verifiers
// outside [43,128] are rejected unconditionally so an attacker cannot
// stuff in a degenerate value that hashes to a known challenge.
func VerifyS256(verifier, challenge string) bool {
	if len(verifier) < VerifierMinLen || len(verifier) > VerifierMaxLen {
		return false
	}
	computed := ChallengeS256(verifier)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}
