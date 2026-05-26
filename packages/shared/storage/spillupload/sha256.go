package spillupload

import "crypto/sha256"

// sha256OfString returns the SHA-256 of s as a 32-byte slice. Used by
// DedupKey; kept private so callers do not accidentally use it for
// anything cryptographically meaningful (HMAC is the right primitive
// for that).
func sha256OfString(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
